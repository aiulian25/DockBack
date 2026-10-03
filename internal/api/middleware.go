package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/gzip"

	"dockback/internal/config"
	"dockback/internal/storage"
	"dockback/internal/version"
)

// mustStorage builds the local backup destination, panicking on failure (called
// once at startup). v1 ships the local mounted-volume backend (PLAN §4.9).
func mustStorage(cfg *config.Config) storage.Backend {
	b, err := storage.NewLocal(cfg.BackupsDir)
	if err != nil {
		panic("init storage: " + err.Error())
	}
	return b
}

// writeJSON writes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(jsonEmptyNotNull(v))
}

// jsonEmptyNotNull turns a nil slice or map into an empty one, so a list
// endpoint answers `[]` and never `null` (F179).
//
// Go marshals a nil slice as `null`, which is a real difference to a caller: a
// container with no listable mounts answered `null`, and the browser called
// .filter on it and took the whole page down with "can't access property
// filter, c is null". Nothing about that response was an error — the honest
// answer to "which mounts does this container have" was "none".
//
// Fixed at the boundary rather than at the call site, because the call site is
// wherever the NEXT empty list happens to be. Every endpoint that returns a
// slice has always had this waiting in it; the four stacks that hit it just
// happened to contain a service with nothing to mount.
//
// Only the top-level value: a nil slice inside a struct is a field the client
// already reads defensively, and rewriting arbitrary nested values on the way
// out is a much bigger promise than this needs to make.
func jsonEmptyNotNull(v any) any {
	if v == nil {
		return v
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice:
		if rv.IsNil() {
			return reflect.MakeSlice(rv.Type(), 0, 0).Interface()
		}
	case reflect.Map:
		if rv.IsNil() {
			return reflect.MakeMap(rv.Type()).Interface()
		}
	}
	return v
}

// errJSON writes a JSON error envelope.
func errJSON(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// readJSON decodes a JSON request body (capped to guard against abuse).
func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// mustJSON marshals v to a string, returning "{}" on error.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// securityHeaders applies hardening headers to every response (PLAN §2/§8.5,
// §10.1). HSTS is sent only over real https; Cache-Control: no-store is scoped to
// the API (so fingerprinted SPA assets stay cacheable).
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy",
			"accelerometer=(), autoplay=(), camera=(), display-capture=(), "+
				"encrypted-media=(), fullscreen=(), geolocation=(), gyroscope=(), "+
				"magnetometer=(), microphone=(), midi=(), payment=(), usb=()")
		// style-src is locked to 'self' — no 'unsafe-inline', so an injected
		// <style> block or external stylesheet (CSS-injection / attribute-selector
		// data exfiltration) is blocked. The bundled Vite stylesheet is a <link>
		// (covered by 'self'); we ship no inline <style> elements, so no nonce is
		// needed. The only inline styles are a handful of dynamic bar dimensions
		// (progress/usage widths, chart heights) set via the style= *attribute*,
		// which a nonce cannot authorize anyway — those are scoped to
		// style-src-attr 'unsafe-inline', a far smaller surface than a blanket
		// style-src 'unsafe-inline'. scripts are external/bundled (script-src
		// inherits default-src 'self', no inline allowed).
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; "+
				"style-src 'self'; style-src-attr 'unsafe-inline'; "+
				"font-src 'self' data:; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		// HSTS only over real TLS (built-in HTTPS, or behind a trusted https
		// proxy) — never pin a plain-HTTP host.
		if r.TLS != nil || (s.trustForwarded(r) && r.Header.Get("X-Forwarded-Proto") == "https") {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		// Never cache API/auth responses (they carry account + infrastructure
		// state). Static SPA assets are fingerprinted and remain cacheable.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// recoverer turns panics into 500s instead of crashing the server.
func recovererHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic: %v", rec)
				errJSON(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler { return recovererHandler(next) }

// spaHandler serves embedded static files, falling back to index.html so the
// React single-page app handles client-side routing.
//
// Caching policy (perf Fix 1): everything under assets/ is content-hashed by
// Vite (JS, CSS, fonts), so it is served immutable for a year — a repeat visit
// re-downloads nothing. index.html must NEVER be cached long-term (it names the
// current hashes; a stale copy after an upgrade would pin dead assets), so it
// gets no-cache + a build-version ETag answered manually with 304s — embed.FS
// has no modtime, so http.ServeContent's validators can't work here.
func spaHandler(uiFS fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(uiFS))
	idx, _ := fs.ReadFile(uiFS, "index.html")
	// Content-derived validator: index.html names the current asset hashes, so
	// hashing it guarantees a new build always misses the cache — even for an
	// unstamped ("dev") binary where version.Version alone wouldn't change.
	sum := sha256.Sum256(idx)
	etag := fmt.Sprintf("%q", version.Version+"-"+hex.EncodeToString(sum[:8]))
	serveIndex := func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-cache")
		h.Set("ETag", etag)
		if inm := r.Header.Get("If-None-Match"); inm != "" && strings.Contains(inm, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		h.Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(idx)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" || p == "index.html" {
			serveIndex(w, r)
			return
		}
		if _, err := fs.Stat(uiFS, p); err != nil {
			// Unknown path that isn't an API call -> serve the SPA shell.
			serveIndex(w, r)
			return
		}
		if strings.HasPrefix(p, "assets/") {
			// Content-hashed filenames: safe to cache forever.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		fileServer.ServeHTTP(w, r)
	})
}

// --- Response compression (perf Fix 1) ---

// gzipPool recycles gzip writers. Level 5 on klauspost/compress runs close to
// stdlib BestSpeed while compressing markedly better (~452 KB vs ~485 KB for
// the main JS chunk) — a few ms once per cold client; repeats are 304s thanks
// to the cache headers above.
var gzipPool = sync.Pool{New: func() any {
	zw, _ := gzip.NewWriterLevel(io.Discard, 5)
	return zw
}}

// gzipMinSize is the smallest payload worth compressing — below ~one MTU the
// header overhead eats the benefit.
const gzipMinSize = 1400

// gzipSkipPath lists responses that must never be compressed:
//   - the SSE log stream (a compressor buffers, breaking live flushing),
//   - archive/tool downloads (already-compressed bytes; length must be exact),
//   - the PUBLIC share endpoint (unauthenticated and attacker-fetchable —
//     compressing secret-bearing output an attacker can partially influence
//     invites BREACH-class length side channels; authenticated same-origin API
//     GETs don't have that exposure).
func gzipSkipPath(p string) bool {
	return p == "/api/logs/stream" ||
		strings.HasPrefix(p, "/share/") ||
		strings.HasSuffix(p, "/download") ||
		strings.HasSuffix(p, "/extract")
}

// gzipCompressibleType reports whether a response Content-Type benefits from
// compression (text-ish only — images/fonts/archives are already compressed).
func gzipCompressibleType(ct string) bool {
	switch {
	case strings.HasPrefix(ct, "text/"),
		strings.HasPrefix(ct, "application/json"),
		strings.HasPrefix(ct, "application/javascript"),
		strings.HasPrefix(ct, "image/svg+xml"):
		return true
	}
	return false
}

// gzipResponseWriter defers the compress/plain decision until enough body bytes
// (or the end of the response) are seen, so small payloads pass through
// untouched and Content-Length stays correct for them.
type gzipResponseWriter struct {
	http.ResponseWriter
	status  int    // deferred status (0 = none written yet)
	buf     []byte // body seen before the decision
	decided bool
	gz      *gzip.Writer // non-nil once compressing
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	if g.decided {
		g.ResponseWriter.WriteHeader(code)
		return
	}
	g.status = code // defer: the compress decision must set headers first
}

// decide commits to compressing (or not) and flushes deferred state.
func (g *gzipResponseWriter) decide(compress bool) {
	if g.decided {
		return
	}
	g.decided = true
	h := g.Header()
	if compress && h.Get("Content-Encoding") == "" && gzipCompressibleType(h.Get("Content-Type")) {
		h.Del("Content-Length") // no longer valid for the compressed stream
		h.Set("Content-Encoding", "gzip")
		zw := gzipPool.Get().(*gzip.Writer)
		zw.Reset(g.ResponseWriter)
		g.gz = zw
	}
	if g.status != 0 {
		g.ResponseWriter.WriteHeader(g.status)
	}
	if len(g.buf) > 0 {
		if g.gz != nil {
			_, _ = g.gz.Write(g.buf)
		} else {
			_, _ = g.ResponseWriter.Write(g.buf)
		}
		g.buf = nil
	}
}

func (g *gzipResponseWriter) Write(p []byte) (int, error) {
	if !g.decided {
		g.buf = append(g.buf, p...)
		if len(g.buf) >= gzipMinSize {
			g.decide(true)
		}
		return len(p), nil
	}
	if g.gz != nil {
		return g.gz.Write(p)
	}
	return g.ResponseWriter.Write(p)
}

// Flush makes the wrapper a transparent http.Flusher: a streaming response that
// flushes before the size threshold commits to the UNCOMPRESSED path (a gzip
// writer would buffer away the point of flushing).
func (g *gzipResponseWriter) Flush() {
	g.decide(false)
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// close finishes the response: an undecided small body goes out raw; a
// compressed stream is flushed and its writer recycled.
func (g *gzipResponseWriter) close() {
	g.decide(false)
	if g.gz != nil {
		_ = g.gz.Close()
		g.gz.Reset(io.Discard)
		gzipPool.Put(g.gz)
		g.gz = nil
	}
}

// gzipMiddleware compresses eligible GET responses (text/JS/CSS/JSON/SVG,
// >= gzipMinSize) for clients that accept gzip. GET-only is deliberate: it
// captures essentially all the win (assets + list/aggregate JSON) while
// excluding every response that echoes request-influenced secrets (login's
// {csrf}, etc.) from compression — see the BREACH note on gzipSkipPath.
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Cache correctness for mixed clients, set even on skipped paths.
		w.Header().Add("Vary", "Accept-Encoding")
		if r.Method != http.MethodGet ||
			!strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") ||
			r.Header.Get("Range") != "" || // don't break byte-range semantics
			gzipSkipPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

// --- Prometheus-style metrics (PLAN §9.12) ---

type metricsState struct {
	backupsTotal  atomic.Int64
	backupsFailed atomic.Int64
	lastSuccessTS atomic.Int64
}

func (m *metricsState) recordBackup(ok bool) {
	m.backupsTotal.Add(1)
	if ok {
		m.lastSuccessTS.Store(time.Now().Unix())
	} else {
		m.backupsFailed.Add(1)
	}
}
