package backup

import (
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// The probe forms, in one place (#16 / #35).
//
// These strings are BOTH what Step 19 recommends when it finds a probe that
// cannot fail AND what Step 23 writes in when there is no probe at all. One
// definition, because two copies of "the correct form" drift, and the pair that
// drifts here would have DockBack recommending one command and injecting
// another.
//
// Every form asserts on CONTENT or on a real query, never on the exit code of a
// lenient client — PLAYBOOK §7.5: "the fix must assert on content, not exit
// code, wherever the CLI is lenient."
const (
	// probeMariaDBShell — ships inside the MariaDB image. `mariadb-admin ping`
	// returns success while InnoDB is still recovering, so it is the wrong probe.
	probeMariaDBShell = `healthcheck.sh --connect --innodb_initialized`

	// probePostgresShell — pg_isready proves a listener accepted a connection;
	// only a query proves the database will answer one. The database is pinned to
	// `postgres` rather than POSTGRES_DB's own default because initdb always
	// creates it, whereas POSTGRES_USER's namesake database exists only when
	// POSTGRES_DB is unset.
	probePostgresShell = `pg_isready -q -U "${POSTGRES_USER:-postgres}" -d "${POSTGRES_DB:-postgres}" && psql -U "${POSTGRES_USER:-postgres}" -d "${POSTGRES_DB:-postgres}" -tAc 'select 1' >/dev/null`

	// probeRedisShell — redis-cli exits 0 for server-side errors, so the reply
	// itself has to be read.
	probeRedisShell = `redis-cli ping | grep -q '^PONG'`
	// probeRedisAuthShell — the same assertion with the password kept out of the
	// process list, as PLAYBOOK §7.5 writes it.
	probeRedisAuthShell = `REDISCLI_AUTH="$REDIS_PASSWORD" redis-cli ping | grep -q '^PONG'`

	// probeHTTPShell is advice only and is never injected: the port and the
	// endpoint are the application's, and guessing either produces a probe that
	// reports an application broken because DockBack asked it the wrong question.
	probeHTTPShell = `curl -f http://localhost:<port>/<a real endpoint>`
)

// The cadence R2 measured on the stack this fixes: "BookStack-DB Healthy →
// BookStack Starting, healthy in 15 s, no race." Kept as measured rather than
// invented — and the whole budget (start period plus every retry) stays inside
// the restore health gate's own five-minute default, so an injected probe
// cannot be the thing that runs the gate out of time.
const (
	probeInterval    = 10 * time.Second
	probeTimeout     = 5 * time.Second
	probeStartPeriod = 40 * time.Second
	probeRetries     = 12
)

// composeProbeLine renders a probe as the line an operator pastes into a compose
// file — `$` doubled, because compose interpolates it, and quotes escaped for
// the JSON array form.
func composeProbeLine(shell string) string {
	escaped := strings.ReplaceAll(shell, `$`, `$$`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `test: ["CMD-SHELL", "` + escaped + `"]`
}

// guardedProbe makes a probe self-disabling when its tool is not in the image.
//
// This matters only because DockBack injects blind: it recognises the family
// from the image NAME and cannot see what is inside. A probe whose command does
// not exist exits 127 forever, the container never reports healthy, and the
// restore gate rolls back a restore that actually worked — turning a helpful
// addition into data loss. Missing tool means the container is judged exactly as
// it was before the injection, which is the honest fallback.
func guardedProbe(tool, shell string) []string {
	return []string{"CMD-SHELL", "command -v " + tool + " >/dev/null 2>&1 || exit 0; " + shell}
}

// InjectableProbe is the probe DockBack will add to a database container that
// has none, and what it asserts.
type InjectableProbe struct {
	Config container.HealthConfig
	// Asserts is the operator-facing sentence: what this probe proves, so the
	// log says what was added rather than only that something was.
	Asserts string
}

// injectableProbe picks the probe for a database container, keyed off the same
// image-name detection the dump path uses.
//
// Returns false wherever DockBack would be guessing. Three engines are
// deliberately refused:
//
//   - MySQL and Percona: `healthcheck.sh` is a MariaDB script and does not exist
//     there, and every alternative needs a credential this may not have.
//   - MongoDB: the client is `mongo` on old images and `mongosh` on new ones,
//     and no migration in the evidence base measured either.
//   - a Redis whose password is reachable only through REDIS_ARGS: the probe
//     could not authenticate, so it would sit red forever.
//
// In each case the finding still fires. Saying "this has no healthcheck and I
// will not guess one" is useful; writing a probe that cannot pass is not.
func injectableProbe(image string, env []string) (InjectableProbe, bool) {
	switch detectDBEngine(image, nil) {
	case "mysql":
		if !strings.Contains(strings.ToLower(image), "mariadb") {
			return InjectableProbe{}, false
		}
		return probeFor(guardedProbe("healthcheck.sh", probeMariaDBShell),
			"the server accepts a connection AND InnoDB has finished initialising"), true

	case "postgres":
		return probeFor(guardedProbe("pg_isready", "export PGPASSWORD=\"$POSTGRES_PASSWORD\"; "+probePostgresShell),
			"the server answers a real query, not just that it accepted a connection"), true

	case "redis":
		shell, ok := redisProbeShell(env)
		if !ok {
			return InjectableProbe{}, false
		}
		return probeFor(guardedProbe("redis-cli", shell),
			"the server replies PONG — read from the reply itself, because redis-cli exits 0 on an error too"), true
	}
	return InjectableProbe{}, false
}

// redisProbeShell picks the Redis form this container's own environment can
// actually authenticate with, or false when nothing here can.
func redisProbeShell(env []string) (string, bool) {
	values := envValues(env)
	if strings.TrimSpace(values["REDIS_PASSWORD"]) != "" {
		return probeRedisAuthShell, true
	}
	// redis-cli reads REDISCLI_AUTH from its own environment, so the plain form
	// is already authenticated when the container sets it.
	if strings.TrimSpace(values["REDISCLI_AUTH"]) != "" {
		return probeRedisShell, true
	}
	if redisRequiresAuth("", env) {
		return "", false
	}
	return probeRedisShell, true
}

// probeFor stamps the shared cadence onto a test command.
func probeFor(test []string, asserts string) InjectableProbe {
	return InjectableProbe{
		Config: container.HealthConfig{
			Test:        test,
			Interval:    probeInterval,
			Timeout:     probeTimeout,
			StartPeriod: probeStartPeriod,
			Retries:     probeRetries,
		},
		Asserts: asserts,
	}
}

// reportMissingHealthcheck records a database that reports nothing better than
// "the process is running" — and what everything waiting on it therefore gets.
//
// Databases only. An application with no probe is a different problem with a
// different answer (see AppProfile.NoHealthcheck and migrationSettle), and a
// finding on every container that has no healthcheck would be a wall of lines
// nobody reads.
func (e *Engine) reportMissingHealthcheck(man *Manifest, logID, name string, insp types.ContainerJSON) {
	if insp.Config == nil || len(healthcheckTest(insp)) > 0 {
		return
	}
	if man.ImageConfig != nil && man.ImageConfig.Healthcheck {
		return // the image ships one; this container inherits it
	}
	if detectDBEngine(insp.Config.Image, nil) == "" {
		return
	}
	message := "This database container defines no healthcheck, so Docker reports nothing better than \"running\" about it — and `depends_on: condition: service_started` waits only for the container to exist, not for the database to accept connections. " +
		"Anything that depends on " + name + " starts against a server that may still be initialising; it survives on the application's own retry loop, which is luck rather than design."
	probe, canInject := injectableProbe(insp.Config.Image, insp.Config.Env)
	if canInject {
		command, _ := healthcheckCommand(probe.Config.Test)
		message += " Restoring this backup offers to add a probe that proves " + probe.Asserts + ", and the stack's reconstructed compose file then waits on it:\n    " + composeProbeLine(command)
	} else {
		message += " DockBack has no probe it can add for this image without guessing at a client or a credential it cannot see, so add one by hand — it must assert on a real query, not on a client's exit code."
	}
	e.addFinding(man, logID, findingHealthcheckMissing, FindingWarn, name, message)
}

// healthcheckTest returns the container's own recorded probe command, if it
// declared one at all.
func healthcheckTest(insp types.ContainerJSON) []string {
	if insp.Config == nil || insp.Config.Healthcheck == nil {
		return nil
	}
	return insp.Config.Healthcheck.Test
}

// applyHealthcheckInjection writes the probe in when the operator asked for it.
//
// Off by default and never over an existing probe, even a weak one: Step 19
// reports those, and replacing an operator's own probe is the deviation #31
// exists to stop. Every outcome is logged, including the ones that changed
// nothing — a restore that quietly did not do what the checkbox said is worse
// than one that says why.
func (e *Engine) applyHealthcheckInjection(b *store.Backup, man *Manifest, inspectBytes []byte, opts RestoreOptions) []byte {
	if !opts.InjectHealthchecks {
		return inspectBytes
	}
	if dockercli.HealthcheckDeclared(inspectBytes) {
		e.logf(b.ID, "INFO", "%s already defines its own healthcheck — left exactly as recorded. If it is one of the probes that cannot fail, this backup's findings say so.", b.TargetName)
		return inspectBytes
	}
	if man.ImageConfig != nil && man.ImageConfig.Healthcheck {
		e.logf(b.ID, "INFO", "%s has no healthcheck of its own but its image ships one, which it inherits — nothing added.", b.TargetName)
		return inspectBytes
	}
	// The container's own image tag first: man.Image is the RESOLVED reference,
	// which for a digest-pinned backup no longer contains the family name the
	// detection keys on.
	image := firstNonEmpty(dockercli.ContainerImage(inspectBytes), manifestImage(man, b))
	probe, ok := injectableProbe(image, dockercli.ContainerEnv(inspectBytes))
	if !ok {
		e.logf(b.ID, "INFO", "No healthcheck added to %s: DockBack has no probe for this image it can write without guessing at a client or a credential. A probe that can never pass would hold this restore's health gate red and roll back a restore that worked.", b.TargetName)
		return inspectBytes
	}
	out, changed, err := dockercli.SetHealthcheck(inspectBytes, probe.Config)
	if err != nil || !changed {
		e.logf(b.ID, "WARN", "Could not add a healthcheck to %s (%v) — the container keeps what this backup recorded, which is none", b.TargetName, err)
		return inspectBytes
	}
	command, _ := healthcheckCommand(probe.Config.Test)
	e.logf(b.ID, "INFO", "Added a healthcheck to %s as you asked — it proves %s: `%s`. The check below now waits for that instead of for the process to exist, and anything using `condition: service_healthy` waits with it.",
		b.TargetName, probe.Asserts, command)
	return out
}
