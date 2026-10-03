package dockercli

import "strings"

import "testing"

// The probe script lives inside a Go RAW STRING literal, which a backtick
// silently terminates — producing a compile error far from the real mistake.
// It has happened three times while writing this feature, so it is now a test:
// the script must never contain one.
func TestProbeScriptHasNoBackticks(t *testing.T) {
	if strings.Contains(probeScript, "`") {
		t.Fatal("the probe script contains a backtick, which terminates the Go raw string literal it lives in")
	}
}

// The script must stay POSIX-sh compatible: it runs under BusyBox ash in the
// Alpine sidecar, not bash. These are the constructs that have actually bitten.
func TestProbeScriptStaysPOSIX(t *testing.T) {
	banned := map[string]string{
		"[[":       "bash test brackets are not in BusyBox ash",
		"function": "the bash 'function' keyword is not POSIX",
		"<<<":      "here-strings are a bashism",
		"${!":      "indirect expansion is a bashism",
	}
	for tok, why := range banned {
		if strings.Contains(probeScript, tok) {
			t.Errorf("probe script contains %q: %s", tok, why)
		}
	}
	// Rates are only meaningful as a delta, so both samples must be taken.
	for _, want := range []string{"sample a", "sample b", "SAMPLESECS"} {
		if !strings.Contains(probeScript, want) {
			t.Errorf("probe script is missing %q", want)
		}
	}
	// Display connectors must never be enumerated as GPUs.
	if !strings.Contains(probeScript, "case \"$cn\" in *-*) continue;; esac") {
		t.Error("probe script must skip DRM connector nodes (cardN-DP-1) when listing GPUs")
	}
}
