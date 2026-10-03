package backup

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"dockback/internal/storage"
)

func TestPreflightChecks(t *testing.T) {
	be, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Missing/short key -> fail fast (never write unencryptable data).
	bad := &Engine{Storage: be, Key: make([]byte, 16), Log: func(string, string, string) {}}
	if _, err := bad.preflight(ctx, nil, Options{}, "", "id"); err == nil || !strings.Contains(err.Error(), "encryption key") {
		t.Fatalf("short key should fail preflight, got %v", err)
	}

	// Valid key + writable local storage, no DB engine -> passes.
	ok := &Engine{Storage: be, Key: make([]byte, 32), Log: func(string, string, string) {}}
	fallback, err := ok.preflight(ctx, nil, Options{}, "", "id")
	if err != nil {
		t.Fatalf("valid key + writable storage should pass, got %v", err)
	}
	// F103: a container that is not a database engine reports no fallback, so an
	// ordinary backup never picks up the raw-files flag.
	if fallback != "" {
		t.Fatalf("a non-database container must report no db fallback, got %q", fallback)
	}
}

// F103: the wording is shared by the manifest, the confidence grade and the
// runbook, so it lives in one place and names the engine that degraded.
func TestDBFallbackNote(t *testing.T) {
	got := dbFallbackNote("postgres")
	for _, want := range []string{"postgres", "dump tools not found", "raw files"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the note must contain %q: %s", want, got)
		}
	}
	if dbFallbackNote("") != "" {
		t.Fatal("no engine means no fallback note")
	}
}

// roBackend is a storage.Backend whose Put fails — simulating an unreachable or
// read-only destination so the writability pre-flight is exercised.
type roBackend struct{}

func (roBackend) Put(context.Context, string, io.Reader) (int64, error) {
	return 0, fmt.Errorf("read-only")
}
func (roBackend) Get(context.Context, string) (io.ReadCloser, error) { return nil, fmt.Errorf("n/a") }
func (roBackend) Delete(context.Context, string) error               { return nil }
func (roBackend) Stat(context.Context, string) (int64, bool, error)  { return 0, false, nil }
func (roBackend) FreeBytes(context.Context) (uint64, error)          { return 0, nil }
func (roBackend) Name() string                                       { return "ro-test" }

func TestPreflightFailsOnUnwritableStorage(t *testing.T) {
	e := &Engine{Storage: roBackend{}, Key: make([]byte, 32), Log: func(string, string, string) {}}
	if _, err := e.preflight(context.Background(), nil, Options{}, "", "id"); err == nil || !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("unwritable storage should fail preflight, got %v", err)
	}
}
