package storage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestNextcloudEndpoints(t *testing.T) {
	cases := []struct {
		url                 string
		ok                  bool
		uploads, files, ocs string
	}{
		{
			url:     "https://cloud.example.com/remote.php/dav/files/alice/",
			ok:      true,
			uploads: "https://cloud.example.com/remote.php/dav/uploads/alice",
			files:   "https://cloud.example.com/remote.php/dav/files/alice",
			ocs:     "https://cloud.example.com/ocs/v2.php/apps/serverinfo/api/v1/info",
		},
		{ // sub-path install + a path after the user
			url:     "https://h.example/nc/remote.php/dav/files/bob/Backups/",
			ok:      true,
			uploads: "https://h.example/nc/remote.php/dav/uploads/bob",
			files:   "https://h.example/nc/remote.php/dav/files/bob",
			ocs:     "https://h.example/nc/ocs/v2.php/apps/serverinfo/api/v1/info",
		},
		{url: "https://nas.local/webdav/", ok: false},       // Synology / plain WebDAV
		{url: "https://h/remote.php/dav/files/", ok: false}, // no user
		{url: "://bad", ok: false},                          // unparseable
	}
	for _, c := range cases {
		u, f, ocs, ok := nextcloudEndpoints(c.url)
		if ok != c.ok {
			t.Fatalf("%s: ok=%v want %v", c.url, ok, c.ok)
		}
		if ok && (u != c.uploads || f != c.files || ocs != c.ocs) {
			t.Fatalf("%s:\n got uploads=%q files=%q ocs=%q\nwant uploads=%q files=%q ocs=%q", c.url, u, f, ocs, c.uploads, c.files, c.ocs)
		}
	}
}

func TestRawDoRetried(t *testing.T) {
	// Transient 502 twice, then success — must retry and succeed.
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	wd := &WebDAV{httpc: srv.Client()}
	code, err := wd.rawDoRetried(context.Background(), http.MethodPut, srv.URL+"/c", []byte("x"), nil)
	if err != nil || code != http.StatusCreated {
		t.Fatalf("transient: code=%d err=%v", code, err)
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Fatalf("transient: want 3 attempts, got %d", n)
	}

	// Permanent 413 — must NOT retry (chunking decision is the caller's; a size
	// rejection won't change on retry).
	var calls2 int32
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls2, 1)
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	}))
	defer srv2.Close()
	wd2 := &WebDAV{httpc: srv2.Client()}
	code, _ = wd2.rawDoRetried(context.Background(), http.MethodPut, srv2.URL, nil, nil)
	if code != http.StatusRequestEntityTooLarge || atomic.LoadInt32(&calls2) != 1 {
		t.Fatalf("permanent: code=%d calls=%d (want 413, 1)", code, calls2)
	}
}

func TestOk2xx(t *testing.T) {
	for _, c := range []struct {
		code int
		want bool
	}{{200, true}, {201, true}, {204, true}, {299, true}, {199, false}, {300, false}, {413, false}, {500, false}} {
		if ok2xx(c.code) != c.want {
			t.Errorf("ok2xx(%d)=%v want %v", c.code, ok2xx(c.code), c.want)
		}
	}
}
