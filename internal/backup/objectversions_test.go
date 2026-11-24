package backup

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"dockback/internal/crypto"
	"dockback/internal/storage"
	"dockback/internal/store"
)

const versionedBucket = "backups"

// objectVersion is one stored version of a key: its bytes, or a delete marker.
type objectVersion struct {
	id          string
	data        []byte
	marker      bool
	retainUntil time.Time
}

// versionedS3 is a bucket with versioning and Object Lock, as S3 has them: a
// delete by key only adds a marker, a delete by version removes that version
// unless it is still retained, and a read by version finds it behind a marker.
type versionedS3 struct {
	*httptest.Server
	mu       sync.Mutex
	next     int
	versions map[string][]objectVersion
	uploads  map[string][]byte
	retain   map[string]time.Time
}

func newVersionedS3(t *testing.T) *versionedS3 {
	t.Helper()
	f := &versionedS3{versions: map[string][]objectVersion{}, uploads: map[string][]byte{}, retain: map[string]time.Time{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func s3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
}

func retainUntilOf(r *http.Request) time.Time {
	until, _ := time.Parse(time.RFC3339, r.Header.Get("X-Amz-Object-Lock-Retain-Until-Date"))
	return until
}

func (f *versionedS3) store(key string, data []byte, until time.Time) string {
	f.next++
	id := "v" + strconv.Itoa(f.next)
	f.versions[key] = append(f.versions[key], objectVersion{id: id, data: data, retainUntil: until})
	return id
}

func (f *versionedS3) find(key, versionID string) (objectVersion, bool) {
	all := f.versions[key]
	if versionID == "" && len(all) > 0 {
		return all[len(all)-1], true
	}
	for _, v := range all {
		if v.id == versionID {
			return v, true
		}
	}
	return objectVersion{}, false
}

func (f *versionedS3) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.TrimPrefix(r.URL.Path, "/"+versionedBucket+"/")
	q := r.URL.Query()
	body, _ := io.ReadAll(r.Body)
	if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		body = unchunk(body)
	}
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		upload := "up-" + strconv.Itoa(len(f.uploads)+1)
		f.uploads[upload] = nil
		f.retain[upload] = retainUntilOf(r)
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, versionedBucket, key, upload)
	case r.Method == http.MethodPut && q.Has("partNumber"):
		upload := q.Get("uploadId")
		f.uploads[upload] = append(f.uploads[upload], body...)
		w.Header().Set("ETag", `"part"`)
	case r.Method == http.MethodPost && q.Has("uploadId"):
		upload := q.Get("uploadId")
		id := f.store(key, f.uploads[upload], f.retain[upload])
		w.Header().Set("x-amz-version-id", id)
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><CompleteMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><ETag>"done"</ETag></CompleteMultipartUploadResult>`, versionedBucket, key)
	case r.Method == http.MethodPut:
		w.Header().Set("x-amz-version-id", f.store(key, body, retainUntilOf(r)))
		w.Header().Set("ETag", `"done"`)
	case r.Method == http.MethodDelete && q.Get("versionId") != "":
		v, ok := f.find(key, q.Get("versionId"))
		if ok && time.Now().Before(v.retainUntil) {
			s3Error(w, http.StatusForbidden, "AccessDenied")
			return
		}
		kept := f.versions[key][:0]
		for _, other := range f.versions[key] {
			if other.id != q.Get("versionId") {
				kept = append(kept, other)
			}
		}
		f.versions[key] = kept
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete:
		f.next++
		f.versions[key] = append(f.versions[key], objectVersion{id: "m" + strconv.Itoa(f.next), marker: true})
		w.Header().Set("x-amz-delete-marker", "true")
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		v, ok := f.find(key, q.Get("versionId"))
		if !ok || v.marker {
			s3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(v.data)))
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("ETag", `"done"`)
		w.Header().Set("x-amz-version-id", v.id)
		if r.Method == http.MethodGet {
			_, _ = w.Write(v.data)
		}
	default:
		w.WriteHeader(http.StatusOK)
	}
}

// unchunk strips the aws-chunked framing a signed streaming upload wraps its
// payload in: "<hex size>[;chunk-signature=…]\r\n<bytes>\r\n", ending at size 0.
func unchunk(framed []byte) []byte {
	var payload []byte
	for len(framed) > 0 {
		header, rest, found := strings.Cut(string(framed), "\r\n")
		size, err := strconv.ParseInt(strings.SplitN(header, ";", 2)[0], 16, 64)
		if !found || err != nil || size == 0 || int64(len(rest)) < size {
			break
		}
		payload = append(payload, rest[:size]...)
		framed = []byte(strings.TrimPrefix(rest[size:], "\r\n"))
	}
	return payload
}

// liveData reports the bytes the bucket still stores for key, markers aside.
func (f *versionedS3) liveData(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, v := range f.versions[key] {
		if !v.marker {
			n++
		}
	}
	return n
}

// versionedEngine has one enabled S3 destination on the fake bucket, with
// Object Lock for lockDays (0 = none).
func versionedEngine(t *testing.T, lockDays int) (*Engine, *versionedS3, storage.Backend) {
	t.Helper()
	bucket := newVersionedS3(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	local, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	endpoint, _ := url.Parse(bucket.URL)
	cfg := map[string]string{"endpoint": endpoint.Host, "bucket": versionedBucket, "access_key": "test", "secret_key": "test-secret", "insecure": "true", "region": "us-east-1"}
	if lockDays > 0 {
		cfg["object_lock"] = "governance"
		cfg["lock_days"] = strconv.Itoa(lockDays)
	}
	js, _ := json.Marshal(cfg)
	enc, err := crypto.SealString(string(js), key)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateDestination(&store.Destination{ID: "d-s3", Name: "d-s3", Type: "s3", Enabled: true, ConfigEnc: enc}); err != nil {
		t.Fatal(err)
	}
	backend, err := storage.NewFromConfig("s3", cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{Store: st, Storage: local, Key: key, Log: func(string, string, string) {}}, bucket, backend
}

const versionedArchiveKey = "node1/web/2026-01-01_00-00-00_abc.dback"

// remoteOnlyBackup is a backup whose only copy is the one putArchive wrote to the
// fake bucket, recorded the way the mirror records it.
func remoteOnlyBackup(t *testing.T, e *Engine, backend storage.Backend, archive string) *store.Backup {
	t.Helper()
	versionID, err := putArchive(context.Background(), backend, versionedArchiveKey, strings.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if versionID == "" {
		t.Fatal("a versioned bucket's upload must record the version it was given")
	}
	b := &store.Backup{ID: "bk1", NodeID: "node1", TargetName: "web", Status: "success", StorageKey: versionedArchiveKey}
	b.LocationsJSON = mustJSON([]Location{{Kind: "dest", DestID: "d-s3", Name: "d-s3", Type: "s3", VersionID: versionID}})
	return b
}

// Whoever holds the destination's delete permission can hide a locked archive
// behind a delete marker — Object Lock protects a version, not the key. The
// restore must still read the version this backup wrote.
func TestARestoreReadsTheRecordedVersionBehindADeleteMarker(t *testing.T) {
	e, _, backend := versionedEngine(t, 30)
	b := remoteOnlyBackup(t, e, backend, "archive-bytes")
	if err := backend.Delete(context.Background(), versionedArchiveKey); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Get(context.Background(), versionedArchiveKey); err == nil {
		t.Fatal("the fixture's delete marker must hide the key")
	}

	rc, err := e.openVerified(context.Background(), b, "d-s3")
	if err != nil {
		t.Fatalf("the retained version must stay readable behind a delete marker: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "archive-bytes" {
		t.Errorf("read %q, want the bytes this backup wrote", got)
	}
}

// A newer object at the same key must not be read in place of the backup's own.
func TestARestoreIgnoresANewerObjectAtTheSameKey(t *testing.T) {
	e, _, backend := versionedEngine(t, 0)
	b := remoteOnlyBackup(t, e, backend, "archive-bytes")
	if _, err := backend.Put(context.Background(), versionedArchiveKey, strings.NewReader("someone-else")); err != nil {
		t.Fatal(err)
	}

	rc, err := e.openVerified(context.Background(), b, "d-s3")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "archive-bytes" {
		t.Errorf("read %q, want the bytes this backup wrote", got)
	}
}

// Deleting by key on a versioned bucket frees nothing. A recorded version is
// deleted itself, so the bytes really go, and only then is the copy "removed".
func TestDeletingARecordedVersionFreesItsBytes(t *testing.T) {
	e, bucket, backend := versionedEngine(t, 0)
	b := remoteOnlyBackup(t, e, backend, "archive-bytes")

	deletions, _ := e.deleteCopies(context.Background(), b)

	if len(deletions) != 1 || deletions[0].Outcome != CopyRemoved || !Reclaimed(deletions) {
		t.Fatalf("want the copy removed and reclaimed, got %+v", deletions)
	}
	if n := bucket.liveData(versionedArchiveKey); n != 0 {
		t.Errorf("the bucket still stores %d version(s) of the archive", n)
	}
}

// A provider that refuses to delete a retained version is a failed delete, not a
// freed copy — whatever DockBack's own record of the lock said.
func TestARetainedVersionIsNotCountedAsFreed(t *testing.T) {
	e, bucket, backend := versionedEngine(t, 30)
	b := remoteOnlyBackup(t, e, backend, "archive-bytes")

	deletions, _ := e.deleteCopies(context.Background(), b)

	if len(deletions) != 1 || deletions[0].Outcome != CopyFailed || Reclaimed(deletions) {
		t.Fatalf("a refused version delete must be reported as failed, got %+v", deletions)
	}
	if n := bucket.liveData(versionedArchiveKey); n != 1 {
		t.Errorf("the retained version must still be stored, found %d", n)
	}
}

// A copy written before versions were recorded is still deleted by key, but on
// a versioned bucket that leaves its bytes behind, so it is not called freed.
func TestALegacyCopyIsDeletedWithoutClaimingTheBytes(t *testing.T) {
	e, bucket, backend := versionedEngine(t, 0)
	b := remoteOnlyBackup(t, e, backend, "archive-bytes")
	b.LocationsJSON = mustJSON([]Location{{Kind: "dest", DestID: "d-s3", Name: "d-s3", Type: "s3"}})

	rc, err := e.openVerified(context.Background(), b, "d-s3")
	if err != nil {
		t.Fatalf("a legacy copy must stay readable by key: %v", err)
	}
	rc.Close()
	deletions, _ := e.deleteCopies(context.Background(), b)

	if len(deletions) != 1 || deletions[0].Outcome != CopyUnconfirmed || Reclaimed(deletions) {
		t.Fatalf("want an unconfirmed delete, got %+v", deletions)
	}
	if n := bucket.liveData(versionedArchiveKey); n != 1 {
		t.Errorf("the fixture's versioned bucket keeps the bytes behind the marker, found %d", n)
	}
}

func TestOnlyConfirmedRemovalsAreReclaimed(t *testing.T) {
	removed := CopyDeletion{Location: "local", Outcome: CopyRemoved}
	for _, other := range []string{CopyUnconfirmed, CopyLocked, CopyFailed} {
		if Reclaimed([]CopyDeletion{removed, {Location: "offsite", Outcome: other}}) {
			t.Errorf("a %s copy must keep the backup from counting as freed", other)
		}
	}
	if !Reclaimed([]CopyDeletion{removed}) {
		t.Error("every copy removed is freed")
	}
}

// The mirror records the version the bucket gave the archive, so every later
// read and delete of this copy can name it.
func TestAMirroredCopyRecordsItsVersion(t *testing.T) {
	e, _, _ := versionedEngine(t, 0)
	ctx := context.Background()
	if _, err := e.Storage.Put(ctx, versionedArchiveKey, strings.NewReader("archive-bytes")); err != nil {
		t.Fatal(err)
	}
	man, _ := json.Marshal(Manifest{BackupID: "bk1", TargetName: "web"})
	b := &store.Backup{ID: "bk1", NodeID: "node1", TargetName: "web", Status: "success", StorageKey: versionedArchiveKey, ManifestJSON: string(man)}
	if err := e.Store.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}

	if err := e.MirrorExisting(ctx, b.ID, []string{"d-s3"}, true); err != nil {
		t.Fatal(err)
	}

	stored, err := e.Store.GetBackup(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	var offsite *Location
	for _, loc := range e.locations(stored) {
		if loc.Kind == "dest" {
			offsite = &loc
		}
	}
	if offsite == nil || offsite.VersionID == "" || offsite.Unversioned {
		t.Fatalf("the mirrored copy must record the version it was written as: %+v", e.locations(stored))
	}
}
