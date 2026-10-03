package backup

import (
	"strings"
	"testing"
)

// TestLudashProfile (F137).
func TestLudashProfile(t *testing.T) {
	p := ProfileFor("ghcr.io/theduffman85/linux-update-dashboard:latest")
	if p == nil || p.Name != "Linux Update Dashboard" {
		t.Fatalf("expected the dashboard profile, got %+v", p)
	}
	if p.OneWayMigration == "" {
		t.Error("it migrates on start — a downgrade must be blocked")
	}
	if !p.LocalOnlyPath("/data") {
		t.Error("WAL-mode SQLite must be kept off network storage")
	}
	// Still in the credential-store class, and the text must now name the
	// separation a backup collapses — that is the reason write-only matters here
	// more than the archive's size suggests.
	if p.CredentialStore == "" {
		t.Fatal("this app must stay in the credential-store class")
	}
	if !strings.Contains(p.CredentialStore, "both halves") {
		t.Errorf("the note should explain that a backup collapses the on-disk key separation, got %q", p.CredentialStore)
	}
	if v := AppVersionCompatibility(p, "2026.7.3", "2026.5.1"); !v.Blocking {
		t.Errorf("a downgrade must BLOCK, got %+v", v)
	}
}

// TestBindExternal (F136) is the kind DockBack cannot reach. The distinction
// matters because the failure it causes points at the wrong thing: the app is
// healthy and only login breaks.
func TestBindExternal(t *testing.T) {
	p := ProfileFor("linux-update-dashboard")
	var ext, env *AddressBinding
	for i := range p.Address {
		switch p.Address[i].Kind {
		case BindExternal:
			ext = &p.Address[i]
		case BindEnv:
			env = &p.Address[i]
		}
	}
	if env == nil || len(env.Keys) != 1 || env.Keys[0] != "LUDASH_BASE_URL" {
		t.Fatalf("the base URL must be an env binding DockBack sets, got %+v", env)
	}
	if ext == nil {
		t.Fatal("the identity provider's redirect URI must be declared as external")
	}
	// It must say plainly that this lives elsewhere, or an operator will wait for
	// DockBack to handle it.
	if !strings.Contains(ext.Note, "identity provider") {
		t.Errorf("the note must name what has to change, got %q", ext.Note)
	}
	if !strings.Contains(ext.Symptom, "reads as a broken restore") {
		t.Errorf("the symptom must name the misdiagnosis, got %q", ext.Symptom)
	}
	// And it must reassure that managed-host connections are unaffected —
	// otherwise a move looks scarier than it is for a fleet-patching tool.
	if !strings.Contains(ext.Note, "unaffected by a move") {
		t.Errorf("the note should say managed hosts are unaffected, got %q", ext.Note)
	}

	// An external binding is surfaced WHETHER OR NOT an address change is
	// planned: it is the one thing nothing here can verify afterwards.
	pre := AppPreconditionsFor(&Manifest{Image: "ghcr.io/theduffman85/linux-update-dashboard:latest"})
	if pre == nil {
		t.Fatal("expected preconditions")
	}
	joined := strings.Join(pre.Notes, " ")
	if !strings.Contains(joined, "cannot see or change this") {
		t.Errorf("the panel must be explicit that this is outside DockBack's reach, got %v", pre.Notes)
	}
	// Ordinary apps gain no external binding.
	for _, img := range []string{"nginx:alpine", "ghcr.io/gotify/server"} {
		if q := ProfileFor(img); q != nil {
			for _, b := range q.Address {
				if b.Kind == BindExternal {
					t.Errorf("%q should declare no external binding", img)
				}
			}
		}
	}
}
