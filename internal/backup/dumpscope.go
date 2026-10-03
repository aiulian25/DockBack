package backup

import (
	"context"
	"fmt"
	"strings"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// What the dump could actually reach, recorded rather than assumed (#13).
//
// R2 §Issue 13: the environment declared a root password that had never applied,
// because the entrypoint only honours it when the data directory is empty at
// first start and this one predated the value. The dump succeeded — as the
// application user — and that is a materially different backup: it covers one
// schema and not `mysql.user`, which is "a real gap on a full-server restore".
//
// Two things follow, and neither is the dump failing:
//   - the manifest records the PRIVILEGE LEVEL captured, so a restore knows what
//     it is holding;
//   - the drift between declared and effective configuration is a finding,
//     because that is "exactly the sort of thing they want to learn before a
//     disaster, not during one".

// Dump scopes.
const (
	// DumpScopeServer — a privileged identity dumped the whole server.
	DumpScopeServer = "server"
	// dumpScopeSchemaPrefix — an application identity dumped only what it owns.
	dumpScopeSchemaPrefix = "schema:"
)

// privilegedDumpUsers are the identities whose dump covers the server rather
// than one application's own schema.
func privilegedDumpUser(user string) bool {
	switch strings.ToLower(strings.TrimSpace(user)) {
	case "root", "postgres":
		return true
	}
	return false
}

// dumpScopeFor turns the authenticated identity and what it could see into the
// scope this dump actually has.
func dumpScopeFor(user string, databases []string) string {
	if privilegedDumpUser(user) {
		return DumpScopeServer
	}
	if len(databases) == 0 {
		return dumpScopeSchemaPrefix + user
	}
	return dumpScopeSchemaPrefix + strings.Join(databases, ",")
}

// SchemaScoped reports whether a recorded scope is an application-user dump.
func SchemaScoped(scope string) (names string, yes bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(scope), dumpScopeSchemaPrefix)
	return rest, ok && rest != ""
}

// declaredRootRejected reports whether the environment declared a root password
// that did not turn out to be the identity used.
func declaredRootRejected(env []string, user string) bool {
	values := envValues(env)
	declared := firstNonEmpty(values["MYSQL_ROOT_PASSWORD"], values["MARIADB_ROOT_PASSWORD"])
	return strings.TrimSpace(declared) != "" && !privilegedDumpUser(user)
}

// recordDumpScope probes which credential actually authenticates, records the
// privilege level on the dump, and reports a declared credential that does not
// work.
//
// Runs beside the dump on the same exec channel. It never fails a backup: the
// dump has already succeeded by the time this is asked, and not knowing the
// scope is worth a missing field, not a lost backup.
func (e *Engine) recordDumpScope(ctx context.Context, cli *client.Client, containerID, engine string, env []string, man *Manifest, dump *DBDump, logID string) {
	switch engine {
	case "postgres":
		// The Postgres path connects as the declared superuser and refuses to
		// dump at all if that fails, so a dump that exists is a server dump.
		dump.DumpScope = DumpScopeServer
		return
	case "mysql":
	default:
		return
	}

	out, err := dockercli.ExecCapture(ctx, cli, containerID, MySQLAuthProbeCommand(env))
	if err != nil {
		return
	}
	user, databases, ok := parseAuthProbe(string(out))
	if !ok {
		return
	}
	dump.DumpScope = dumpScopeFor(user, databases)

	if names, schemaScoped := SchemaScoped(dump.DumpScope); schemaScoped {
		e.logf(logID, "INFO", "This dump was taken as %q, which reaches %s and not the server's own accounts and grants. It restores that data completely; users and privileges come from the restored container's own configuration.",
			user, names)
	}
	if !declaredRootRejected(env, user) {
		return
	}
	e.addFinding(man, logID, findingDeclaredCredentialInert, FindingWarn, "", fmt.Sprintf(
		"This container declares a root password that does not work — the backup authenticated as %q instead. The entrypoint only applies that variable when the data directory is empty at first start, so a value added or changed later never took effect: the declared configuration does not describe the running system. "+
			"The backup is complete for %s. What it cannot contain is the server's own accounts and grants, which only a privileged identity can read — so a full-server rebuild from this dump comes back with the databases and without the users.",
		user, strings.Join(databases, ", ")))
}

// noteDumpScope says, before a fresh server is built from a dump, what that dump
// does not contain.
//
// R2's "real gap" sentence, at the moment it applies: an application user's dump
// restores its own schemas completely and holds no accounts or grants, so a
// server rebuilt from it comes back with the databases and without the users.
// Stated, never blocking — the data is all there, and the users come from the
// restored container's own configuration.
func (e *Engine) noteDumpScope(b *store.Backup, man *Manifest) {
	if man == nil {
		return
	}
	for i := range man.Databases {
		names, schemaScoped := SchemaScoped(man.Databases[i].DumpScope)
		if !schemaScoped {
			continue
		}
		e.logf(b.ID, "WARN", "This dump was taken by an application account, so it holds %s and none of the server's own accounts or grants. Every row comes back; the database users do not, and are created from this container's own environment instead. If anything else connected to this server with its own account, that account is not in this backup.",
			names)
	}
}
