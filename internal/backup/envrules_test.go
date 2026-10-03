package backup

import (
	"strings"
	"testing"
)

// PLAYBOOK §2.1: "#7 is a version-gap artifact, not a universal rule." R5's
// matched-platform control is the reason the gate exists at all.
func TestEnvironmentalRuleGate(t *testing.T) {
	for _, tc := range []struct {
		name        string
		diff        hostDiff
		usesLocal   bool
		wantApplied bool
		wantWhy     string
	}{
		{
			// R1 §Issue 7's exact migration.
			name:      "a newer engine with a localhost probe applies",
			diff:      hostDiff{SourceEngine: "24.0.2", TargetEngine: "29.7.2"},
			usesLocal: true, wantApplied: true, wantWhy: "::1 first",
		},
		{
			// R5 §2's control: matched platforms, rule did not fire.
			name:      "matched engines skip",
			diff:      hostDiff{SourceEngine: "29.7.2", TargetEngine: "29.7.2"},
			usesLocal: true, wantApplied: false, wantWhy: "not a newer engine",
		},
		{
			name:      "an older target cannot exhibit the resolution order",
			diff:      hostDiff{SourceEngine: "29.7.2", TargetEngine: "24.0.2"},
			usesLocal: true, wantApplied: false, wantWhy: "not a newer engine",
		},
		{
			// The other half of §2.1's reason: "no probe used localhost".
			name:      "no localhost in the probe skips, whatever the engines",
			diff:      hostDiff{SourceEngine: "24.0.2", TargetEngine: "29.7.2"},
			usesLocal: false, wantApplied: false, wantWhy: "no probe uses localhost",
		},
		{
			// Unknown is never "the same": a rule applied on a guess is churn.
			name:      "an unrecorded source engine skips",
			diff:      hostDiff{TargetEngine: "29.7.2"},
			usesLocal: true, wantApplied: false, wantWhy: "unknown",
		},
		{
			name:      "an unreadable target engine skips",
			diff:      hostDiff{SourceEngine: "24.0.2", TargetEngine: "not-a-version"},
			usesLocal: true, wantApplied: false, wantWhy: "unknown",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := localhostRewriteRule(tc.diff, tc.usesLocal)
			if got.Applied != tc.wantApplied {
				t.Errorf("applied = %v, want %v (%s)", got.Applied, tc.wantApplied, got.Why)
			}
			if !strings.Contains(got.Why, tc.wantWhy) {
				t.Errorf("why = %q, want it to mention %q", got.Why, tc.wantWhy)
			}
			if got.Why == "" {
				t.Error("§2.1: a skipped rule must say WHY it was skipped")
			}
		})
	}

	t.Run("both outcomes are stated, so skipped is never silence", func(t *testing.T) {
		applied := localhostRewriteRule(hostDiff{SourceEngine: "24.0.2", TargetEngine: "29.7.2"}, true).String()
		if !strings.HasPrefix(applied, "localhost-rewrite APPLIED (") {
			t.Errorf("applied = %q", applied)
		}
		skipped := localhostRewriteRule(hostDiff{SourceEngine: "29.7.2", TargetEngine: "29.7.2"}, true).String()
		if !strings.HasPrefix(skipped, "localhost-rewrite SKIPPED (") {
			t.Errorf("skipped = %q", skipped)
		}
	})

	t.Run("the engine major version is read, not the whole string", func(t *testing.T) {
		for _, tc := range []struct {
			in   string
			want int
			ok   bool
		}{
			{"29.7.2", 29, true}, {"24.0.2", 24, true}, {"27.1.1-rd", 27, true},
			{"", 0, false}, {"latest", 0, false}, {"0.9", 0, false},
		} {
			got, ok := engineMajor(tc.in)
			if got != tc.want || ok != tc.ok {
				t.Errorf("engineMajor(%q) = %d,%v want %d,%v", tc.in, got, ok, tc.want, tc.ok)
			}
		}
	})
}
