package backup

import (
	"path/filepath"
	"strings"
	"testing"

	"dockback/internal/store"
)

// Termix: the write-only requirement, the version lock, and what the profile
// says about an archive that is somebody's whole SSH fleet (F163–F165).

func termixEngine(t *testing.T) *Engine {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Engine{Store: st}
}

// F163 — off by default, and off means nothing changes for anybody.
func TestRequireWriteOnlyDefaultOff(t *testing.T) {
	e := termixEngine(t)
	if e.RequireWriteOnly("n1", "termix") {
		t.Error("the requirement must be opt-in — refusing by default would mean no backup at all for somebody who has not set up an offline key yet")
	}
	if err := e.writeOnlyRequirement("n1", "termix", "ghcr.io/lukegus/termix"); err != nil {
		t.Errorf("with the requirement off, a run must proceed: %v", err)
	}
}

// On, and write-only is off: the run is refused, and the message carries the
// whole decision for somebody who did not set the flag.
func TestRequireWriteOnlyRefusesWhenOff(t *testing.T) {
	e := termixEngine(t)
	if err := e.SetRequireWriteOnly("n1", "termix", true); err != nil {
		t.Fatal(err)
	}
	err := e.writeOnlyRequirement("n1", "termix", "ghcr.io/lukegus/termix")
	if err == nil {
		t.Fatal("a run that would produce a readable archive must be refused")
	}
	msg := err.Error()
	for _, want := range []string{
		"SSH credentials", // why THIS container
		"which is currently OFF",
		"Turn write-only back on", // the way out
		"clear the requirement",   // and the other way out
		"Nothing was captured",    // what state the operator is in
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal should contain %q; got %q", want, msg)
		}
	}
	// The same refusal is what the UI shows before a run is ever started.
	if got := e.WriteOnlyRequirementFor("n1", "termix", "ghcr.io/lukegus/termix"); got != msg {
		t.Error("the UI must show exactly the reason the run would give")
	}
}

// On, and write-only is on: nothing to refuse.
func TestRequireWriteOnlySatisfied(t *testing.T) {
	e := termixEngine(t)
	if err := e.SetRequireWriteOnly("n1", "termix", true); err != nil {
		t.Fatal(err)
	}
	// Arming write-only is storing a public key — public material, by design.
	if err := e.Store.SetSetting("backup.write_only_pubkey", "a-public-key"); err != nil {
		t.Fatal(err)
	}
	if !e.WriteOnlyEnabled() {
		t.Fatal("write-only should be armed")
	}
	if err := e.writeOnlyRequirement("n1", "termix", "ghcr.io/lukegus/termix"); err != nil {
		t.Errorf("with write-only on, the run must proceed: %v", err)
	}
	if got := e.WriteOnlyRequirementFor("n1", "termix", "ghcr.io/lukegus/termix"); got != "" {
		t.Errorf("nothing to report: %q", got)
	}
}

// It is per container, and it is a plain toggle.
func TestRequireWriteOnlyIsPerContainer(t *testing.T) {
	e := termixEngine(t)
	if err := e.SetRequireWriteOnly("n1", "termix", true); err != nil {
		t.Fatal(err)
	}
	if e.RequireWriteOnly("n1", "other") {
		t.Error("the setting must not leak to another container on the same node")
	}
	if e.RequireWriteOnly("n2", "termix") {
		t.Error("the setting must not leak to the same name on another node")
	}
	// And a container with no profile at all can still be marked — the reasoning
	// generalises, and the registry does not know every credential store.
	if err := e.SetRequireWriteOnly("n1", "something-custom", true); err != nil {
		t.Fatal(err)
	}
	err := e.writeOnlyRequirement("n1", "something-custom", "someone/private-image")
	if err == nil || !strings.Contains(err.Error(), "marked as one that must only be backed up") {
		t.Errorf("an unrecognised image must still be protected, got %v", err)
	}
	// Turning it off clears it.
	if err := e.SetRequireWriteOnly("n1", "termix", false); err != nil {
		t.Fatal(err)
	}
	if e.RequireWriteOnly("n1", "termix") {
		t.Error("turning it off must clear it")
	}
}

// F164 — the strictest version verdict: both directions blocked, with the way
// out named.
func TestTermixVersionLock(t *testing.T) {
	p := ProfileFor("ghcr.io/lukegus/termix:latest")
	if p == nil {
		t.Fatal("Termix must have a profile")
	}
	if p.VersionLock == "" {
		t.Fatal("Termix must lock its version")
	}
	up := AppVersionCompatibility(p, "2.5.1", "2.6.0")
	if !up.Blocking {
		t.Error("restoring into a NEWER Termix must be blocked — an upgrade has no demonstrated compatibility story either")
	}
	down := AppVersionCompatibility(p, "2.6.0", "2.5.1")
	if !down.Blocking {
		t.Error("restoring into an older Termix must be blocked")
	}
	if !strings.Contains(up.Warning, "Pin the target") || !strings.Contains(up.Warning, "image digest") {
		t.Errorf("the block must name the way out; got %q", up.Warning)
	}
	// The same version is silent — a digest-pinned restore, which is the normal
	// path, lands here and must never be blocked.
	if same := AppVersionCompatibility(p, "2.5.1", "2.5.1"); same.Blocking || same.Warning != "" {
		t.Errorf("an identical version must be silent, got %+v", same)
	}
	// An unreadable version on either side yields no verdict at all — the
	// fail-open rule the other verdicts follow.
	if v := AppVersionCompatibility(p, "", "2.5.1"); v.Blocking || v.Warning != "" {
		t.Errorf("an unknown version must produce no verdict, got %+v", v)
	}
	if v := AppVersionCompatibility(p, "2.5.1", "latest"); v.Blocking || v.Warning != "" {
		t.Errorf("an unreadable target version must produce no verdict, got %+v", v)
	}
}

// F165 — what the profile claims, and what it deliberately does not.
func TestTermixProfile(t *testing.T) {
	p := ProfileFor("ghcr.io/lukegus/termix:latest")
	if p == nil {
		t.Fatal("no profile")
	}
	// The credential-store text has to say the thing that makes this different
	// from every other archive: the key is in the same directory as the data.
	for _, want := range []string{"same directory", "write-only", "rotate every credential"} {
		if !strings.Contains(p.CredentialStore, want) {
			t.Errorf("the credential-store note should mention %q; got %q", want, p.CredentialStore)
		}
	}
	// A STOP, not a pause: pausing freezes the process with its state in memory.
	if mode, why := AppPauseDefault("ghcr.io/lukegus/termix"); mode != PauseStop || why == "" {
		t.Errorf("Termix must default to a full stop, with a reason; got %q / %q", mode, why)
	}
	// The database is encrypted, so there is no snapshot path — which is exactly
	// what the embedded-database warning is for, and why the quiesce matters.
	if !strings.Contains(p.EmbeddedDBWarning, "never opens") {
		t.Errorf("the warning should say DockBack never opens the database; got %q", p.EmbeddedDBWarning)
	}
	// The key file's permissions are audited.
	if len(p.SecretFiles) == 0 || !strings.HasSuffix(p.SecretFiles[0].Path, "/.env") {
		t.Error("the key file's permissions must be audited")
	}
	if p.SecretFiles[0].MaxMode != 0o600 {
		t.Errorf("the file that decrypts the database should be owner-only, got %o", p.SecretFiles[0].MaxMode)
	}
	// NOTHING may be excluded from this application. Every part of the data
	// directory is either the database or the key that opens it.
	if len(p.NeverBackup) != 0 || len(p.Regenerable) != 0 {
		t.Error("nothing in Termix's data directory is disposable — no exclusions may be declared")
	}
	// The move story: its own address does not matter, and the two deployments
	// must never be crossed.
	note := p.Address[0].Note
	if !strings.Contains(note, "its OWN backup") {
		t.Errorf("the note must warn against cross-restoring two instances; got %q", note)
	}
}

// The pre-restore panel must carry the things that decide whether this restore
// is safe at all.
func TestTermixPreconditions(t *testing.T) {
	pre := AppPreconditionsFor(&Manifest{Image: "ghcr.io/lukegus/termix:latest"})
	if pre == nil {
		t.Fatal("Termix must have preconditions")
	}
	joined := strings.ToLower(strings.Join(pre.Notes, " "))
	for _, want := range []string{"encrypted", "never opens", "local disk"} {
		if !strings.Contains(joined, want) {
			t.Errorf("preconditions should mention %q; got %q", want, joined)
		}
	}
}

// The backup page must recommend write-only BEFORE the choice is made, since
// after the fact it is only a regret.
func TestTermixAdvertisesWriteOnly(t *testing.T) {
	joined := strings.Join(AutoHookLabels("ghcr.io/lukegus/termix:latest", false), " | ")
	if !strings.Contains(joined, "write-only") {
		t.Errorf("write-only must be recommended on the backup page; got %q", joined)
	}
	if !strings.Contains(joined, "never opens") {
		t.Errorf("the never-decrypted property should be stated too; got %q", joined)
	}
}
