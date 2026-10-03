package dockercli

import (
	"encoding/json"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
)

// Adding the healthcheck a database never had (#16).
//
// R2 §Issue 16: neither container defined one ("Healthcheck": null on both) and
// the app used `depends_on: db: condition: service_started`, which waits only
// for the container to EXIST — not for MariaDB to accept connections. "The stack
// survived on the app's internal wait-for-db loop, which is luck, not design."
//
// Writing a probe in is the one place this tool may add configuration the source
// does not have, and only because the operator asked for it: without a probe
// there is no signal for "is the restore actually up", and the restore gate
// falls back to "the process is running", which for a database is not an answer.

// HealthcheckDeclared reports whether this container's OWN configuration says
// anything about health.
//
// An explicit ["NONE"] counts: disabling the image's probe is a decision, and
// overriding a decision is not injection. An empty Test does not — that means
// "inherit whatever the image declares", which this container never chose.
func HealthcheckDeclared(inspectJSON []byte) bool {
	var insp types.ContainerJSON
	if json.Unmarshal(inspectJSON, &insp) != nil || insp.Config == nil || insp.Config.Healthcheck == nil {
		return false
	}
	return len(insp.Config.Healthcheck.Test) > 0
}

// ContainerImage returns the image reference the recorded container was created
// from — the tag its family is recognised by, which a digest-pinned manifest
// reference on its own no longer carries.
func ContainerImage(inspectJSON []byte) string {
	var insp types.ContainerJSON
	if json.Unmarshal(inspectJSON, &insp) != nil || insp.Config == nil {
		return ""
	}
	return insp.Config.Image
}

// SetHealthcheck writes a probe into a recorded inspect, and refuses to touch
// one that already declares any — including a weak one.
//
// The refusal is the #31 rule, not politeness: a probe the operator wrote is
// configuration the source has, and replacing it silently is the deviation that
// makes a clone stop being a copy. Step 19 reports weak probes; this only fills
// an absence.
func SetHealthcheck(inspectJSON []byte, probe container.HealthConfig) ([]byte, bool, error) {
	if HealthcheckDeclared(inspectJSON) {
		return inspectJSON, false, nil
	}
	var insp types.ContainerJSON
	if err := json.Unmarshal(inspectJSON, &insp); err != nil {
		return inspectJSON, false, err
	}
	if insp.Config == nil {
		return inspectJSON, false, nil
	}
	insp.Config.Healthcheck = &probe
	out, err := json.Marshal(insp)
	if err != nil {
		return inspectJSON, false, err
	}
	return out, true, nil
}
