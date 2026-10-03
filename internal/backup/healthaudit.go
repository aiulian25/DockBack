package backup

import (
	"regexp"
	"strings"

	"github.com/docker/docker/api/types"

	"dockback/internal/store"
)

// Probes whose success condition is satisfied by the failure state (#35).
//
// PLAYBOOK §7.5: "the most common defect found in EXISTING stacks across four
// migrations was not data loss — it was assurance that isn't assurance."
//
// The measured case: `redis-cli ping` exits 0 for server-side errors and
// non-zero only for connection failures, so against a --requirepass server it
// prints `NOAUTH Authentication required.` and exits 0. The container reports
// healthy with FailingStreak 0, and `depends_on: condition: service_healthy`
// gates on a meaningless green. It is "a liveness probe masquerading as a
// readiness probe".
//
// This matters to DockBack specifically because its own restore gate consumes
// container health verbatim — so a probe that cannot fail flows a false green
// through the gate that exists to catch a bad restore.
//
// Reported, never rewritten. Changing a probe in the restored container would be
// introducing configuration the source does not have, which is #31's own
// mistake; and the active audit §7.5 recommends — break the dependency, confirm
// the probe goes red — stops services, which is not a thing a backup tool does
// unasked.

// WeakProbe is a healthcheck that cannot fail, and the form that can.
type WeakProbe struct {
	// Why the probe passes when the thing it checks is broken.
	Why string
	// Correct is the replacement, copy-pasteable into a compose file.
	Correct string
	// Working marks a probe that is weak in principle but sound in THIS
	// deployment — the failure mode it cannot see is unreachable here. Reported
	// as a latent risk, never as a defect: PLAYBOOK §7.5, "fix what is broken;
	// report what is merely weak."
	Working bool
}

// curlFailFlag matches curl's -f / --fail in any flag cluster (-sf, -fsS, …).
var curlFailFlag = regexp.MustCompile(`(^|\s)-(-fail\b|[A-Za-z]*f)`)

// redisPasswordKeys are the environment variables that mean this Redis requires
// authentication — which is what turns a lenient probe from weak into blind.
var redisPasswordKeys = []string{"REDIS_PASSWORD", "REDISCLI_AUTH", "REDIS_ARGS"}

// healthcheckCommand renders a recorded Test into the command it actually runs.
// Docker records ["CMD-SHELL", "…"] or ["CMD", "bin", "arg"], and "NONE" or an
// empty list mean there is no probe at all.
func healthcheckCommand(test []string) (string, bool) {
	if len(test) == 0 {
		return "", false
	}
	switch strings.ToUpper(strings.TrimSpace(test[0])) {
	case "NONE":
		return "", false
	case "CMD", "CMD-SHELL":
		test = test[1:]
	}
	joined := strings.TrimSpace(strings.Join(test, " "))
	if joined == "" {
		return "", false
	}
	return joined, true
}

// redisRequiresAuth reports whether this container's Redis wants a password —
// from its environment, or from a requirepass on the probe or command line.
func redisRequiresAuth(command string, env []string) bool {
	if strings.Contains(strings.ToLower(command), "requirepass") {
		return true
	}
	values := envValues(env)
	for _, key := range redisPasswordKeys {
		if strings.TrimSpace(values[key]) != "" {
			return true
		}
	}
	return false
}

// weakHealthcheck judges one recorded probe.
//
// Every pattern here was measured in a real stack, and every replacement is the
// one PLAYBOOK §7.5 states. Nothing is inferred from a probe's shape alone: a
// command this does not recognise is not called weak, because a false accusation
// about a healthcheck teaches an operator to ignore the report.
func weakHealthcheck(test, env []string) (WeakProbe, bool) {
	command, ok := healthcheckCommand(test)
	if !ok {
		return WeakProbe{}, false
	}
	lower := strings.ToLower(command)

	switch {
	// Redis: exit 0 on NOAUTH and on a wrong password. Only asserting on the
	// CONTENT of the reply can tell those from a real PONG.
	case strings.Contains(lower, "redis-cli") && strings.Contains(lower, "ping") && !strings.Contains(lower, "grep"):
		if !redisRequiresAuth(command, env) {
			return WeakProbe{
				Working: true,
				Why:     "redis-cli exits 0 for server-side errors, so this probe would report healthy against a server that is refusing every command. This Redis has no password set, so it genuinely answers PONG and the probe works today — it is a latent risk rather than a defect, and it is kept exactly as it is",
				Correct: composeProbeLine(probeRedisShell),
			}, true
		}
		return WeakProbe{
			Why:     "this Redis requires a password and redis-cli exits 0 on `NOAUTH Authentication required.`, so the probe reports healthy against a server that is refusing every command. Anything waiting on service_healthy starts against a Redis it cannot use",
			Correct: composeProbeLine(probeRedisAuthShell),
		}, true

	// PostgreSQL: pg_isready proves a listener accepted a connection, not that
	// the database will answer a query.
	case strings.Contains(lower, "pg_isready") && !strings.Contains(lower, "psql"):
		return WeakProbe{
			Why:     "pg_isready checks that a connection is accepted, not that the database can answer — it reports ready while the server is still refusing queries",
			Correct: composeProbeLine(probePostgresShell),
		}, true

	// MariaDB: ping answers true while InnoDB is still recovering.
	case (strings.Contains(lower, "mariadb-admin") || strings.Contains(lower, "mysqladmin")) && strings.Contains(lower, "ping"):
		return WeakProbe{
			Why:     "mariadb-admin ping answers true while InnoDB is still recovering, so the container reports healthy before the database can be used",
			Correct: composeProbeLine(probeMariaDBShell),
		}, true

	// HTTP: without -f, curl exits 0 on a 500.
	case strings.Contains(lower, "curl") && !curlFailFlag.MatchString(command):
		return WeakProbe{
			Why:     "curl without -f exits 0 for any response it received, including a 500 — so the probe passes while the application is returning errors",
			Correct: composeProbeLine(probeHTTPShell),
		}, true
	}
	return WeakProbe{}, false
}

// describeWeakProbe is the finding's message: what is wrong, and the line to
// paste in its place.
func (w WeakProbe) describeWeakProbe(command string) string {
	if w.Working {
		return "This container's healthcheck is `" + command + "`. " + w.Why +
			". It is left exactly as it is — changing a working probe would be introducing configuration this deployment does not have. If you ever put a password on this server, change the probe at the same time:\n    " + w.Correct
	}
	return "This container's healthcheck is `" + command + "`, and it cannot fail: " + w.Why +
		". A probe that cannot fail is worse than none, because everything downstream — including this tool's own post-restore health gate — treats its green as proof. Replace it with:\n    " + w.Correct
}

// auditHealthcheck records what this container's probe would and would not
// notice, at capture, while the environment that decides it is right there.
func (e *Engine) auditHealthcheck(man *Manifest, logID string, insp types.ContainerJSON) {
	if insp.Config == nil || insp.Config.Healthcheck == nil {
		return
	}
	command, ok := healthcheckCommand(insp.Config.Healthcheck.Test)
	if !ok {
		return
	}
	weak, found := weakHealthcheck(insp.Config.Healthcheck.Test, insp.Config.Env)
	if !found {
		return
	}
	severity := FindingWarn
	if weak.Working {
		severity = FindingInfo
	}
	e.addFinding(man, logID, findingHealthcheckCannotFail, severity, "", weak.describeWeakProbe(command))
}

// warnIfGateTrustsWeakProbe says, once, that the green the restore gate is about
// to accept may not mean anything.
//
// The gate's behaviour is deliberately unchanged. Rewriting the probe is #31's
// mistake, and refusing a restore because the application's own healthcheck is
// weak would be refusing recovery over a defect that predates the backup.
func (e *Engine) warnIfGateTrustsWeakProbe(b *store.Backup, insp types.ContainerJSON) {
	if insp.Config == nil || insp.Config.Healthcheck == nil {
		return
	}
	weak, found := weakHealthcheck(insp.Config.Healthcheck.Test, insp.Config.Env)
	if !found || weak.Working {
		return
	}
	command, _ := healthcheckCommand(insp.Config.Healthcheck.Test)
	e.logf(b.ID, "WARN", "This container's health is judged by `%s`, which cannot fail: %s. The healthy verdict below rests on that probe, so treat it as 'the container started' rather than 'the application works' until the probe is replaced.",
		command, weak.Why)
}
