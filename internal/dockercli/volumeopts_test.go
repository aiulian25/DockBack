package dockercli

import (
	"errors"
	"strings"
	"testing"
)

// A named volume backed by NFS or CIFS used to come back as plain local disk on
// restore, silently. These tests pin the fix and — more importantly — the line
// it must not cross: the manifest sidecar is READABLE by default and travels to
// every destination, so a CIFS password must never reach it.

func TestRedactVolumeOptionsWithholdsCredentials(t *testing.T) {
	// A real CIFS volume: the password lives inside the packed `o` string.
	opts := map[string]string{
		"type":   "cifs",
		"device": "//nas.local/backups",
		"o":      "username=backup,password=hunter2,vers=3.0,uid=1000",
	}
	safe, redacted := RedactVolumeOptions(opts)

	if strings.Contains(safe["o"], "hunter2") {
		t.Fatalf("the password must never survive into the manifest: %q", safe["o"])
	}
	// Everything that is NOT a credential must survive, or the mount spec is
	// useless and we would have dropped the whole feature to protect one field.
	for _, want := range []string{"username=backup", "vers=3.0", "uid=1000"} {
		if !strings.Contains(safe["o"], want) {
			t.Fatalf("non-secret mount options must be preserved (%s): %q", want, safe["o"])
		}
	}
	if safe["type"] != "cifs" || safe["device"] != "//nas.local/backups" {
		t.Fatalf("plain options must pass through: %+v", safe)
	}
	if len(redacted) != 1 || redacted[0] != "o.password" {
		t.Fatalf("the withheld key must be reported, got %v", redacted)
	}
	if !HasRedactedOptions(safe) {
		t.Fatal("HasRedactedOptions must detect the withheld value")
	}
}

// NFS carries no credential, so it must round-trip completely — this is the case
// the feature exists for.
func TestRedactVolumeOptionsLeavesNFSIntact(t *testing.T) {
	opts := map[string]string{
		"type":   "nfs",
		"o":      "addr=10.0.0.5,nfsvers=4,rw",
		"device": ":/export/backups",
	}
	safe, redacted := RedactVolumeOptions(opts)
	if len(redacted) != 0 {
		t.Fatalf("an NFS volume has nothing to withhold, got %v", redacted)
	}
	if HasRedactedOptions(safe) {
		t.Fatal("nothing should be marked redacted")
	}
	for k, v := range opts {
		if safe[k] != v {
			t.Fatalf("%s changed: %q -> %q", k, v, safe[k])
		}
	}
	// Nothing recorded stays nothing.
	if s, r := RedactVolumeOptions(nil); s != nil || r != nil {
		t.Fatalf("empty options must stay empty: %+v %v", s, r)
	}
}

// A top-level secret key must be caught too, not just one inside `o`.
func TestRedactVolumeOptionsCatchesTopLevelSecrets(t *testing.T) {
	safe, redacted := RedactVolumeOptions(map[string]string{
		"PASSWORD": "s3cret", "access-key": "AKIA…", "endpoint": "https://s3.local",
	})
	if safe["PASSWORD"] == "s3cret" || safe["access-key"] == "AKIA…" {
		t.Fatalf("secret-named options must be withheld: %+v", safe)
	}
	if safe["endpoint"] != "https://s3.local" {
		t.Fatalf("a non-secret option must survive: %+v", safe)
	}
	if len(redacted) != 2 {
		t.Fatalf("both secrets must be reported, got %v", redacted)
	}
}

// The whole point: restoring into a volume backed by different storage would
// write the data to the wrong place.
func TestVolumeOptionsMismatchIsRecognisable(t *testing.T) {
	err := error(&VolumeOptionsMismatchError{
		Name: "app_data",
		Diff: []string{`device recorded as ":/export/app", host has ":/export/other"`},
	})
	if !errors.Is(err, ErrVolumeOptionsMismatch) {
		t.Fatal("a mismatch must satisfy errors.Is(err, ErrVolumeOptionsMismatch)")
	}
	msg := err.Error()
	for _, want := range []string{"app_data", "wrong backing store", ":/export/other"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("the message must state %q: %s", want, msg)
		}
	}
}

func TestDiffVolumeOptions(t *testing.T) {
	want := map[string]string{"type": "nfs", "device": ":/export/app"}

	if d := diffVolumeOptions(want, map[string]string{"type": "nfs", "device": ":/export/app"}); len(d) != 0 {
		t.Fatalf("an identical volume must produce no diff: %v", d)
	}
	d := diffVolumeOptions(want, map[string]string{"type": "nfs", "device": ":/export/other"})
	if len(d) != 1 || !strings.Contains(d[0], "device") {
		t.Fatalf("a changed device must be reported: %v", d)
	}
	d = diffVolumeOptions(want, map[string]string{"type": "nfs"})
	if len(d) != 1 || !strings.Contains(d[0], "absent on this host") {
		t.Fatalf("a missing option must be reported: %v", d)
	}
	// A value WE withheld cannot be compared — complaining that it differs would
	// be blaming the host for our own redaction.
	withheld := map[string]string{"o": "username=u,password=" + redactedMarker}
	if d := diffVolumeOptions(withheld, map[string]string{"o": "username=u,password=real"}); len(d) != 0 {
		t.Fatalf("a redacted value must not be diffed: %v", d)
	}
	// A pre-F90 backup recorded nothing, so there is nothing to disagree about.
	if d := diffVolumeOptions(nil, map[string]string{"type": "nfs"}); len(d) != 0 {
		t.Fatalf("no recorded options must mean no diff: %v", d)
	}
}
