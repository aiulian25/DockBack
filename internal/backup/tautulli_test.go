package backup

import (
	"strings"
	"testing"
)

// Tautulli: the dependency address, the reachability probe, and the curated
// exclusions (F160–F162).

// F160 — the generated INI edit's safety-critical properties. All of them were
// also verified by RUNNING it against a realistic config.ini under the sidecar's
// own shell; these pin what a future edit could quietly break.
func TestIniSetScript(t *testing.T) {
	script := iniSetScript("/config/config.ini", "PMS", map[string]string{"pms_url": "http://10.0.0.5:32400"})

	// Section-scoped. A key name is only unique inside its section, and awk is
	// used precisely because sed cannot know which section it is in.
	if !strings.Contains(script, "-v SEC='PMS'") {
		t.Error("the edit must be scoped to the declared section")
	}
	// The value arrives through -v and is never interpolated into the program
	// text, so it cannot become part of the script.
	if !strings.Contains(script, "-v VAL='http://10.0.0.5:32400'") {
		t.Error("the value must be passed as an awk variable")
	}
	// A key that matched nothing is a silent no-op, which is the failure this
	// guards against: awk exits 9, and the grep afterwards proves the value took.
	if !strings.Contains(script, "exit 9") || !strings.Contains(script, "did not take the new value") {
		t.Error("an edit that changed nothing must fail loudly")
	}
	// A missing file or a missing awk is reported rather than silently skipped.
	for _, want := range []string{"no such file", "no awk"} {
		if !strings.Contains(script, want) {
			t.Errorf("the script should handle %q", want)
		}
	}
	// The working copy is removed.
	if !strings.Contains(script, "rm -f") {
		t.Error("the temporary file must not be left behind")
	}
}

// The keys are filled from one address: a URL key gets the whole thing, a host
// key gets only the host — a key named for a host that is handed a URL is a
// value the application will never match.
func TestUpstreamHost(t *testing.T) {
	cases := map[string]string{
		"http://10.168.1.123:32400":  "10.168.1.123",
		"https://plex.example.com":   "plex.example.com",
		"10.168.1.50:32400":          "10.168.1.50",
		"plex.example.com":           "plex.example.com",
		"http://[2001:db8::1]:32400": "2001:db8::1",
	}
	for in, want := range cases {
		if got := upstreamHost(in); got != want {
			t.Errorf("upstreamHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// The declaration itself: what is written, where, and what the probe promises.
func TestTautulliUpstream(t *testing.T) {
	up := upstreamFor("ghcr.io/tautulli/tautulli:latest")
	if up == nil {
		t.Fatal("Tautulli must declare the address of the server it reads from")
	}
	if up.File != "/config/config.ini" || up.Section != "PMS" {
		t.Errorf("unexpected target: %s [%s]", up.File, up.Section)
	}
	if len(up.URLKeys) == 0 || len(up.HostKeys) == 0 {
		t.Error("both the URL and the bare-host key are recorded by this application")
	}
	// The credential and the recorded identity must NOT be in the rewrite set —
	// the whole point is that they stay valid wherever the server went.
	for _, k := range append(append([]string{}, up.URLKeys...), up.HostKeys...) {
		if strings.Contains(k, "token") || strings.Contains(k, "identifier") || strings.Contains(k, "client_id") {
			t.Errorf("%q must never be rewritten — moving a server does not invalidate its credential", k)
		}
	}
	if up.Symptom == "" || !strings.Contains(strings.ToLower(up.Symptom), "silently") {
		t.Errorf("the symptom should say that the failure is a quiet one; got %q", up.Symptom)
	}

	// The probe prints a verdict and nothing else, and never the token.
	probe := strings.Join(up.Probe, " ")
	if !strings.Contains(probe, "PLEXCHK") || up.ProbeOK != "PLEXCHK ok" {
		t.Error("the probe must print a recognisable verdict")
	}
	if strings.Contains(probe, "print(tok") || strings.Contains(probe, `"pms_token", tok`) {
		t.Error("the token must never be printed")
	}
	// It must degrade to silence rather than to a failure when the interpreter is
	// not there — a check that could not run must not read as one that failed.
	if !strings.Contains(probe, "command -v python3") {
		t.Error("a missing interpreter must produce no verdict")
	}

	// Nothing else in the registry declares one, so no other restore grows a field.
	if got := upstreamFor("nginx:1.27"); got != nil {
		t.Errorf("an ordinary image must declare no dependency address, got %+v", got)
	}
}

// The field only appears for an application that has one, and its label says
// which question it is asking.
func TestUpstreamPrompt(t *testing.T) {
	got := UpstreamPrompt("ghcr.io/tautulli/tautulli")
	if !strings.Contains(got, "Plex") || !strings.Contains(got, "leave blank") {
		t.Errorf("the label should name the service and say blank is fine; got %q", got)
	}
	if UpstreamPrompt("nginx:1.27") != "" {
		t.Error("an ordinary image must offer no such field")
	}

	// And it reaches the pre-restore panel, alongside a note explaining that
	// moving THIS application needs nothing.
	pre := AppPreconditionsFor(&Manifest{Image: "ghcr.io/tautulli/tautulli:latest"})
	if pre == nil || pre.UpstreamPrompt == "" {
		t.Fatalf("the prompt must reach the restore dialog: %+v", pre)
	}
	joined := strings.Join(pre.Notes, " ")
	if !strings.Contains(joined, "needs nothing here") {
		t.Errorf("the note should say a move of Tautulli itself changes nothing; got %q", joined)
	}
}

// F162 — the curated selection: two thirds of the old archive was cache and
// logs, and the parts that matter must not be excluded with them.
func TestTautulliExclusions(t *testing.T) {
	p := ProfileFor("ghcr.io/tautulli/tautulli")
	if p == nil {
		t.Fatal("no profile")
	}
	excluded := map[string]bool{}
	for _, nb := range p.NeverBackup {
		excluded[nb.Path] = true
		if nb.Why == "" {
			t.Errorf("%q is excluded without saying why", nb.Path)
		}
	}
	for _, want := range []string{"/config/cache", "/config/logs"} {
		if !excluded[want] {
			t.Errorf("%q should be excluded", want)
		}
	}
	// The database and the settings are the backup. Excluding either would be
	// excluding the point.
	for _, keep := range []string{"/config/tautulli.db", "/config/config.ini", "/config/newsletters", "/config/exports"} {
		if excluded[keep] {
			t.Errorf("%q must never be excluded", keep)
		}
	}
	// The application's own scheduled copies are a CHOICE — real value as a
	// portable extra, so not an always-exclusion.
	if len(p.Regenerable) != 1 || p.Regenerable[0].Path != "/config/backups" {
		t.Errorf("its own backup copies should be an opt-in exclusion, got %+v", p.Regenerable)
	}
	if !strings.Contains(p.Regenerable[0].Cost, "credentials") {
		t.Error("the trade should mention that those copies carry the same credentials")
	}
}

// The rest of the profile's claims.
func TestTautulliProfile(t *testing.T) {
	p := ProfileFor("ghcr.io/tautulli/tautulli:latest")
	if p == nil {
		t.Fatal("Tautulli must have a profile")
	}
	if p.CredentialStore == "" {
		t.Error("an archive holding a live media-server token is a credential store")
	}
	if len(p.CriticalTables) == 0 {
		t.Error("an empty history after a restore is a fresh install, not a shortfall")
	}
	for _, c := range p.CriticalTables {
		if c.DB != "/config/tautulli.db" || c.Means == "" {
			t.Errorf("critical table declared badly: %+v", c)
		}
	}
	// The configuration file holds the token and the hashed web password.
	if len(p.SecretFiles) == 0 || !strings.HasSuffix(p.SecretFiles[0].Path, "config.ini") {
		t.Error("the configuration file's permissions must be audited")
	}
	// Forward migrates, backward is refused.
	if v := AppVersionCompatibility(p, "2.17.2", "2.14.0"); !v.Blocking {
		t.Error("restoring a newer backup into an older Tautulli must be blocked")
	}
	if v := AppVersionCompatibility(p, "2.14.0", "2.17.2"); v.Blocking || v.Warning == "" {
		t.Error("restoring into a newer Tautulli must warn, not block")
	}
}
