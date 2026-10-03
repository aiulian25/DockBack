package api

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// Perf Fix 1: eligible GET responses gzip; small/streaming/skip-listed ones
// pass through byte-identical.

func gzGet(t *testing.T, h http.Handler, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestGzipMiddleware(t *testing.T) {
	big := strings.Repeat("{\"k\":\"vvvvvvvvvvvvvvvv\"},", 200) // ~4.4 KB, compressible
	mux := http.NewServeMux()
	mux.HandleFunc("/big.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(big))
	})
	mux.HandleFunc("/small.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(bytes.Repeat([]byte{0x42}, 5000))
	})
	mux.HandleFunc("/api/logs/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: hello\n\n")
		f.Flush()
		_, _ = io.WriteString(w, "data: "+big+"\n\n")
	})
	mux.HandleFunc("/share/runbook/tok", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<html>"+big+"</html>")
	})
	h := gzipMiddleware(mux)

	// Large JSON compresses, shrinks, and round-trips exactly.
	rec := gzGet(t, h, "/big.json", nil)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("large JSON must be gzipped, headers=%v", rec.Header())
	}
	if got := rec.Header().Values("Vary"); len(got) == 0 || !strings.Contains(strings.Join(got, ","), "Accept-Encoding") {
		t.Fatal("Vary: Accept-Encoding must be set")
	}
	if rec.Body.Len() >= len(big) {
		t.Fatalf("compressed body (%d) not smaller than original (%d)", rec.Body.Len(), len(big))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	if string(plain) != big {
		t.Fatal("gzip round-trip mismatch")
	}

	// Below the threshold: untouched.
	rec = gzGet(t, h, "/small.json", nil)
	if rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("small body must pass through raw: %v %q", rec.Header(), rec.Body.String())
	}

	// Non-compressible type: untouched even when large.
	rec = gzGet(t, h, "/bin", nil)
	if rec.Header().Get("Content-Encoding") != "" || rec.Body.Len() != 5000 {
		t.Fatal("binary must pass through raw")
	}

	// SSE skip-listed: streams raw even with a large frame after a Flush.
	rec = gzGet(t, h, "/api/logs/stream", nil)
	if rec.Header().Get("Content-Encoding") != "" || !strings.HasPrefix(rec.Body.String(), "data: hello") {
		t.Fatal("SSE must never be compressed")
	}

	// Public share endpoint skip-listed (BREACH surface).
	rec = gzGet(t, h, "/share/runbook/tok", nil)
	if rec.Header().Get("Content-Encoding") != "" {
		t.Fatal("public share responses must never be compressed")
	}

	// No Accept-Encoding: raw.
	req := httptest.NewRequest(http.MethodGet, "/big.json", nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Header().Get("Content-Encoding") != "" || rec2.Body.String() != big {
		t.Fatal("client without gzip support must get identity bytes")
	}

	// A handler that flushes mid-stream before the threshold stays raw.
	mux.HandleFunc("/flushy", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "early")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, big)
	})
	rec = gzGet(t, h, "/flushy", nil)
	if rec.Header().Get("Content-Encoding") != "" || !strings.HasPrefix(rec.Body.String(), "early") {
		t.Fatal("a flushed stream must stay uncompressed")
	}
}

func TestSpaHandlerCaching(t *testing.T) {
	ui := fstest.MapFS{
		"index.html":          {Data: []byte("<html>app</html>")},
		"assets/index-abc.js": {Data: []byte("console.log('x')")},
	}
	h := spaHandler(ui)

	// Hashed assets: immutable for a year.
	rec := gzGet(t, h, "/assets/index-abc.js", nil)
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Fatalf("asset Cache-Control = %q", cc)
	}

	// index.html: no-cache + ETag, and If-None-Match answers 304 with no body.
	rec = gzGet(t, h, "/", nil)
	etag := rec.Header().Get("ETag")
	if rec.Header().Get("Cache-Control") != "no-cache" || etag == "" {
		t.Fatalf("index must be no-cache with an ETag: %v", rec.Header())
	}
	if rec.Body.String() != "<html>app</html>" {
		t.Fatal("index body wrong")
	}
	rec = gzGet(t, h, "/", map[string]string{"If-None-Match": etag})
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Fatalf("matching ETag must 304 with empty body, got %d (%d bytes)", rec.Code, rec.Body.Len())
	}
	// SPA fallback (deep link) serves index with the same policy.
	rec = gzGet(t, h, "/servers/n1/containers/abc", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") != etag || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("SPA fallback must serve validated index: %d %v", rec.Code, rec.Header())
	}
}
