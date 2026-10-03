package storage

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dockback/internal/egress"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3 is an S3-compatible backup destination (AWS S3, Backblaze B2, MinIO,
// Wasabi, etc.) via minio-go — pure HTTP(S), works in the hardened container
// (PLAN §4.9). Multiple S3 destinations are supported (each is its own record).
type S3 struct {
	client *minio.Client
	bucket string
	prefix string
	name   string

	// Object Lock (WORM, PLAN §9.1): when set, every uploaded object carries a
	// retention period the bucket enforces — the object cannot be deleted or
	// overwritten (even by these credentials) until it expires. Requires a bucket
	// created with Object Lock enabled; pair with append-only credentials.
	lockMode minio.RetentionMode // "" = disabled | GOVERNANCE | COMPLIANCE
	lockDays int

	// idleTimeout overrides s3IdleTimeout. Zero means the constant; only tests
	// set it, so an inactivity test need not run for ten minutes.
	idleTimeout time.Duration
}

// NewS3 builds an S3 backend from a config map.
//
//	endpoint:    s3.amazonaws.com | s3.us-west-002.backblazeb2.com | minio:9000
//	region:      e.g. us-east-1 (optional)
//	bucket:      target bucket (required)
//	access_key / secret_key: credentials (required)
//	path:        key prefix inside the bucket (optional)
//	insecure:    "true" to use HTTP instead of HTTPS (optional)
func NewS3(cfg map[string]string) (*S3, error) {
	endpoint := strings.TrimSpace(cfg["endpoint"])
	bucket := strings.TrimSpace(cfg["bucket"])
	if endpoint == "" || bucket == "" {
		return nil, fmt.Errorf("s3: endpoint and bucket are required")
	}
	endpoint = strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	secure := !strings.EqualFold(cfg["insecure"], "true")

	// Guard the dialer so a redirect/rebind to a non-allowed host is refused at
	// the TCP layer (PLAN §3.10 default-deny). Clone the default transport so
	// minio keeps its sane HTTP/2, keep-alive and timeout defaults.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = egress.Default().GuardDial(tr.DialContext)

	cl, err := minio.New(endpoint, &minio.Options{
		Creds:     credentials.NewStaticV4(cfg["access_key"], cfg["secret_key"], ""),
		Secure:    secure,
		Region:    cfg["region"],
		Transport: tr,
	})
	if err != nil {
		return nil, fmt.Errorf("s3: %w", err)
	}
	lockMode, lockDays := parseObjectLock(cfg)
	return &S3{
		client:   cl,
		bucket:   bucket,
		prefix:   strings.Trim(strings.ReplaceAll(cfg["path"], "\\", "/"), "/"),
		name:     fmt.Sprintf("s3://%s/%s", endpoint, bucket),
		lockMode: lockMode,
		lockDays: lockDays,
	}, nil
}

// parseObjectLock reads the optional Object Lock (WORM) config. Returns ("", 0)
// when disabled. "compliance" (no one can delete before expiry) or "governance"
// (privileged users can); requires a positive retention period in days.
func parseObjectLock(cfg map[string]string) (minio.RetentionMode, int) {
	days, _ := strconv.Atoi(strings.TrimSpace(cfg["lock_days"]))
	if days <= 0 {
		return "", 0
	}
	switch strings.ToLower(strings.TrimSpace(cfg["object_lock"])) {
	case "compliance":
		return minio.Compliance, days
	case "governance":
		return minio.Governance, days
	default:
		return "", 0
	}
}

// Immutable reports whether this destination writes WORM-locked objects (PLAN §9.1).
func (s *S3) Immutable() bool { return s.lockMode != "" && s.lockDays > 0 }

// LockDays is the configured retention period in days (0 when not immutable).
func (s *S3) LockDays() int {
	if !s.Immutable() {
		return 0
	}
	return s.lockDays
}

// VerifyObjectLock performs a NON-DESTRUCTIVE preflight (PLAN §9.1): it asks the
// bucket whether Object Lock is enabled/enforcing, so an operator who marked a
// destination immutable learns immediately if the bucket wouldn't actually make
// backups WORM (the single biggest ransomware-protection footgun). It writes
// nothing. Append-only keys may lack s3:GetBucketObjectLockConfiguration — that's
// reported as "couldn't verify" (Checked=false), not a false "not enabled",
// because WORM still applies at upload time when the bucket was created for it.
func (s *S3) VerifyObjectLock(ctx context.Context) ObjectLockStatus {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	lock, mode, validity, unit, err := s.client.GetObjectLockConfig(ctx, s.bucket)
	return objectLockStatus(lock, mode, validity, unit, err)
}

// objectLockStatus turns a GetObjectLockConfig result into a verdict. Pure, so
// the three branches (enabled / not-enabled / couldn't-check) are unit-testable
// without a live bucket.
//
// Note: S3/MinIO report a bucket WITHOUT Object Lock as an *error*
// ("ObjectLockConfigurationNotFoundError"), not a clean empty config — so that
// specific code is the loud "not enabled" case, while other errors (AccessDenied
// for an append-only key, network, etc.) mean "couldn't verify".
func objectLockStatus(lock string, mode *minio.RetentionMode, validity *uint, unit *minio.ValidityUnit, err error) ObjectLockStatus {
	if err != nil {
		if isObjectLockNotConfigured(err) {
			return ObjectLockStatus{
				Checked: true,
				Detail:  "This bucket does NOT have Object Lock enabled — uploaded backups will NOT be immutable. Object Lock can only be enabled when a bucket is created; recreate the bucket with Object Lock on.",
			}
		}
		return ObjectLockStatus{
			Checked: false,
			Detail:  "Could not read the bucket's Object Lock configuration (" + err.Error() + "). Append-only keys often can't read it — that's fine; WORM still applies on upload if the bucket was created with Object Lock enabled.",
		}
	}
	if !strings.EqualFold(lock, "Enabled") {
		return ObjectLockStatus{
			Checked: true,
			Detail:  "This bucket does NOT have Object Lock enabled — uploaded backups will NOT be immutable. Object Lock can only be enabled when a bucket is created; recreate the bucket with Object Lock on.",
		}
	}
	detail := "Object Lock is enabled on this bucket — uploaded backups are immutable (WORM)."
	if mode != nil && validity != nil && unit != nil {
		detail += fmt.Sprintf(" Bucket default retention: %s %d %s.", *mode, *validity, strings.ToLower(string(*unit)))
	}
	return ObjectLockStatus{Enforced: true, Checked: true, Detail: detail}
}

// isObjectLockNotConfigured reports whether the error means the bucket simply has
// no Object Lock configuration (i.e. it is definitively NOT immutable), as
// opposed to a permission/network error. AWS + MinIO use the S3 error code
// "ObjectLockConfigurationNotFoundError"; a message fallback covers variants.
func isObjectLockNotConfigured(err error) bool {
	if minio.ToErrorResponse(err).Code == "ObjectLockConfigurationNotFoundError" {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "object lock configuration does not exist")
}

func (s *S3) objKey(key string) string {
	k := strings.Trim(strings.ReplaceAll(key, "\\", "/"), "/")
	if s.prefix != "" {
		return s.prefix + "/" + k
	}
	return k
}

// Upload and control-call bounds (PLAN §4.5/§4.9).
const (
	// s3MetadataTimeout bounds the small control calls — a stat, a delete, a
	// listing, a reachability probe. Each is a single round trip that answers in
	// milliseconds, so anything past this is an endpoint that has stopped
	// answering rather than one working slowly.
	s3MetadataTimeout = 5 * time.Minute

	// s3IdleTimeout bounds INACTIVITY during an upload, never its total
	// duration. It replaces a fixed 2-hour cap that failed any archive larger
	// than roughly (link rate × 2h) — the biggest and most valuable backups,
	// on every run, while the network was working perfectly. A 4-day initial
	// seed over a home uplink is a legitimate upload and must be allowed to
	// finish; a target that has gone silent must still fail promptly. Same
	// posture as the SMB backend's smbIdleTimeout, and the same window as the
	// mirror's own stall guard one layer up.
	s3IdleTimeout = 10 * time.Minute

	// s3IdleCheckInterval is how often the watchdog looks. Cheap, and it bounds
	// the overshoot past s3IdleTimeout to one interval.
	s3IdleCheckInterval = 30 * time.Second
)

// withTimeout bounds a METADATA call. Uploads do not use it: see Put.
func (s *S3) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s3MetadataTimeout)
}

// idleTimeout is the upload inactivity window, overridable in tests.
func (s *S3) idle() time.Duration {
	if s.idleTimeout > 0 {
		return s.idleTimeout
	}
	return s3IdleTimeout
}

// uploadActivity is the upload's "still moving" clock, touched from BOTH
// directions of the transfer.
//
// Both are needed. minio reads a whole part into memory before sending any of
// it (for an unknown-size object the part is ~537 MiB), so while that part goes
// out on the wire the source reader is untouched for minutes, and while the
// source is being drained nothing is on the wire. A watchdog watching either
// signal alone would abort a perfectly healthy transfer on a slow link — the
// exact failure this replaces.
type uploadActivity struct {
	mu   sync.Mutex
	last time.Time
}

func newUploadActivity() *uploadActivity { return &uploadActivity{last: time.Now()} }

func (a *uploadActivity) touch() {
	a.mu.Lock()
	a.last = time.Now()
	a.mu.Unlock()
}

func (a *uploadActivity) idleFor() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return time.Since(a.last)
}

// Read makes this usable as minio's progress hook: minio "reads" it with the
// byte count that just reached the network, which is the wire-side signal.
func (a *uploadActivity) Read(p []byte) (int, error) {
	a.touch()
	return len(p), nil
}

// activityReader records every non-empty read from the upload source — the
// other half of the signal.
type activityReader struct {
	r        io.Reader
	activity *uploadActivity
}

func (a *activityReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		a.activity.touch()
	}
	return n, err
}

// watchUploadIdle cancels the upload once it has gone `idle` with no progress in
// either direction, recording that it was the watchdog that did so. The returned
// function ends the watch and must be called when the upload returns.
func watchUploadIdle(activity *uploadActivity, idle time.Duration, cancel context.CancelFunc, stalled *atomic.Bool) func() {
	done := make(chan struct{})
	// Look often enough that the overshoot past `idle` stays small even when the
	// window itself is short.
	every := s3IdleCheckInterval
	if half := idle / 2; half < every {
		every = half
	}
	if every <= 0 {
		every = time.Millisecond
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if activity.idleFor() < idle {
					continue
				}
				stalled.Store(true)
				cancel()
				return
			}
		}
	}()
	return sync.OnceFunc(func() { close(done) })
}

// Put uploads an object (streaming, unknown size).
//
// Bounded by inactivity rather than by elapsed time, so a large archive on a
// throttled link finishes as long as bytes keep moving.
func (s *S3) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	if err := validateKey(key); err != nil {
		return 0, err
	}
	uploadCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	activity := newUploadActivity()
	var stalled atomic.Bool
	stopWatch := watchUploadIdle(activity, s.idle(), cancel, &stalled)
	defer stopWatch()

	opts := minio.PutObjectOptions{ContentType: "application/octet-stream", Progress: activity}
	if s.Immutable() {
		// WORM: lock the object so it can't be deleted/overwritten until expiry
		// (PLAN §9.1). The bucket must have Object Lock enabled.
		opts.Mode = s.lockMode
		opts.RetainUntilDate = time.Now().UTC().Add(time.Duration(s.lockDays) * 24 * time.Hour)
	}
	info, err := s.client.PutObject(uploadCtx, s.bucket, s.objKey(key), &activityReader{r: r, activity: activity}, -1, opts)
	if err != nil {
		// Say which clock stopped it. Cancellation from the watchdog would
		// otherwise read as "context canceled", indistinguishable from the
		// operator stopping the run.
		if stalled.Load() && ctx.Err() == nil {
			return 0, fmt.Errorf("s3 put %q: no data moved for %s — the destination stopped responding", s.objKey(key), s.idle())
		}
		return 0, fmt.Errorf("s3 put %q: %w", s.objKey(key), err)
	}
	return info.Size, nil
}

// Get opens an object for reading.
//
// validateKey here for the same reason every other method has it: the
// confinement to this backend's prefix is a property of the INTERFACE, and one
// method without it is a path the rest cannot rely on. No absolute deadline —
// Get streams a whole archive to a restore, and that is what step 8 removed.
func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, s.objKey(key), minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	// Force an early stat so a missing object errors here, not mid-read.
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		return nil, err
	}
	return obj, nil
}

// Delete removes an object (idempotent).
func (s *S3) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	return s.client.RemoveObject(ctx, s.bucket, s.objKey(key), minio.RemoveObjectOptions{})
}

// s3NotFound reports whether err means the object is definitively absent, as
// opposed to unreachable. A HEAD on a missing key answers "NoSuchKey" on AWS and
// "NotFound" on the HEAD-only path; anything else (403 from an expired key, a
// 5xx, a dropped connection) is a question we could not answer.
func s3NotFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "NoSuchKey" || resp.Code == "NotFound" || resp.StatusCode == http.StatusNotFound
}

// Stat reports size + existence. See Backend.Stat for the error contract.
func (s *S3) Stat(ctx context.Context, key string) (int64, bool, error) {
	if err := validateKey(key); err != nil {
		return 0, false, err
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	info, err := s.client.StatObject(ctx, s.bucket, s.objKey(key), minio.StatObjectOptions{})
	if err != nil {
		if s3NotFound(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("s3 stat %q: %w", s.objKey(key), err)
	}
	return info.Size, true, nil
}

// FreeBytes is not a concept for object storage.
func (s *S3) FreeBytes(_ context.Context) (uint64, error) { return 0, nil }

// Ping is a cheap reachability check — a HEAD on the bucket. Writes nothing.
func (s *S3) Ping(ctx context.Context) error {
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	ok, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("s3: bucket %q not found", s.bucket)
	}
	return nil
}

// List returns object keys under prefix (recursive), as logical keys (the prefix
// configured on this backend stripped back off, so Get/Delete accept them).
func (s *S3) List(ctx context.Context, prefix string) ([]string, error) {
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	full := s.objKey(prefix)
	out := []string{}
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: full, Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		k := obj.Key
		if s.prefix != "" {
			k = strings.TrimPrefix(k, s.prefix+"/")
		}
		out = append(out, k)
	}
	return out, nil
}

// Name identifies the backend.
func (s *S3) Name() string { return s.name }
