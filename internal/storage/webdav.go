package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	neturl "net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"dockback/internal/egress"

	"github.com/studio-b12/gowebdav"
)

// Chunked-upload tuning for Nextcloud/ownCloud large files (PLAN §4.5). Files
// over chunkThreshold are uploaded in chunkSize pieces via the dav /uploads/
// endpoint so a single huge PUT can't be rejected by a size-limited reverse
// proxy (HTTP 413). 32 MiB chunks stay well under typical Nextcloud/nginx
// limits while keeping the request count modest.
const (
	chunkSize        = 32 << 20 // 32 MiB per chunk
	chunkThreshold   = 64 << 20 // only chunk files larger than this
	maxChunkAttempts = 5        // per-request retries on transient upload failures
	// uploadConcurrency is how many chunks upload in parallel. A single TCP
	// stream can't fill a fast link through a proxy (latency×window limit), so we
	// run several — this scales to saturate gigabit while staying harmless on slow
	// links (the streams just share the pipe). ~4×chunkSize peak memory.
	uploadConcurrency = 4
)

// WebDAV is a WebDAV backup destination (Nextcloud, or Synology over WebDAV) —
// pure HTTP(S), so it works in the hardened container (PLAN §4.9).
type WebDAV struct {
	client *gowebdav.Client
	base   string // display + PROPFIND base URL
	dir    string // base directory on the server

	// For the RFC 4331 quota PROPFIND + chunked uploads (gowebdav doesn't expose
	// raw requests).
	httpc      *http.Client
	authHeader string

	// Nextcloud/ownCloud chunked-upload endpoints (empty for plain WebDAV /
	// Synology, which fall back to a single PUT). Derived from the configured URL.
	ncUploads    string // .../remote.php/dav/uploads/<user>
	ncFiles      string // .../remote.php/dav/files/<user>  (no trailing slash)
	ncServerInfo string // .../ocs/v2.php/apps/serverinfo/api/v1/info — real disk
	//                     free space (what Homepage shows) when the quota is
	//                     unlimited (-3). Needs an admin account or serverinfo token.

	// Short-lived quota cache so the paired FreeBytes/TotalBytes/UsedBytes calls
	// in the list handler share a single PROPFIND round-trip. avail/used are
	// tracked separately because Nextcloud reports used even when the quota is
	// unlimited (available = -3) — we can still show usage.
	qmu          sync.Mutex
	qfree, qused uint64
	qavailOK     bool // a real available/free figure was returned (quota is set)
	qusedOK      bool // a used figure was returned (true even for unlimited quota)
	qat          time.Time
}

// NewWebDAV builds a WebDAV backend from a config map.
//
// For Nextcloud the URL is typically:
//
//	https://HOST/remote.php/dav/files/USERNAME/
func NewWebDAV(cfg map[string]string) (*WebDAV, error) {
	url := strings.TrimSpace(cfg["url"])
	if url == "" {
		return nil, fmt.Errorf("webdav: url is required")
	}
	c := gowebdav.NewClient(url, cfg["username"], cfg["password"])
	c.SetTimeout(60 * time.Second)
	// Reuse connections (keep-alive) so each request doesn't pay a fresh TCP+TLS
	// handshake. Shared between gowebdav and our raw quota/chunk client.
	// HTTP/2 is deliberately DISABLED: large request bodies (chunk PUTs) through
	// nginx/Cloudflare in front of Nextcloud are a common source of spurious 502s
	// over h2; HTTP/1.1 is more robust for big uploads (PLAN §4.5).
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           egress.Default().GuardDial(nil), // PLAN §3.10 default-deny (catches redirects)
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{}, // disable h2
	}
	c.SetTransport(tr)
	// Send Basic credentials preemptively. gowebdav's default "auto" auth probes
	// with an unauthenticated request first (a wasted 401 round-trip on every new
	// client); setting the header up front skips that. Nextcloud accepts Basic
	// auth with an app password directly.
	authHeader := ""
	if u := cfg["username"]; u != "" {
		authHeader = "Basic " + base64.StdEncoding.EncodeToString([]byte(u+":"+cfg["password"]))
		c.SetHeader("Authorization", authHeader)
	}
	uploads, files, serverinfo, _ := nextcloudEndpoints(url)
	return &WebDAV{
		client: c,
		base:   url,
		dir:    strings.Trim(strings.ReplaceAll(cfg["path"], "\\", "/"), "/"),
		// No fixed per-request cap — assembling a large chunked upload (MOVE) can
		// take minutes server-side; requests are bounded by their context instead.
		httpc:        &http.Client{Transport: tr},
		authHeader:   authHeader,
		ncUploads:    uploads,
		ncFiles:      files,
		ncServerInfo: serverinfo,
	}, nil
}

// nextcloudEndpoints derives the chunked-upload (/uploads/<user>) and files
// (/files/<user>) dav roots from a Nextcloud/ownCloud URL of the form
// scheme://host[/subpath]/remote.php/dav/files/<user>/... — returning ok=false
// for any other WebDAV server (which then uses plain single-PUT uploads).
func nextcloudEndpoints(rawURL string) (uploads, files, ocs string, ok bool) {
	u, err := neturl.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "", "", "", false
	}
	const marker = "/remote.php/dav/files/"
	i := strings.Index(u.Path, marker)
	if i < 0 {
		return "", "", "", false
	}
	rest := strings.Trim(u.Path[i+len(marker):], "/")
	user := rest
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		user = rest[:j]
	}
	if user == "" {
		return "", "", "", false
	}
	prefix := u.Scheme + "://" + u.Host + u.Path[:i] // preserves any subpath install
	return prefix + "/remote.php/dav/uploads/" + user,
		prefix + "/remote.php/dav/files/" + user,
		prefix + "/ocs/v2.php/apps/serverinfo/api/v1/info",
		true
}

func (w *WebDAV) full(key string) string {
	p := strings.Trim(strings.ReplaceAll(key, "\\", "/"), "/")
	if w.dir != "" {
		p = w.dir + "/" + p
	}
	return "/" + p
}

// Put streams an object. WriteStream/WriteStreamWithLength already create parent
// collections, so we don't MkdirAll separately (that was a duplicate round-trip
// on every upload). Seekable inputs (local files, byte buffers) are streamed
// with a known Content-Length so gowebdav doesn't buffer the whole object into
// memory just to measure it.
func (w *WebDAV) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	if err := validateKey(key); err != nil {
		return 0, err
	}
	// Size if the reader is seekable (it is for our local archive / file copies).
	size := int64(-1)
	if s, ok := r.(io.Seeker); ok {
		if n, err := s.Seek(0, io.SeekEnd); err == nil {
			if _, err := s.Seek(0, io.SeekStart); err == nil {
				size = n
			}
		}
	}
	// Nextcloud/ownCloud: upload large files in chunks so a single multi-GB PUT
	// can't be rejected by a size-limited proxy (HTTP 413) — PLAN §4.5. Small
	// files and non-Nextcloud servers keep the simple single PUT.
	if w.ncUploads != "" && (size < 0 || size > chunkThreshold) {
		return w.putChunked(ctx, key, r, size)
	}
	return runCtx(ctx, func() (int64, error) { return w.put(key, r) })
}

// putChunked uploads via the Nextcloud/ownCloud chunked-upload v2 protocol:
// MKCOL an upload session, PUT fixed-size chunks (named by byte offset so the
// server assembles them in order), then MOVE the assembled .file to the final
// destination. The session is deleted on any failure so no partial junk is left
// on the remote. After assembly the remote size is verified (PLAN §4.5).
func (w *WebDAV) putChunked(ctx context.Context, key string, r io.Reader, size int64) (int64, error) {
	idb := make([]byte, 8)
	_, _ = rand.Read(idb)
	session := w.ncUploads + "/dback-" + hex.EncodeToString(idb)
	dest := w.ncFiles + w.full(key)

	// Create the upload session (create the parent /uploads/<user> first if needed).
	if code, err := w.rawDoRetried(ctx, "MKCOL", session, nil, nil); err != nil {
		return 0, err
	} else if code == http.StatusConflict {
		_, _ = w.rawDoRetried(ctx, "MKCOL", w.ncUploads, nil, nil)
		if code, err = w.rawDoRetried(ctx, "MKCOL", session, nil, nil); err != nil {
			return 0, err
		} else if !ok2xx(code) && code != http.StatusMethodNotAllowed {
			return 0, fmt.Errorf("webdav: create upload session failed (%d)", code)
		}
	} else if !ok2xx(code) && code != http.StatusMethodNotAllowed {
		return 0, fmt.Errorf("webdav: create upload session failed (%d)", code)
	}
	// From here on, clean up the session on any error path.
	fail := func(format string, a ...any) (int64, error) {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, _ = w.rawDo(cctx, "DELETE", session, nil, nil)
		cancel()
		return 0, fmt.Errorf(format, a...)
	}

	// Ensure the final destination's parent collection exists for the MOVE.
	if parent := path.Dir(w.full(key)); parent != "" && parent != "/" {
		if err := w.client.MkdirAll(parent, 0o755); err != nil {
			return fail("webdav: mkdir %q: %w", parent, err)
		}
	}

	// Upload chunks in parallel (bounded) so throughput isn't capped by a single
	// stream. The read loop is paced by the semaphore so it never races far ahead
	// of in-flight uploads; the first failure cancels the rest.
	uctx, ucancel := context.WithCancel(ctx)
	defer ucancel()
	sem := make(chan struct{}, uploadConcurrency)
	var wg sync.WaitGroup
	var uerrMu sync.Mutex
	var uerr error
	setErr := func(e error) {
		uerrMu.Lock()
		if uerr == nil {
			uerr = e
		}
		uerrMu.Unlock()
		ucancel()
	}

	// One 32 MiB buffer per chunk, reused. A multi-gigabyte upload otherwise
	// allocated a fresh one for every chunk — hundreds of 32 MiB allocations,
	// each a garbage-collection event, inside a container limited to 1 GiB. The
	// pool bounds what is live to roughly the upload concurrency.
	//
	// The buffer is returned by the goroutine that used it, never by this loop:
	// rawDoRetried may still be reading from it when the loop moves on.
	bufPool := sync.Pool{New: func() any { b := make([]byte, chunkSize); return &b }}

	var offset int64
readLoop:
	for {
		bufp := bufPool.Get().(*[]byte)
		buf := *bufp
		n, rerr := io.ReadFull(r, buf)
		if n > 0 {
			select {
			case sem <- struct{}{}:
			case <-uctx.Done():
				bufPool.Put(bufp)
				break readLoop
			}
			wg.Add(1)
			go func(b []byte, bp *[]byte, off int64) {
				defer wg.Done()
				defer func() { <-sem }()
				defer bufPool.Put(bp) // only once this chunk's retries are finished
				code, err := w.rawDoRetried(uctx, http.MethodPut, fmt.Sprintf("%s/%016d", session, off), b, nil)
				if err != nil {
					setErr(fmt.Errorf("webdav: chunk upload: %w", err))
				} else if !ok2xx(code) {
					setErr(fmt.Errorf("webdav: chunk upload rejected (%d) after %d attempts", code, maxChunkAttempts))
				}
			}(buf[:n], bufp, offset)
			offset += int64(n)
		} else {
			bufPool.Put(bufp) // nothing was read into it
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			setErr(fmt.Errorf("webdav: reading source: %w", rerr))
			break
		}
	}
	wg.Wait()
	if uerr != nil {
		return fail("%w", uerr)
	}
	total := offset

	// Assemble: MOVE the virtual .file to the destination. Assembling a large file
	// is slow (tens of seconds for many GB) and ONE-SHOT — a blind retry hits the
	// already-consumed upload session and 404s even though the file landed (a
	// false failure). So we do a single MOVE with a generous timeout, then treat
	// the DESTINATION FILE itself as the source of truth: if it exists at the
	// expected size it succeeded, regardless of what the MOVE response said or a
	// proxy timeout in between (PLAN §4.5 — verify remote object after upload).
	mctx, mcancel := context.WithTimeout(ctx, 30*time.Minute)
	code, merr := w.rawDo(mctx, "MOVE", session+"/.file", nil, map[string]string{
		"Destination":     dest,
		"OC-Total-Length": strconv.FormatInt(total, 10),
		"Overwrite":       "T",
	})
	mcancel()
	if w.confirmSize(ctx, key, total) {
		return total, nil
	}
	return fail("webdav: assemble failed — file not present at %d bytes after MOVE (code=%d, err=%v)", total, code, merr)
}

func ok2xx(code int) bool { return code >= 200 && code < 300 }

// confirmSize polls the destination until it appears at exactly want bytes,
// allowing the server time to finish assembling a large chunked upload. This is
// the authoritative success check — immune to proxy timeouts or a false MOVE
// error after the assemble already completed.
func (w *WebDAV) confirmSize(ctx context.Context, key string, want int64) bool {
	for i := 0; i < 30; i++ { // up to ~10 min of server-side assembly
		if fi, err := w.client.Stat(w.full(key)); err == nil && fi.Size() == want {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(20 * time.Second):
		}
	}
	return false
}

// isTransientCode reports whether an HTTP status is worth retrying (gateway /
// overload errors typical of a proxy in front of Nextcloud, e.g. the 502 a
// chunk PUT can hit). Permanent statuses (2xx/4xx like 413/409) are not retried.
func isTransientCode(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout,
		http.StatusLocked: // 423: Nextcloud file lock during assembly — usually clears shortly
		return true
	}
	return false
}

// rawDoRetried wraps rawDo with bounded exponential back-off, retrying on
// network errors and transient HTTP statuses so a single proxy hiccup mid-upload
// doesn't abandon the whole transfer (PLAN §4.5). Honors ctx cancellation.
func (w *WebDAV) rawDoRetried(ctx context.Context, method, url string, body []byte, headers map[string]string) (int, error) {
	backoff := 500 * time.Millisecond
	var code int
	var err error
	for attempt := 1; ; attempt++ {
		code, err = w.rawDo(ctx, method, url, body, headers)
		if (err == nil && !isTransientCode(code)) || attempt >= maxChunkAttempts {
			return code, err
		}
		select {
		case <-ctx.Done():
			return code, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

// rawDo issues an authenticated WebDAV request (MKCOL/PUT/MOVE/DELETE) the
// gowebdav client doesn't expose, returning the status code.
func (w *WebDAV) rawDo(ctx context.Context, method, url string, body []byte, headers map[string]string) (int, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, err
	}
	if w.authHeader != "" {
		req.Header.Set("Authorization", w.authHeader)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := w.httpc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, nil
}

func (w *WebDAV) put(key string, r io.Reader) (int64, error) {
	full := w.full(key)
	if seeker, ok := r.(io.Seeker); ok {
		if n, err := seeker.Seek(0, io.SeekEnd); err == nil {
			if _, err := seeker.Seek(0, io.SeekStart); err == nil {
				if err := w.client.WriteStreamWithLength(full, r, n, 0o644); err != nil {
					return 0, fmt.Errorf("webdav write %q: %w", full, err)
				}
				return n, nil
			}
		}
	}
	cw := &countWriter{}
	if err := w.client.WriteStream(full, io.TeeReader(r, cw), 0o644); err != nil {
		return 0, fmt.Errorf("webdav write %q: %w", full, err)
	}
	return cw.n, nil
}

// Get opens an object.
func (w *WebDAV) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	return runCtx(ctx, func() (io.ReadCloser, error) { return w.client.ReadStream(w.full(key)) })
}

// Delete removes an object (idempotent). Never targets the folder root.
func (w *WebDAV) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	_, err := runCtx(ctx, func() (struct{}, error) {
		e := w.client.Remove(w.full(key))
		if e != nil && strings.Contains(strings.ToLower(e.Error()), "not found") {
			return struct{}{}, nil
		}
		return struct{}{}, e
	})
	return err
}

// Stat reports size + existence.
func (w *WebDAV) Stat(ctx context.Context, key string) (int64, bool, error) {
	if err := validateKey(key); err != nil {
		return 0, false, err
	}
	type sr struct {
		size   int64
		exists bool
	}
	r, err := runCtx(ctx, func() (sr, error) {
		fi, serr := w.client.Stat(w.full(key))
		if serr != nil {
			// Only a 404 means the file is not there. A 401 from a rotated
			// password, a 403, a 502 from the reverse proxy in front of
			// Nextcloud — all previously read as "absent". See Backend.Stat.
			if gowebdav.IsErrNotFound(serr) || errors.Is(serr, fs.ErrNotExist) {
				return sr{}, nil
			}
			return sr{}, fmt.Errorf("webdav stat %q: %w", key, serr)
		}
		return sr{fi.Size(), true}, nil
	})
	return r.size, r.exists, err
}

// Ping is a cheap reachability check (authenticated PROPFIND on the root via
// gowebdav's Connect) — writes nothing, so it's safe for periodic health polls.
func (w *WebDAV) Ping(ctx context.Context) error {
	_, err := runCtx(ctx, func() (struct{}, error) { return struct{}{}, w.client.Connect() })
	return err
}

// FreeBytes / TotalBytes / UsedBytes report the account's quota when the server
// exposes it (RFC 4331 — Nextcloud/ownCloud do). With a real quota you get a full
// free/total picture; with an UNLIMITED quota (available = -3) free/total are
// unknown (0) but UsedBytes still reports actual usage so the UI shows "X used".
func (w *WebDAV) FreeBytes(ctx context.Context) (uint64, error) {
	w.refreshQuota(ctx)
	w.qmu.Lock()
	defer w.qmu.Unlock()
	if w.qavailOK {
		return w.qfree, nil
	}
	return 0, nil
}

func (w *WebDAV) TotalBytes(ctx context.Context) (uint64, error) {
	w.refreshQuota(ctx)
	w.qmu.Lock()
	defer w.qmu.Unlock()
	if w.qavailOK {
		return w.qfree + w.qused, nil
	}
	return 0, nil
}

// UsedBytes reports bytes used (valid even when the quota is unlimited).
func (w *WebDAV) UsedBytes(ctx context.Context) (uint64, error) {
	w.refreshQuota(ctx)
	w.qmu.Lock()
	defer w.qmu.Unlock()
	if w.qusedOK {
		return w.qused, nil
	}
	return 0, nil
}

// quotaPropfind requests just the two RFC 4331 quota properties.
const quotaPropfind = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:"><d:prop><d:quota-available-bytes/><d:quota-used-bytes/></d:prop></d:propfind>`

// refreshQuota fetches available/used via a Depth:0 PROPFIND on the base
// directory, memoised for ~10s so the paired Free/Total/Used calls don't repeat
// the round-trip. quota-available-bytes may be negative (e.g. -3 = unlimited) →
// available is left unknown, but used is still recorded.
func (w *WebDAV) refreshQuota(ctx context.Context) {
	w.qmu.Lock()
	defer w.qmu.Unlock()
	if !w.qat.IsZero() && time.Since(w.qat) < 10*time.Second {
		return
	}
	w.qat = time.Now()
	w.qfree, w.qused, w.qavailOK, w.qusedOK = 0, 0, false, false

	u := strings.TrimRight(w.base, "/")
	if w.dir != "" {
		u += "/" + w.dir
	}
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", u+"/", strings.NewReader(quotaPropfind))
	if err != nil {
		return
	}
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	if w.authHeader != "" {
		req.Header.Set("Authorization", w.authHeader)
	}
	resp, err := w.httpc.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		return
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var ms struct {
		Responses []struct {
			Propstat []struct {
				Available string `xml:"prop>quota-available-bytes"`
				Used      string `xml:"prop>quota-used-bytes"`
			} `xml:"propstat"`
		} `xml:"response"`
	}
	if xml.Unmarshal(body, &ms) != nil {
		return
	}
	for _, r := range ms.Responses {
		for _, ps := range r.Propstat {
			if us, err := strconv.ParseInt(strings.TrimSpace(ps.Used), 10, 64); err == nil && us >= 0 {
				w.qused, w.qusedOK = uint64(us), true
			}
			// Negative (e.g. -3) means unlimited → leave available unknown.
			if av, err := strconv.ParseInt(strings.TrimSpace(ps.Available), 10, 64); err == nil && av >= 0 {
				w.qfree, w.qavailOK = uint64(av), true
			}
		}
	}
	// WebDAV quota is unlimited (-3) → the real disk free isn't in the PROPFIND.
	// Fall back to Nextcloud's serverinfo API for the data-dir free space (the
	// same figure dashboards like Homepage show), so capacity reports like any
	// other target. Needs an admin account or a serverinfo token; otherwise we
	// keep the used-only result.
	if !w.qavailOK && w.ncServerInfo != "" {
		if free, ok := w.serverinfoFreespace(ctx); ok {
			w.qfree, w.qavailOK = free, true
		}
	}
}

// serverinfoFreespace queries Nextcloud's serverinfo app for the data
// directory's free space. Returns ok=false if the app/endpoint is unavailable or
// the caller lacks permission (non-admin).
func (w *WebDAV) serverinfoFreespace(ctx context.Context) (free uint64, ok bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.ncServerInfo+"?format=json", nil)
	if err != nil {
		return 0, false
	}
	req.Header.Set("OCS-APIRequest", "true")
	req.Header.Set("Accept", "application/json")
	if w.authHeader != "" {
		req.Header.Set("Authorization", w.authHeader)
	}
	resp, err := w.httpc.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var p struct {
		Ocs struct {
			Data struct {
				Nextcloud struct {
					System struct {
						Freespace float64 `json:"freespace"`
					} `json:"system"`
				} `json:"nextcloud"`
			} `json:"data"`
		} `json:"ocs"`
	}
	if json.Unmarshal(body, &p) != nil {
		return 0, false
	}
	if fs := p.Ocs.Data.Nextcloud.System.Freespace; fs > 0 {
		return uint64(fs), true
	}
	return 0, false
}

// List returns object keys directly under prefix (non-recursive, files only).
func (w *WebDAV) List(ctx context.Context, prefix string) ([]string, error) {
	return runCtx(ctx, func() ([]string, error) {
		// Walk RECURSIVELY (breadth-first) so nested archive layouts are enumerated,
		// matching the S3/local backends — a uniform "every object under prefix"
		// contract (F20 adopt). A flat prefix still returns just its files.
		out := []string{}
		queue := []string{strings.Trim(strings.ReplaceAll(prefix, "\\", "/"), "/")}
		for len(queue) > 0 {
			rel := queue[0]
			queue = queue[1:]
			fis, err := w.client.ReadDir(w.full(rel))
			if err != nil {
				continue // missing/inaccessible dir → skip (empty for the start prefix)
			}
			for _, fi := range fis {
				child := fi.Name()
				if rel != "" {
					child = rel + "/" + fi.Name()
				}
				if fi.IsDir() {
					queue = append(queue, child)
					continue
				}
				out = append(out, child)
			}
		}
		return out, nil
	})
}

// Connect verifies the connection (used by Test Connection).
func (w *WebDAV) Connect() error { return w.client.Connect() }

// Name identifies the backend.
func (w *WebDAV) Name() string { return "webdav:" + w.base }

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }
