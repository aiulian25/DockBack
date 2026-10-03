package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/studio-b12/gowebdav"
)

// Stat answers three different questions and callers DELETE based on the answer:
// present, definitively absent, or unanswered. Every backend used to collapse
// the third into the second, so an expired credential or a 502 from a reverse
// proxy read exactly like "this backup is gone".

// statServer answers a HEAD/PROPFIND with a fixed status, standing in for the
// two cases that must not be confused: 404 (absent) and anything else.
func statServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestS3StatSeparatesAbsentFromUnreachable(t *testing.T) {
	newBackend := func(t *testing.T, status int) *S3 {
		srv := statServer(t, status)
		be, err := NewS3(map[string]string{
			"endpoint": strings.TrimPrefix(srv.URL, "http://"), "bucket": "backups",
			"access_key": "a", "secret_key": "s", "region": "us-east-1", "insecure": "true",
		})
		if err != nil {
			t.Fatal(err)
		}
		return be
	}

	// 404: the bucket answered and the object is not there.
	if n, ok, err := newBackend(t, http.StatusNotFound).Stat(context.Background(), "node/app/x.dback"); err != nil || ok || n != 0 {
		t.Errorf("a missing object must be (0, false, nil), got (%d, %v, %v)", n, ok, err)
	}
	// Everything else is a question that was not answered.
	for _, status := range []int{http.StatusForbidden, http.StatusUnauthorized, http.StatusBadGateway} {
		_, ok, err := newBackend(t, status).Stat(context.Background(), "node/app/x.dback")
		if err == nil {
			t.Errorf("HTTP %d must be reported as an error, not as a missing object", status)
		}
		if ok {
			t.Errorf("HTTP %d must not report the object as present", status)
		}
	}
}

func TestWebDAVStatSeparatesAbsentFromUnreachable(t *testing.T) {
	newBackend := func(t *testing.T, status int) *WebDAV {
		srv := statServer(t, status)
		be, err := NewWebDAV(map[string]string{"url": srv.URL, "username": "u", "password": "p"})
		if err != nil {
			t.Fatal(err)
		}
		return be
	}

	if n, ok, err := newBackend(t, http.StatusNotFound).Stat(context.Background(), "node/app/x.dback"); err != nil || ok || n != 0 {
		t.Errorf("a missing file must be (0, false, nil), got (%d, %v, %v)", n, ok, err)
	}
	// A rotated password (401) or the proxy in front of Nextcloud being unwell
	// (502) is not the same thing as the backup having been deleted.
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusBadGateway} {
		_, ok, err := newBackend(t, status).Stat(context.Background(), "node/app/x.dback")
		if err == nil {
			t.Errorf("HTTP %d must be reported as an error, not as a missing file", status)
		}
		if ok {
			t.Errorf("HTTP %d must not report the file as present", status)
		}
	}
}

// The SMB and SFTP libraries both normalise their "no such file" status to
// fs.ErrNotExist, which is what those backends key on. Anything else — access
// denied, a dropped session — must survive as an error.
func TestNotFoundClassification(t *testing.T) {
	cases := map[string]struct {
		err     error
		absent  bool
		comment string
	}{
		"smb: object name not found": {&os.PathError{Op: "stat", Path: `\b\k`, Err: fs.ErrNotExist}, true, ""},
		"smb: access denied":         {&os.PathError{Op: "stat", Path: `\b\k`, Err: os.ErrPermission}, false, "a permission change is not a deleted file"},
		"sftp: no such file":         {fs.ErrNotExist, true, ""},
		"sftp: session dropped":      {errors.New("connection lost"), false, "an unreachable server is not an empty one"},
		"wrapped not-found":          {fmt.Errorf("sftp stat %q: %w", "k", fs.ErrNotExist), true, "wrapping must not hide it"},
	}
	for name, c := range cases {
		if got := errors.Is(c.err, fs.ErrNotExist); got != c.absent {
			t.Errorf("%s: treated as absent = %v, want %v %s", name, got, c.absent, c.comment)
		}
	}

	// WebDAV needs the library's own helper: its 404 is a StatusError inside a
	// PathError and does NOT satisfy errors.Is(err, fs.ErrNotExist).
	notFound := gowebdav.NewPathError("stat", "/k", http.StatusNotFound)
	if !gowebdav.IsErrNotFound(notFound) {
		t.Error("a 404 must be recognised as absent")
	}
	if errors.Is(notFound, fs.ErrNotExist) {
		t.Log("gowebdav now maps 404 to fs.ErrNotExist too — the extra check is harmless")
	}
	if gowebdav.IsErrNotFound(gowebdav.NewPathError("stat", "/k", http.StatusBadGateway)) {
		t.Error("a 502 must not be read as absent")
	}
}

// The local backend already had the contract; this pins it so the fleet-wide
// rule is checked on every implementation that has a testable one.
func TestLocalStatKeepsTheContract(t *testing.T) {
	dir := t.TempDir()
	be, err := NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := be.Stat(context.Background(), "nothing/here.dback"); err != nil || ok {
		t.Errorf("a missing file must be (0, false, nil), got (%v, %v)", ok, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "there.dback"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, ok, err := be.Stat(context.Background(), "there.dback"); err != nil || !ok || n != 5 {
		t.Errorf("an existing file must report its size: (%d, %v, %v)", n, ok, err)
	}
}
