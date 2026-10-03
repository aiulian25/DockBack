package api

import (
	"encoding/json"
	"testing"

	"dockback/internal/backup"
)

// F79: a stack run must produce, per service, the same options a scheduled run
// of that container would — remembered compression / app-native export /
// save-image — with the stack dialog's per-run compression/pause overrides on
// top. Before F79 both stack paths hardcoded balanced and dropped the rest.

func TestStackServiceBackupOptions(t *testing.T) {
	s := &Server{store: testStore(t)}
	dests := []string{"d1"}

	// Remembered manual choices for "web" on node n1.
	js, _ := json.Marshal(backup.SavedBackupOptions{Compression: "xz", SaveImage: true})
	if err := s.store.SetSetting(backup.BackupOptionsKey("n1", "web"), string(js)); err != nil {
		t.Fatal(err)
	}

	// No per-run overrides → the remembered values apply verbatim.
	o := s.stackServiceBackupOptions("n1", "cid1", "web", dests, "", "")
	if o.Compression != "xz" || !o.SaveImage {
		t.Fatalf("remembered options dropped: compression=%q saveImage=%v", o.Compression, o.SaveImage)
	}
	if o.PauseMode != "" {
		t.Fatalf("no pause override requested, got %q", o.PauseMode)
	}
	if !o.DestinationsExplicit || len(o.Destinations) != 1 {
		t.Fatalf("destinations must stay explicit: %+v", o)
	}
	if o.IncludeMounts != nil {
		t.Fatalf("IncludeMounts must stay nil (remembered/default): %v", o.IncludeMounts)
	}

	// Per-run compression override wins, but the OTHER remembered fields stay.
	o = s.stackServiceBackupOptions("n1", "cid1", "web", dests, "fast", "stop")
	if o.Compression != "fast" {
		t.Fatalf("per-run compression should win: %q", o.Compression)
	}
	if !o.SaveImage {
		t.Fatal("per-run compression must not drop remembered SaveImage")
	}
	if o.PauseMode != "stop" {
		t.Fatalf("per-run pause mode should apply: %q", o.PauseMode)
	}

	// A service with NO saved options keeps today's default — balanced.
	o = s.stackServiceBackupOptions("n1", "cid2", "db", dests, "", "")
	if o.Compression != "balanced" || o.SaveImage || o.AppExport {
		t.Fatalf("no saved options must mean balanced/off: %+v", o)
	}
}

// F84: CompressionExplicit marks a deliberate choice — saved non-default and
// stack-dialog overrides set it; a saved/implicit "balanced" stays eligible for
// the incompressible-data autotune.
func TestCompressionExplicitFlags(t *testing.T) {
	s := &Server{store: testStore(t)}
	dests := []string{"d1"}

	// Saved "xz" → explicit; saved "balanced" → implicit; nothing saved → implicit.
	jsXz, _ := json.Marshal(backup.SavedBackupOptions{Compression: "xz"})
	_ = s.store.SetSetting(backup.BackupOptionsKey("n1", "media"), string(jsXz))
	jsBal, _ := json.Marshal(backup.SavedBackupOptions{Compression: "balanced"})
	_ = s.store.SetSetting(backup.BackupOptionsKey("n1", "photos"), string(jsBal))

	if o := s.scheduledBackupOptions("n1", "c1", "media", dests); !o.CompressionExplicit {
		t.Fatal("saved xz must be explicit")
	}
	if o := s.scheduledBackupOptions("n1", "c2", "photos", dests); o.CompressionExplicit {
		t.Fatal("saved balanced is the carried-along default — must stay implicit")
	}
	if o := s.scheduledBackupOptions("n1", "c3", "fresh", dests); o.CompressionExplicit {
		t.Fatal("no saved options must stay implicit")
	}

	// Stack-dialog override: ANY picked value is deliberate, balanced included.
	if o := s.stackServiceBackupOptions("n1", "c3", "fresh", dests, "balanced", ""); !o.CompressionExplicit {
		t.Fatal("stack per-run balanced override must be explicit")
	}
	if o := s.stackServiceBackupOptions("n1", "c2", "photos", dests, "", ""); o.CompressionExplicit {
		t.Fatal("no stack override must keep the saved/implicit state")
	}
}

// The request-level compression gate: every engine-recognized value passes,
// anything else is rejected (the handler 400s on !ValidCompression).
func TestStackCompressionValidation(t *testing.T) {
	for _, ok := range []string{"", "fast", "balanced", "max", "max-long", "gzip", "xz"} {
		if !backup.ValidCompression(ok) {
			t.Fatalf("%q should be a valid compression choice", ok)
		}
	}
	for _, bad := range []string{"zstd", "best", "none", "BALANCED", "lz4"} {
		if backup.ValidCompression(bad) {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}
