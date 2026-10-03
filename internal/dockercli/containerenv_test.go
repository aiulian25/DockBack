package dockercli

import (
	"encoding/json"
	"strings"
	"testing"
)

func inspectWithEnv(t *testing.T, env ...string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"Config": map[string]any{"Env": env}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func envOf(t *testing.T, b []byte) []string {
	t.Helper()
	var v struct {
		Config struct{ Env []string }
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v.Config.Env
}

// TestSetContainerEnvReplaces: an existing key is updated in place, and nothing
// else in the environment is disturbed — the rest of it holds the credentials the
// app needs to start.
func TestSetContainerEnvReplaces(t *testing.T) {
	in := inspectWithEnv(t, "APP_URL=https://old.example", "DB_PASSWORD=secret", "TZ=UTC")
	out, changed, err := SetContainerEnv(in, "APP_URL", "https://new.example")
	if err != nil || !changed {
		t.Fatalf("expected a change, got changed=%v err=%v", changed, err)
	}
	env := envOf(t, out)
	if len(env) != 3 {
		t.Fatalf("the environment must keep its other entries, got %v", env)
	}
	if env[0] != "APP_URL=https://new.example" {
		t.Errorf("APP_URL not updated in place: %v", env)
	}
	if env[1] != "DB_PASSWORD=secret" || env[2] != "TZ=UTC" {
		t.Errorf("unrelated entries were disturbed: %v", env)
	}
}

// TestSetContainerEnvAdds: a key the container never declared is added. "The app
// does not currently set this" is exactly the state on a first move.
func TestSetContainerEnvAdds(t *testing.T) {
	in := inspectWithEnv(t, "TZ=UTC")
	out, changed, err := SetContainerEnv(in, "HOMEPAGE_ALLOWED_HOSTS", "nas:3000")
	if err != nil || !changed {
		t.Fatalf("expected a change, got changed=%v err=%v", changed, err)
	}
	env := envOf(t, out)
	if len(env) != 2 || env[1] != "HOMEPAGE_ALLOWED_HOSTS=nas:3000" {
		t.Errorf("expected the key appended, got %v", env)
	}
}

// TestSetContainerEnvNoop: setting a key to what it already holds must report no
// change, so a re-run converges and the log stays quiet.
func TestSetContainerEnvNoop(t *testing.T) {
	in := inspectWithEnv(t, "APP_URL=https://same.example")
	_, changed, err := SetContainerEnv(in, "APP_URL", "https://same.example")
	if err != nil || changed {
		t.Errorf("setting an unchanged value must be a no-op, got changed=%v err=%v", changed, err)
	}
	if _, changed, _ := SetContainerEnv(in, "", "x"); changed {
		t.Error("an empty key must change nothing")
	}
}

// TestContainerEnvValue reads one NAMED key. It must not become a way to dump an
// environment full of passwords into a run log.
func TestContainerEnvValue(t *testing.T) {
	in := inspectWithEnv(t, "APP_URL=https://x.example", "DB_PASSWORD=secret")
	if got := ContainerEnvValue(in, "APP_URL"); got != "https://x.example" {
		t.Errorf("got %q", got)
	}
	if got := ContainerEnvValue(in, "MISSING"); got != "" {
		t.Errorf("an absent key must read empty, got %q", got)
	}
	if got := ContainerEnvValue([]byte("{not json"), "APP_URL"); got != "" {
		t.Errorf("unparseable inspect must read empty, got %q", got)
	}
}

// TestAddToEnvList is the one that protects against a self-inflicted outage:
// the accepted-hosts list is a SET, and replacing it with a single new entry
// would lock the app out at every address it currently answers on.
func TestAddToEnvList(t *testing.T) {
	got, changed := AddToEnvList("home.example.org,10.168.1.10:3000", "10.168.1.99:3000")
	if !changed {
		t.Fatal("a new host must be added")
	}
	if !strings.Contains(got, "home.example.org") || !strings.Contains(got, "10.168.1.10:3000") {
		t.Errorf("existing entries must survive — the app still answers on them: %q", got)
	}
	if !strings.HasSuffix(got, "10.168.1.99:3000") {
		t.Errorf("the new host must be appended, got %q", got)
	}

	// Already present: converge, don't grow the list on every restore.
	if _, changed := AddToEnvList("a.example,b.example", "b.example"); changed {
		t.Error("an entry already present must not be added again")
	}
	if _, changed := AddToEnvList("a.example", "A.EXAMPLE"); changed {
		t.Error("host comparison must be case-insensitive")
	}

	// Empty current value: becomes a one-entry list, not a leading comma.
	got, changed = AddToEnvList("", "only.example")
	if !changed || got != "only.example" {
		t.Errorf("got %q (changed=%v), want a clean single entry", got, changed)
	}
	// Stray whitespace and empty segments are normalised away.
	got, _ = AddToEnvList(" a.example , , b.example ", "c.example")
	if got != "a.example,b.example,c.example" {
		t.Errorf("expected a normalised list, got %q", got)
	}
	if _, changed := AddToEnvList("a.example", "   "); changed {
		t.Error("an empty entry must change nothing")
	}
}
