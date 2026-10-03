package dockercli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
)

func inspectWithProbe(t *testing.T, test []string, env []string, cmd []string) []byte {
	t.Helper()
	insp := types.ContainerJSON{Config: &container.Config{Env: env, Cmd: cmd}}
	if test != nil {
		insp.Config.Healthcheck = &container.HealthConfig{Test: test}
	}
	raw, err := json.Marshal(insp)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRewriteHealthcheckLocalhost(t *testing.T) {
	t.Run("R1 §7's probe is rewritten", func(t *testing.T) {
		// The measured case: on Docker 29 this connects to [::1]:3000 and is
		// refused, while the app serves perfectly on IPv4.
		in := inspectWithProbe(t, []string{"CMD-SHELL", "wget --no-verbose --tries=1 --spider http://localhost:3000"}, nil, nil)
		out, changes, err := RewriteHealthcheckLocalhost(in)
		if err != nil || changes != 1 {
			t.Fatalf("changes=%d err=%v", changes, err)
		}
		var insp types.ContainerJSON
		if json.Unmarshal(out, &insp) != nil {
			t.Fatal("unreadable result")
		}
		got := insp.Config.Healthcheck.Test[1]
		if !strings.Contains(got, "http://127.0.0.1:3000") || strings.Contains(got, "localhost") {
			t.Errorf("probe = %q", got)
		}
	})

	t.Run("the blast radius is one field", func(t *testing.T) {
		// localhost in an env var or a command may be load-bearing in ways this
		// cannot see; rewriting it would be introducing configuration the source
		// does not have.
		in := inspectWithProbe(t,
			[]string{"CMD", "curl", "-f", "http://localhost/health"},
			[]string{"APP_URL=http://localhost:3000", "DB_HOST=localhost"},
			[]string{"serve", "--bind", "localhost"})
		out, changes, err := RewriteHealthcheckLocalhost(in)
		if err != nil || changes != 1 {
			t.Fatalf("changes=%d err=%v", changes, err)
		}
		var insp types.ContainerJSON
		if json.Unmarshal(out, &insp) != nil {
			t.Fatal("unreadable")
		}
		for _, e := range insp.Config.Env {
			if !strings.Contains(e, "localhost") {
				t.Errorf("the environment must be untouched: %q", e)
			}
		}
		if !strings.Contains(strings.Join(insp.Config.Cmd, " "), "localhost") {
			t.Errorf("the command must be untouched: %v", insp.Config.Cmd)
		}
	})

	t.Run("nothing to do leaves the document byte-identical", func(t *testing.T) {
		for _, in := range []([]byte){
			inspectWithProbe(t, []string{"CMD", "curl", "-f", "http://127.0.0.1/health"}, nil, nil),
			inspectWithProbe(t, nil, []string{"A=localhost"}, nil),
			inspectWithProbe(t, []string{"NONE"}, nil, nil),
		} {
			out, changes, err := RewriteHealthcheckLocalhost(in)
			if err != nil || changes != 0 {
				t.Fatalf("changes=%d err=%v", changes, err)
			}
			if string(out) != string(in) {
				t.Error("an unapplied rewrite must not churn the document")
			}
		}
	})

	t.Run("only whole host tokens match", func(t *testing.T) {
		in := inspectWithProbe(t, []string{"CMD-SHELL", "check --db localhostdb --proxy my-localhost-proxy"}, nil, nil)
		_, changes, _ := RewriteHealthcheckLocalhost(in)
		if changes != 0 {
			t.Error("localhostdb is not localhost")
		}
	})

	t.Run("asking first is what keeps the rule from being gratuitous", func(t *testing.T) {
		if !HealthcheckUsesLocalhost(inspectWithProbe(t, []string{"CMD-SHELL", "wget http://localhost:3000"}, nil, nil)) {
			t.Error("this probe uses localhost")
		}
		if HealthcheckUsesLocalhost(inspectWithProbe(t, []string{"CMD-SHELL", "wget http://127.0.0.1:3000"}, nil, nil)) {
			t.Error("this one does not")
		}
		if HealthcheckUsesLocalhost(inspectWithProbe(t, nil, []string{"X=http://localhost"}, nil)) {
			t.Error("a localhost in the environment is not a probe")
		}
	})

	t.Run("two tokens sharing a delimiter are both rewritten", func(t *testing.T) {
		// A match consumes its trailing delimiter, so a single pass would leave
		// the second behind.
		in := inspectWithProbe(t, []string{"CMD-SHELL", "wget http://localhost:1 || wget http://localhost:2"}, nil, nil)
		out, changes, err := RewriteHealthcheckLocalhost(in)
		if err != nil || changes != 1 {
			t.Fatalf("changes=%d err=%v", changes, err)
		}
		if strings.Contains(string(out), "localhost") {
			t.Errorf("both tokens must be rewritten: %s", out)
		}
	})

	t.Run("a malformed document is refused, not mangled", func(t *testing.T) {
		if _, _, err := RewriteHealthcheckLocalhost([]byte("not json")); err == nil {
			t.Error("want an error")
		}
	})
}
