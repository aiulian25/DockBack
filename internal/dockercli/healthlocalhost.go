package dockercli

import (
	"encoding/json"
	"regexp"

	"github.com/docker/docker/api/types"
)

// Rewriting `localhost` in a healthcheck, and nowhere else (#7).
//
// R1 §Issue 7: a probe copied verbatim went unhealthy on the target while the
// application served perfectly. On Docker 29.7.2 the container's /etc/hosts
// resolves `localhost` to `::1` FIRST; on the source's Docker 24.0.2 it resolved
// to `127.0.0.1`. Wiki.js binds IPv4 only, so the probe connected to [::1]:3000
// and was refused. "Identical config, opposite result — purely from the engine
// version gap."
//
// It is not cosmetic: `depends_on: condition: service_healthy` never fires, so
// dependent services never start, and restart policies and monitoring react to a
// failure that is not one. The application is fine; the orchestration around it
// is broken.
//
// The blast radius is deliberately one field. `localhost` in an environment
// variable or a command may be load-bearing in ways this cannot see — an app that
// binds to it, a URL it publishes — and rewriting those would be introducing
// configuration the source does not have. The healthcheck is the one place the
// measurement covers.

// localhostInProbe matches `localhost` only as a WHOLE host token — bounded by a
// character that cannot appear inside a hostname.
//
// `\b` is not enough, measured: it treats `-` and `.` as boundaries, so
// `my-localhost-proxy` matches and would be rewritten to `my-127.0.0.1-proxy`, a
// different host that was never the one this rule is about. The delimiters are
// spelled out instead, and the two around a match are preserved by the
// replacement.
var localhostInProbe = regexp.MustCompile(`(^|[^A-Za-z0-9._-])localhost($|[^A-Za-z0-9._-])`)

// localhostReplacement keeps whatever delimited the token.
const localhostReplacement = "${1}127.0.0.1${2}"

// rewriteLocalhostTokens replaces every whole `localhost` token.
//
// Looped because a match consumes its trailing delimiter, so two tokens sharing
// one would otherwise leave the second behind. Bounded so a pathological string
// cannot spin.
func rewriteLocalhostTokens(s string) string {
	for i := 0; i < 8; i++ {
		next := localhostInProbe.ReplaceAllString(s, localhostReplacement)
		if next == s {
			return s
		}
		s = next
	}
	return s
}

// HealthcheckUsesLocalhost reports whether a recorded probe would be affected.
//
// The rule applies only when the probe actually says `localhost` — R5 §2's
// control run notes the rule did not fire partly because "no probe used
// localhost". Asking first is what keeps it from being applied gratuitously.
func HealthcheckUsesLocalhost(inspectJSON []byte) bool {
	var insp types.ContainerJSON
	if json.Unmarshal(inspectJSON, &insp) != nil || insp.Config == nil || insp.Config.Healthcheck == nil {
		return false
	}
	for _, part := range insp.Config.Healthcheck.Test {
		if rewriteLocalhostTokens(part) != part {
			return true
		}
	}
	return false
}

// RewriteHealthcheckLocalhost replaces `localhost` with `127.0.0.1` inside
// Config.Healthcheck.Test, and touches nothing else.
//
// Returns the document unchanged, byte for byte, when there is nothing to do —
// so a caller that applies the rule when it does not apply cannot introduce
// churn by accident.
func RewriteHealthcheckLocalhost(inspectJSON []byte) ([]byte, int, error) {
	var insp types.ContainerJSON
	if err := json.Unmarshal(inspectJSON, &insp); err != nil {
		return inspectJSON, 0, err
	}
	if insp.Config == nil || insp.Config.Healthcheck == nil {
		return inspectJSON, 0, nil
	}
	changes := 0
	for i, part := range insp.Config.Healthcheck.Test {
		rewritten := rewriteLocalhostTokens(part)
		if rewritten != part {
			insp.Config.Healthcheck.Test[i] = rewritten
			changes++
		}
	}
	if changes == 0 {
		return inspectJSON, 0, nil
	}
	out, err := json.Marshal(insp)
	if err != nil {
		return inspectJSON, 0, err
	}
	return out, changes, nil
}
