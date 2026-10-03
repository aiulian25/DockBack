package backup

import (
	"strings"
	"testing"
)

// PLAYBOOK §7.5's table, and its takeaway: "fix what is broken; report what is
// merely weak."
func TestWeakHealthcheck(t *testing.T) {
	for _, tc := range []struct {
		name        string
		test        []string
		env         []string
		wantFlagged bool
		wantWorking bool
		wantCorrect string
	}{
		{
			// The measured case: NOAUTH exits 0, so the container reports healthy
			// with FailingStreak 0 while refusing every command.
			name:        "redis-cli ping on a password-protected server cannot fail",
			test:        []string{"CMD-SHELL", "redis-cli ping || exit 1"},
			env:         []string{"REDIS_PASSWORD=hunter2"},
			wantFlagged: true, wantWorking: false,
			wantCorrect: "grep -q '^PONG'",
		},
		{
			// R5 §7.5: Immich's Redis has no requirepass, so the same probe
			// genuinely returns PONG. Kept verbatim — changing a working probe
			// would introduce configuration the source does not have.
			name:        "the same probe with no password is weak-but-working",
			test:        []string{"CMD-SHELL", "redis-cli ping || exit 1"},
			env:         []string{"TZ=Europe/London"},
			wantFlagged: true, wantWorking: true,
			wantCorrect: "grep -q '^PONG'",
		},
		{
			name:        "requirepass on the command line counts as a password",
			test:        []string{"CMD-SHELL", "redis-cli ping"},
			env:         []string{"REDIS_ARGS=--requirepass secret"},
			wantFlagged: true, wantWorking: false,
		},
		{
			// The correct form is not flagged at all.
			name: "a probe that asserts on the reply is sound",
			test: []string{"CMD-SHELL", `REDISCLI_AUTH="$REDIS_PASSWORD" redis-cli ping | grep -q '^PONG'`},
			env:  []string{"REDIS_PASSWORD=hunter2"},
		},
		{
			name:        "bare pg_isready proves a connection, not readiness",
			test:        []string{"CMD-SHELL", "pg_isready -U postgres"},
			wantFlagged: true,
			wantCorrect: "select 1",
		},
		{
			name: "pg_isready followed by a real query is sound",
			test: []string{"CMD-SHELL", "pg_isready && psql -U postgres -c 'select 1'"},
		},
		{
			name:        "mariadb-admin ping is true while InnoDB recovers",
			test:        []string{"CMD", "mariadb-admin", "ping", "-h", "localhost"},
			wantFlagged: true,
			wantCorrect: "healthcheck.sh --connect --innodb_initialized",
		},
		{
			name:        "mysqladmin ping is the same probe under its old name",
			test:        []string{"CMD-SHELL", "mysqladmin ping -h 127.0.0.1"},
			wantFlagged: true,
			wantCorrect: "healthcheck.sh --connect --innodb_initialized",
		},
		{
			name:        "curl without -f passes on a 500",
			test:        []string{"CMD-SHELL", "curl http://localhost:80/"},
			wantFlagged: true,
			wantCorrect: "curl -f",
		},
		{
			name: "curl -f is sound",
			test: []string{"CMD", "curl", "-f", "http://localhost:80/health"},
		},
		{
			// Flag clusters are how these are actually written.
			name: "curl -fsS is sound",
			test: []string{"CMD-SHELL", "curl -fsS http://localhost/health"},
		},
		{
			name: "curl --fail is sound",
			test: []string{"CMD-SHELL", "curl --fail http://localhost/health"},
		},
		{
			name: "no healthcheck at all is not a weak one",
			test: []string{"NONE"},
		},
		{
			name: "an empty probe yields nothing",
			test: nil,
		},
		{
			// A probe this does not recognise is NOT called weak: a false
			// accusation teaches an operator to ignore the report.
			name: "an unrecognised probe is left alone",
			test: []string{"CMD-SHELL", "/app/bin/selfcheck --deep"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			weak, flagged := weakHealthcheck(tc.test, tc.env)
			if flagged != tc.wantFlagged {
				t.Fatalf("flagged = %v, want %v (%+v)", flagged, tc.wantFlagged, weak)
			}
			if !flagged {
				return
			}
			if weak.Working != tc.wantWorking {
				t.Errorf("working = %v, want %v", weak.Working, tc.wantWorking)
			}
			if tc.wantCorrect != "" && !strings.Contains(weak.Correct, tc.wantCorrect) {
				t.Errorf("correct = %q, want it to contain %q", weak.Correct, tc.wantCorrect)
			}
			if weak.Why == "" || weak.Correct == "" {
				t.Error("a finding must say what is wrong AND what to paste instead")
			}
		})
	}

	t.Run("the message carries the probe verbatim and the replacement to paste", func(t *testing.T) {
		weak, _ := weakHealthcheck([]string{"CMD-SHELL", "redis-cli ping"}, []string{"REDIS_PASSWORD=x"})
		msg := weak.describeWeakProbe("redis-cli ping")
		if !strings.Contains(msg, "`redis-cli ping`") {
			t.Errorf("the operator must see their own probe: %s", msg)
		}
		if !strings.Contains(msg, `test: ["CMD-SHELL"`) {
			t.Errorf("the replacement must be a compose line: %s", msg)
		}
		if !strings.Contains(msg, "cannot fail") {
			t.Errorf("msg = %s", msg)
		}
	})

	t.Run("a weak-but-working probe says it is kept as it is", func(t *testing.T) {
		weak, _ := weakHealthcheck([]string{"CMD-SHELL", "redis-cli ping"}, nil)
		msg := weak.describeWeakProbe("redis-cli ping")
		if !strings.Contains(msg, "left exactly as it is") {
			t.Errorf("R5 §7.5: a working probe is kept verbatim: %s", msg)
		}
		if strings.Contains(msg, "cannot fail") {
			t.Errorf("a working probe is a latent risk, not a defect: %s", msg)
		}
	})

	t.Run("the command is rendered from either Docker form", func(t *testing.T) {
		if got, _ := healthcheckCommand([]string{"CMD", "curl", "-f", "http://x"}); got != "curl -f http://x" {
			t.Errorf("CMD form = %q", got)
		}
		if got, _ := healthcheckCommand([]string{"CMD-SHELL", "a | b"}); got != "a | b" {
			t.Errorf("CMD-SHELL form = %q", got)
		}
		if _, ok := healthcheckCommand([]string{"NONE"}); ok {
			t.Error("NONE is no probe")
		}
	})
}
