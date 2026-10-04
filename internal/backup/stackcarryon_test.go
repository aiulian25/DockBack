package backup

import (
	"errors"
	"strings"
	"testing"
)

func member(service string, dependsOn ...string) *stackService {
	return &stackService{service: service, man: &Manifest{DependsOn: dependsOn}}
}

// In the 2026-10-04 recovery a one-shot helper failed first and ended the whole stack
// restore: six real services were never attempted. A failure now only stops
// what depends on it.
func TestOnlyDependentsOfAFailureAreSkipped(t *testing.T) {
	failed := map[string]error{"arr-bootstrap": errors.New("read-only file system")}
	skipped := map[string]string{}

	for _, s := range []*stackService{
		member("gluetun"),
		member("qbittorrent", "gluetun"),
		member("sonarr"),
		member("radarr"),
	} {
		if dep := failedDependency(s, failed, skipped); dep != "" {
			t.Errorf("%s must be restored — it does not depend on the failed helper, got %q", s.service, dep)
		}
	}

	// Something that DOES depend on the failure is skipped, and the skip carries
	// down the chain.
	if dep := failedDependency(member("seeder", "arr-bootstrap"), failed, skipped); dep != "arr-bootstrap" {
		t.Errorf("a direct dependent must be skipped, got %q", dep)
	}
	skipped["seeder"] = "arr-bootstrap"
	if dep := failedDependency(member("reporter", "seeder"), failed, skipped); dep != "seeder" {
		t.Errorf("a dependent of a skipped service must be skipped too, got %q", dep)
	}
}

func TestTheIncompleteStackErrorSaysWhatCameBack(t *testing.T) {
	err := stackIncompleteError(7,
		map[string]error{"arr-bootstrap": errors.New("boom")},
		map[string]string{"seeder": "arr-bootstrap"})
	msg := err.Error()
	for _, want := range []string{"5 of 7 service(s) restored", "failed: arr-bootstrap", "skipped because a dependency failed: seeder"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
}

// The arr helper's container carried no depends_on label although its compose
// declared `depends_on: [sonarr, radarr]`, so the order fell back to names and
// it went first. The stack's own compose file is the fallback, in both forms
// Compose accepts.
func TestDependsOnIsReadFromTheStacksOwnCompose(t *testing.T) {
	compose := []byte(`
services:
  arr-bootstrap:
    image: alpine
    depends_on: [sonarr, radarr]
  qbittorrent:
    image: qbit
    depends_on:
      gluetun:
        condition: service_healthy
  gluetun:
    image: gluetun
  sonarr:
    image: sonarr
`)
	got := dependsOnDeclaredIn(compose)
	if strings.Join(got["arr-bootstrap"], ",") != "sonarr,radarr" {
		t.Errorf("list form: arr-bootstrap depends_on = %v", got["arr-bootstrap"])
	}
	if strings.Join(got["qbittorrent"], ",") != "gluetun" {
		t.Errorf("map form: qbittorrent depends_on = %v", got["qbittorrent"])
	}
	if len(got["gluetun"]) != 0 {
		t.Errorf("a service with no depends_on must stay without, got %v", got["gluetun"])
	}
	if dependsOnDeclaredIn([]byte("not: [valid")) != nil {
		t.Error("an unreadable compose file must give nothing, not a guess")
	}
}

func TestTheComposeFillsOnlyWhatTheLabelsDidNotRecord(t *testing.T) {
	svcs := map[string]*stackService{
		"arr-bootstrap": member("arr-bootstrap"),
		"web":           member("web", "db"), // recorded by its label: the label wins
		"sonarr":        member("sonarr"),
	}
	filled := fillMissingDependsOn(svcs, map[string][]string{
		"arr-bootstrap": {"sonarr", "radarr"},
		"web":           {"something-else"},
	})
	if strings.Join(filled, ",") != "arr-bootstrap" {
		t.Errorf("filled %v, want only arr-bootstrap", filled)
	}
	if strings.Join(svcs["web"].man.DependsOn, ",") != "db" {
		t.Errorf("a recorded depends_on must not be overwritten, got %v", svcs["web"].man.DependsOn)
	}

	// And with it filled, the order puts the helper after what it needs.
	svcs["radarr"] = member("radarr")
	order := topoOrder(svcs)
	position := map[string]int{}
	for i, s := range order {
		position[s.service] = i
	}
	if position["arr-bootstrap"] < position["sonarr"] || position["arr-bootstrap"] < position["radarr"] {
		t.Errorf("the helper must come after sonarr and radarr, order = %v", position)
	}
}
