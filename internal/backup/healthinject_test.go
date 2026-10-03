package backup

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"

	"dockback/internal/dockercli"
)

func TestInjectHealthcheck(t *testing.T) {
	t.Run("probe per family", func(t *testing.T) {
		cases := []struct {
			image   string
			env     []string
			want    string
			refused bool
		}{
			{image: "mariadb:11.4", want: "healthcheck.sh --connect --innodb_initialized"},
			{image: "linuxserver/mariadb", want: "--innodb_initialized"},
			{image: "postgres:16-alpine", want: "select 1"},
			{image: "tensorchord/pgvecto-rs:pg14-v0.2.0", want: "select 1"},
			{image: "redis:7-alpine", want: "grep -q '^PONG'"},
			{image: "redis:7", env: []string{"REDIS_PASSWORD=hunter2"}, want: `REDISCLI_AUTH="$REDIS_PASSWORD"`},
			// redis-cli reads REDISCLI_AUTH from its own environment, so the plain
			// assertion is already authenticated.
			{image: "valkey/valkey:8", env: []string{"REDISCLI_AUTH=hunter2"}, want: "redis-cli ping | grep"},

			// Refused: a probe that cannot pass would hold the restore's health
			// gate red and roll back a restore that worked.
			{image: "mysql:8", refused: true},       // healthcheck.sh is MariaDB's
			{image: "percona:8.0", refused: true},   // ditto
			{image: "mongo:7", refused: true},       // mongo vs mongosh, unmeasured
			{image: "nginx:1.27", refused: true},    // not a database at all
			{image: "ghcr.io/x/app", refused: true}, // ditto
			{image: "redis:7", env: []string{"REDIS_ARGS=--requirepass s3cret"}, refused: true},
		}
		for _, tc := range cases {
			probe, ok := injectableProbe(tc.image, tc.env)
			if tc.refused {
				if ok {
					t.Errorf("%s: injected %v, want no probe", tc.image, probe.Config.Test)
				}
				continue
			}
			if !ok {
				t.Errorf("%s: no probe offered", tc.image)
				continue
			}
			command, _ := healthcheckCommand(probe.Config.Test)
			if !strings.Contains(command, tc.want) {
				t.Errorf("%s: probe %q does not contain %q", tc.image, command, tc.want)
			}
			if probe.Asserts == "" {
				t.Errorf("%s: probe says nothing about what it asserts", tc.image)
			}
		}
	})

	t.Run("a missing tool disables the probe instead of pinning it red", func(t *testing.T) {
		for _, image := range []string{"mariadb:11", "postgres:16", "redis:7"} {
			probe, ok := injectableProbe(image, nil)
			if !ok {
				t.Fatalf("%s: no probe", image)
			}
			command, _ := healthcheckCommand(probe.Config.Test)
			if !strings.Contains(command, "|| exit 0") {
				t.Errorf("%s: probe %q has no missing-tool guard", image, command)
			}
		}
	})

	t.Run("the probe carries no secret, only a reference to one", func(t *testing.T) {
		probe, _ := injectableProbe("redis:7", []string{"REDIS_PASSWORD=hunter2"})
		command, _ := healthcheckCommand(probe.Config.Test)
		if strings.Contains(command, "hunter2") {
			t.Fatalf("probe embeds the password: %q", command)
		}
		probe, _ = injectableProbe("postgres:16", []string{"POSTGRES_PASSWORD=hunter2", "POSTGRES_USER=app"})
		if command, _ = healthcheckCommand(probe.Config.Test); strings.Contains(command, "hunter2") {
			t.Fatalf("probe embeds the password: %q", command)
		}
	})

	t.Run("the whole budget fits inside the restore health gate", func(t *testing.T) {
		probe, _ := injectableProbe("mariadb:11", nil)
		budget := probe.Config.StartPeriod + time.Duration(probe.Config.Retries)*probe.Config.Interval
		gate := time.Duration(defaultRestoreHealthSeconds) * time.Second
		if budget >= gate {
			t.Fatalf("a probe that takes %s to go red cannot fit in a %s gate", budget, gate)
		}
	})

	t.Run("never over an existing probe, even a weak one", func(t *testing.T) {
		weak := inspectWithHealthcheck(t, &container.HealthConfig{Test: []string{"CMD-SHELL", "mysqladmin ping"}})
		out, changed, err := dockercli.SetHealthcheck(weak, container.HealthConfig{Test: []string{"CMD-SHELL", "true"}})
		if err != nil || changed {
			t.Fatalf("overwrote an existing probe (changed=%v, err=%v)", changed, err)
		}
		if string(out) != string(weak) {
			t.Fatal("document was rewritten")
		}
		// An explicit NONE is a decision to have no probe, not an absence.
		disabled := inspectWithHealthcheck(t, &container.HealthConfig{Test: []string{"NONE"}})
		if _, changed, _ := dockercli.SetHealthcheck(disabled, container.HealthConfig{Test: []string{"CMD-SHELL", "true"}}); changed {
			t.Fatal("overrode a deliberately disabled healthcheck")
		}
	})

	t.Run("written into an inspect that has none", func(t *testing.T) {
		none := inspectWithHealthcheck(t, nil)
		if dockercli.HealthcheckDeclared(none) {
			t.Fatal("an inspect with no healthcheck reports one")
		}
		probe, _ := injectableProbe("mariadb:11", nil)
		out, changed, err := dockercli.SetHealthcheck(none, probe.Config)
		if err != nil || !changed {
			t.Fatalf("SetHealthcheck: changed=%v err=%v", changed, err)
		}
		var back types.ContainerJSON
		if err := json.Unmarshal(out, &back); err != nil {
			t.Fatalf("re-read: %v", err)
		}
		if back.Config.Healthcheck == nil || back.Config.Healthcheck.Retries != probeRetries {
			t.Fatalf("probe not written back: %+v", back.Config.Healthcheck)
		}
	})

	t.Run("the advice and the injection are the same command", func(t *testing.T) {
		// One definition of "the correct form". Two copies drift, and the pair
		// that drifts here has DockBack recommending one probe and writing another.
		weak, found := weakHealthcheck([]string{"CMD-SHELL", "mariadb-admin ping"}, nil)
		if !found || !strings.Contains(weak.Correct, probeMariaDBShell) {
			t.Fatalf("advice %q is not the injected form", weak.Correct)
		}
		probe, _ := injectableProbe("mariadb:11", nil)
		command, _ := healthcheckCommand(probe.Config.Test)
		if !strings.Contains(command, probeMariaDBShell) {
			t.Fatalf("injected %q is not the recommended form", command)
		}
	})

	t.Run("compose advice escapes the dollar compose would eat", func(t *testing.T) {
		line := composeProbeLine(probeRedisAuthShell)
		if !strings.Contains(line, `$$REDIS_PASSWORD`) || strings.Contains(line, `"$REDIS`) {
			t.Fatalf("compose line would interpolate at parse time: %s", line)
		}
	})
}

// inspectWithHealthcheck builds a minimal recorded inspect.
func inspectWithHealthcheck(t *testing.T, probe *container.HealthConfig) []byte {
	t.Helper()
	insp := types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{Name: "/db"},
		Config:            &container.Config{Image: "mariadb:11", Healthcheck: probe},
	}
	out, err := json.Marshal(insp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}
