package dockercli

import (
	"errors"
	"strings"
	"testing"
)

// F178 — a logging driver the target host cannot provide must not cost a
// restore.
//
// Reported from a cross-host Wiki.js stack restore that died at the first
// service: `creating container "Wiki.js-DB": error looking up logging plugin db:
// plugin "db" not found`. The container was recreated with the driver it was
// captured with, the target daemon could not load it, and the whole stack
// restore ended there — over where stdout is written.
//
// This is the gate on the retry that drops the driver, so it has to be narrow:
// anything it does not clearly recognise must fail with the original error
// rather than be quietly reconfigured.
func TestLogDriverUnavailable(t *testing.T) {
	unavailable := []string{
		// The one that was reported, verbatim.
		`Error response from daemon: error looking up logging plugin db: plugin "db" not found`,
		`error looking up logging plugin loki: plugin "loki" not found`,
		// The other shapes the daemon uses for the same condition.
		`logger: no log driver named 'gelf' is registered`,
		`Error response from daemon: error creating logger: fluentd: connection refused`,
		`log driver "awslogs" is not supported`,
		`Error response from daemon: log driver syslog is not registered`,
	}
	for _, msg := range unavailable {
		if !logDriverUnavailable(errors.New(msg)) {
			t.Errorf("should be recognised as a logging-driver failure: %q", msg)
		}
	}

	// Everything else has to fall through. Retrying these without the log config
	// would either fail again with a message that no longer names the real cause,
	// or — worse — succeed for an unrelated reason and leave the operator
	// believing the logging driver was the problem.
	other := []string{
		`Error response from daemon: Conflict. The container name "/x" is already in use`,
		`Error response from daemon: No such image: postgres:16`,
		`Error response from daemon: network wikijs_default not found`,
		`Error response from daemon: invalid mount config for type "bind": bind source path does not exist`,
		`Error response from daemon: driver failed programming external connectivity on endpoint`,
		`Error response from daemon: could not select device driver "nvidia" with capabilities`,
		`context deadline exceeded`,
		"",
	}
	for _, msg := range other {
		var err error
		if msg != "" {
			err = errors.New(msg)
		}
		if logDriverUnavailable(err) {
			t.Errorf("must not be treated as a logging-driver failure: %q", msg)
		}
	}
}

// The message has to distinguish the two causes, because they need different
// fixes: a driver named in the backup is a problem on the machine the container
// came FROM, while nothing recorded means the daemon's own default on the
// machine it landed on is the broken one.
func TestQuotedOrNone(t *testing.T) {
	if got := quotedOrNone("db"); got != `"db"` {
		t.Errorf("a captured driver should be quoted plainly, got %q", got)
	}
	for _, empty := range []string{"", "   "} {
		got := quotedOrNone(empty)
		if !strings.Contains(got, "none recorded") || !strings.Contains(got, "default") {
			t.Errorf("nothing captured must point at the daemon's default, got %q", got)
		}
	}
}
