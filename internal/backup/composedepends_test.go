package backup

import (
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"

	yaml "go.yaml.in/yaml/v3"
)

// R2 §Issue 16 — neither container defined a healthcheck and the app used
// `depends_on: db: condition: service_started`, which "waits only for the
// container to exist — not for MariaDB to accept connections. The stack survived
// on the app's internal wait-for-db loop, which is luck, not design."
//
// DockBack's reconstruction emitted no healthchecks and no depends_on at all, so
// the file it wrote could not even describe the race, let alone the fix.
func TestComposeDependsOnConditions(t *testing.T) {
	// conditionOf reads what one service's dependency on another says.
	conditionOf := func(t *testing.T, merged []byte, service, dep string) (string, bool) {
		t.Helper()
		var doc struct {
			Services map[string]struct {
				DependsOn map[string]struct {
					Condition string `yaml:"condition"`
				} `yaml:"depends_on"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(merged, &doc); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, merged)
		}
		entry, ok := doc.Services[service].DependsOn[dep]
		return entry.Condition, ok
	}

	t.Run("a probe-less DB keeps service_started, because service_healthy would never start", func(t *testing.T) {
		app := serviceDoc(t, "bookstack", "lscr.io/linuxserver/bookstack:latest", nil, "db:service_started:false", []string{"db", "bookstack"})
		db := serviceDoc(t, "db", "lscr.io/linuxserver/mariadb:latest", nil, "", []string{"db", "bookstack"})
		merged, promoted, err := mergeComposeDocs([][]byte{app, db})
		if err != nil {
			t.Fatal(err)
		}
		if promoted != 0 {
			t.Fatalf("promoted %d conditions onto a service with no probe", promoted)
		}
		if got, ok := conditionOf(t, merged, "bookstack", "db"); !ok || got != conditionStarted {
			t.Fatalf("condition = %q (present=%v), want %q", got, ok, conditionStarted)
		}
	})

	t.Run("a DB with a probe gets service_healthy", func(t *testing.T) {
		probe, _ := injectableProbe("lscr.io/linuxserver/mariadb:latest", nil)
		app := serviceDoc(t, "bookstack", "lscr.io/linuxserver/bookstack:latest", nil, "db:service_started:false", []string{"db", "bookstack"})
		db := serviceDoc(t, "db", "lscr.io/linuxserver/mariadb:latest", &probe.Config, "", []string{"db", "bookstack"})
		merged, promoted, err := mergeComposeDocs([][]byte{app, db})
		if err != nil {
			t.Fatal(err)
		}
		if promoted != 1 {
			t.Fatalf("promoted %d, want 1", promoted)
		}
		if got, _ := conditionOf(t, merged, "bookstack", "db"); got != conditionHealthy {
			t.Fatalf("condition = %q, want %q", got, conditionHealthy)
		}
		// And the probe itself is in the file — a compose file that omits the
		// healthcheck recreates the race on the next `up`.
		for _, want := range []string{"healthcheck:", "innodb_initialized", "interval: 10s", "retries: 12", "start_period: 40s"} {
			if !strings.Contains(string(merged), want) {
				t.Errorf("merged compose is missing %q:\n%s", want, merged)
			}
		}
	})

	t.Run("only data-tier dependencies are promoted", func(t *testing.T) {
		// A web app with a probe is not a reason to make everything wait on its
		// readiness — that is a much larger change to how the project starts than
		// the evidence asks for.
		probe := container.HealthConfig{Test: []string{"CMD-SHELL", "curl -f http://127.0.0.1/"}}
		worker := serviceDoc(t, "worker", "ghcr.io/x/worker:1", nil, "web:service_started:false", []string{"web", "worker"})
		web := serviceDoc(t, "web", "ghcr.io/x/web:1", &probe, "", []string{"web", "worker"})
		merged, promoted, err := mergeComposeDocs([][]byte{worker, web})
		if err != nil {
			t.Fatal(err)
		}
		if promoted != 0 {
			t.Fatalf("promoted %d conditions onto an application", promoted)
		}
		if got, _ := conditionOf(t, merged, "worker", "web"); got != conditionStarted {
			t.Fatalf("condition = %q, want it untouched", got)
		}
	})

	t.Run("a recorded condition that is not a race is left alone", func(t *testing.T) {
		probe, _ := injectableProbe("postgres:16", nil)
		app := serviceDoc(t, "app", "ghcr.io/x/app:1", nil, "db:service_completed_successfully:false", []string{"db", "app"})
		db := serviceDoc(t, "db", "postgres:16", &probe.Config, "", []string{"db", "app"})
		merged, _, err := mergeComposeDocs([][]byte{app, db})
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := conditionOf(t, merged, "app", "db"); got != "service_completed_successfully" {
			t.Fatalf("condition = %q, want it preserved verbatim", got)
		}
	})

	t.Run("a dependency on a service that is not in the file is dropped", func(t *testing.T) {
		// Compose refuses to start a project with an undefined dependency, so a
		// member whose live container could not be read costs its own entry, not
		// the whole file's usability.
		app := serviceDoc(t, "app", "ghcr.io/x/app:1", nil, "db:service_started:false,gone:service_started:false", []string{"db", "app", "gone"})
		db := serviceDoc(t, "db", "postgres:16", nil, "", []string{"db", "app", "gone"})
		merged, _, err := mergeComposeDocs([][]byte{app, db})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := conditionOf(t, merged, "app", "gone"); ok {
			t.Fatalf("kept a dependency on an absent service:\n%s", merged)
		}
		if _, ok := conditionOf(t, merged, "app", "db"); !ok {
			t.Fatal("dropped the dependency that IS in the file")
		}
	})

	t.Run("a one-service reconstruction stays runnable on its own", func(t *testing.T) {
		// Its siblings are not in the file, and compose errors out on a dependency
		// it cannot resolve — so the standalone document declares none.
		solo := serviceDoc(t, "app", "ghcr.io/x/app:1", nil, "db:service_started:false", nil)
		if strings.Contains(string(solo), "depends_on") {
			t.Fatalf("standalone reconstruction references a service that is not in it:\n%s", solo)
		}
	})
}

// serviceDoc builds one service's reconstruction the way the stack path does.
func serviceDoc(t *testing.T, service, image string, probe *container.HealthConfig, dependsOn string, siblingNames []string) []byte {
	t.Helper()
	var siblings map[string]bool
	if siblingNames != nil {
		siblings = map[string]bool{}
		for _, name := range siblingNames {
			siblings[name] = true
		}
	}
	labels := map[string]string{"com.docker.compose.service": service}
	if dependsOn != "" {
		labels["com.docker.compose.depends_on"] = dependsOn
	}
	insp := types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{Name: "/" + service},
		Config:            &container.Config{Image: image, Labels: labels, Healthcheck: probe},
	}
	doc, err := composeFromInspect(insp, image, nil, siblings, nil)
	if err != nil {
		t.Fatalf("composeFromInspect(%s): %v", service, err)
	}
	return doc
}
