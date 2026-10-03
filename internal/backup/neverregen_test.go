package backup

import (
	"slices"
	"strings"
	"testing"
)

// secretValue is what must never appear anywhere this test looks. Shaped like a
// real Laravel key so a substring match would find it if anything leaked it.
const secretValue = "base64:kZ9vQm2XpL7wYtR4sN6cB8hJ3dF5gA1eT0uI9oP2xC="

func TestNeverRegenerate(t *testing.T) {
	const bookstack = "solidnerd/bookstack:23.06"

	t.Run("a profiled app missing its irreplaceable key fails the capture", func(t *testing.T) {
		var logs []string
		e := &Engine{Log: func(_, _, msg string) { logs = append(logs, msg) }}
		man := &Manifest{}
		env := []string{"APP_URL=https://wiki.example.com", "DB_HOST=db"}

		err := e.assertNeverRegenerate(man, "run1", "bookstack", bookstack, env)
		if err == nil {
			t.Fatal("a BookStack backup without APP_KEY restores into permanently unreadable columns; it must not be saved as a success")
		}
		for _, want := range []string{"APP_KEY", "BookStack", "bookstack"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal must name %q: %s", want, err)
			}
		}
		if len(logs) != 0 {
			t.Errorf("the refusal is the caller's to log: %v", logs)
		}
	})

	t.Run("a key present with an empty value is missing", func(t *testing.T) {
		// APP_KEY= carries nothing, and restores into the same unreadable columns
		// as recording no key at all.
		e := &Engine{Log: func(string, string, string) {}}
		if err := e.assertNeverRegenerate(&Manifest{}, "run1", "bookstack", bookstack, []string{"APP_KEY="}); err == nil {
			t.Error("an empty APP_KEY must be treated as absent")
		}
	})

	t.Run("a profiled app carrying its key passes, silently", func(t *testing.T) {
		var logs []string
		e := &Engine{Log: func(_, _, msg string) { logs = append(logs, msg) }}
		man := &Manifest{}
		env := []string{"APP_KEY=" + secretValue, "APP_URL=https://wiki.example.com"}

		if err := e.assertNeverRegenerate(man, "run1", "bookstack", bookstack, env); err != nil {
			t.Fatalf("a complete capture must proceed: %v", err)
		}
		assertNoSecretAnywhere(t, man, logs, "")
	})

	t.Run("an unprofiled app gets a finding, never a failure", func(t *testing.T) {
		var logs []string
		e := &Engine{Log: func(_, _, msg string) { logs = append(logs, msg) }}
		man := &Manifest{}
		env := []string{
			"FOO_SECRET=" + secretValue,
			"WIDGET_API_KEY=" + secretValue,
			"SESSION_SALT=" + secretValue,
			"EMPTY_SECRET=",
			"GPG_KEY=A035C8C19219BA821ECEA86B64E628F8D684696D",
			"SIGNING_PUBLIC_KEY=ssh-ed25519 AAAA",
			"APP_URL=https://example.com",
			"PATH=/usr/bin",
		}

		if err := e.assertNeverRegenerate(man, "run1", "someapp", "ghcr.io/nobody/someapp:1", env); err != nil {
			t.Fatalf("a suffix match is a guess about a NAME; hard-failing on it would refuse to back up most stacks: %v", err)
		}
		if len(man.Findings) != 1 || man.Findings[0].Code != findingIrreplaceableSecret {
			t.Fatalf("want one %s finding, got %+v", findingIrreplaceableSecret, man.Findings)
		}
		if man.Findings[0].Severity != FindingInfo {
			t.Errorf("severity = %q, want info — nothing is wrong, it is worth knowing", man.Findings[0].Severity)
		}
		message := man.Findings[0].Message
		for _, want := range []string{"FOO_SECRET", "WIDGET_API_KEY", "SESSION_SALT"} {
			if !strings.Contains(message, want) {
				t.Errorf("the finding must name %s: %s", want, message)
			}
		}
		// Public by definition. Listing them teaches the operator to disbelieve
		// the finding, which costs more than the two names are worth.
		for _, notWant := range []string{"EMPTY_SECRET", "APP_URL", "PATH", "GPG_KEY", "SIGNING_PUBLIC_KEY"} {
			if strings.Contains(message, notWant) {
				t.Errorf("%s is not an irreplaceable value: %s", notWant, message)
			}
		}
		assertNoSecretAnywhere(t, man, logs, "")
	})

	t.Run("restore over an existing container with a changed key", func(t *testing.T) {
		keys := NeverRegenerateFor(bookstack)
		if !slices.Contains(keys, "APP_KEY") {
			t.Fatalf("the BookStack profile must declare APP_KEY, got %v", keys)
		}
		recorded := []string{"APP_KEY=" + secretValue, "APP_URL=https://wiki.example.com"}

		// The target was rebuilt and given a fresh key. The data would restore
		// encrypted under the old one and never be readable again.
		changed := ChangedNeverRegenerateKeys(keys, recorded, []string{"APP_KEY=base64:DIFFERENT0000000000000000000000000000000000="})
		if len(changed) != 1 || changed[0] != "APP_KEY" {
			t.Fatalf("a changed key must be reported by NAME, got %v", changed)
		}
		warning := ChangedSecretWarning("BookStack", changed)
		if !strings.Contains(warning, "restoring would keep data encrypted under a key this target no longer has") {
			t.Errorf("the warning must say what goes wrong: %s", warning)
		}
		assertNoSecretAnywhere(t, nil, nil, warning)

		// Same value on both sides: nothing to say.
		if got := ChangedNeverRegenerateKeys(keys, recorded, recorded); len(got) != 0 {
			t.Errorf("an unchanged key is not a refusal: %v", got)
		}
		// The target has no such variable at all — same permanent outcome.
		if got := ChangedNeverRegenerateKeys(keys, recorded, []string{"APP_URL=x"}); len(got) != 1 {
			t.Errorf("a target missing the key entirely must also refuse: %v", got)
		}
		// The BACKUP does not carry it, so there is nothing to compare against.
		// (That case is the capture-side failure above, not a restore refusal.)
		if got := ChangedNeverRegenerateKeys(keys, []string{"APP_URL=x"}, []string{"APP_KEY=anything"}); len(got) != 0 {
			t.Errorf("no recorded value means no comparison: %v", got)
		}
	})

	t.Run("an unprofiled app with nothing secret-shaped says nothing", func(t *testing.T) {
		e := &Engine{Log: func(string, string, string) {}}
		man := &Manifest{}
		if err := e.assertNeverRegenerate(man, "run1", "web", "nginx:1.27", []string{"PATH=/usr/bin"}); err != nil {
			t.Fatal(err)
		}
		if len(man.Findings) != 0 {
			t.Errorf("nothing to report: %+v", man.Findings)
		}
	})
}

// assertNoSecretAnywhere is the pass criterion "values never appear in log
// fixtures", applied to every surface a value could escape through.
func assertNoSecretAnywhere(t *testing.T, man *Manifest, logs []string, extra string) {
	t.Helper()
	surfaces := append([]string{extra}, logs...)
	if man != nil {
		for _, f := range man.Findings {
			surfaces = append(surfaces, f.Message, f.Subject)
		}
	}
	for _, s := range surfaces {
		if strings.Contains(s, secretValue) {
			t.Errorf("a secret VALUE escaped into operator-facing text: %s", s)
		}
	}
}

func TestNeverRegenerateFiles(t *testing.T) {
	const nextcloud = "nextcloud:30-apache"

	t.Run("the Nextcloud profile declares its config-file secrets", func(t *testing.T) {
		profile := ProfileFor(nextcloud)
		if profile == nil || len(profile.NeverRegenerateFiles) != 1 {
			t.Fatalf("want one declared config location, got %+v", profile)
		}
		spec := profile.NeverRegenerateFiles[0]
		if spec.Path != "/var/www/html/config" {
			t.Errorf("path = %q", spec.Path)
		}
		if !slices.Contains(spec.Keys, "secret") || !slices.Contains(spec.Keys, "passwordsalt") {
			t.Errorf("keys = %v, want secret and passwordsalt", spec.Keys)
		}
		// They must NOT also be in the environment list: they are never in the
		// environment, so that would refuse every Nextcloud backup.
		if len(profile.NeverRegenerate) != 0 {
			t.Errorf("config-file secrets must not be asserted against the environment: %v", profile.NeverRegenerate)
		}
	})

	t.Run("the probe emits key names and verdicts, never values", func(t *testing.T) {
		script := neverRegenerateFileScript(NeverRegenerateFile{
			Path: "/var/www/html/config", Keys: []string{"secret", "passwordsalt"},
		})
		for _, want := range []string{"'/var/www/html/config'", "grep -r -s -q -E", "FOUND|%s", "MISSING|%s", "UNREADABLE|%s"} {
			if !strings.Contains(script, want) {
				t.Errorf("script must contain %q: %s", want, script)
			}
		}
		// grep -q: the matched LINE must never be printed, because the matched
		// line is the secret.
		if strings.Contains(script, "grep -r -s -E") || strings.Contains(script, "-n ") {
			t.Error("the probe must never print a matching line")
		}
		// A key that could not be a setting name never becomes a pattern.
		bad := neverRegenerateFileScript(NeverRegenerateFile{Path: "/c", Keys: []string{"a'; rm -rf /; #"}})
		if strings.Contains(bad, "rm -rf") {
			t.Errorf("an unsafe key must be dropped, not escaped into the script: %s", bad)
		}
	})

	t.Run("parse: silence is not absence", func(t *testing.T) {
		present, unreadable := parseConfigKeyProbe("FOUND|secret\nMISSING|passwordsalt\n")
		if unreadable {
			t.Error("a complete report is not unreadable")
		}
		if !present["secret"] || present["passwordsalt"] {
			t.Errorf("present = %v", present)
		}
		if got := missingConfigKeys([]string{"secret", "passwordsalt"}, present); len(got) != 1 || got[0] != "passwordsalt" {
			t.Errorf("missing = %v, want only passwordsalt", got)
		}
		// A key the probe said nothing about must NOT be counted missing — that
		// is the difference between an assertion and a guess.
		if got := missingConfigKeys([]string{"secret", "neverprobed"}, present); len(got) != 0 {
			t.Errorf("an unprobed key must not fail a backup: %v", got)
		}
		// An absent path yields no assertion at all.
		if _, unreadable := parseConfigKeyProbe("UNREADABLE|/var/www/html/config\n"); !unreadable {
			t.Error("an absent path must read as unreadable")
		}
		if _, unreadable := parseConfigKeyProbe("garbage\n"); unreadable {
			t.Error("noise is not an unreadable verdict")
		}
	})

	t.Run("the pattern distinguishes a value from an empty one and from a lookalike key", func(t *testing.T) {
		// Both of these were found by running the pattern against a real
		// config.php, not by reading it.
		pattern := configKeyPattern("secret")
		if !strings.HasPrefix(pattern, `(^|[^A-Za-z0-9_])`) {
			t.Error("without a left boundary, 'dbsecret' vouches for 'secret'")
		}
		if !strings.Contains(pattern, `[^'",>[:space:]]`) {
			t.Error("without '>' excluded, the '=' alternative lets `'secret' => ''` read as present")
		}
	})
}
