package backup

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// Capturing a PostgreSQL cluster's configuration beside its dump (#39).
//
// A logical dump carries databases, not the cluster they live in. Server
// settings live in postgresql.conf inside the data directory, which the restore
// deliberately wipes so the engine re-initialises cleanly for the import — so
// every setting of the restored cluster comes from the TARGET's environment and
// defaults unless it is recorded here first.
//
// Two classes, both invisible until they are expensive:
//
//   - timezone. Renders every timestamptz differently, so a byte-identical
//     restore compares as broken. Measured: 19 of 66 tables "differed".
//   - encoding, collation, ctype. Immutable after initdb, and collation decides
//     text index order — get it wrong and queries silently miss rows that exist.

// pgClusterQuery reads the cluster's identity in one round trip.
//
// Rows are tagged rather than run as two statements so one exec answers both
// questions. Ordering is by the tagged text, which makes the recorded settings
// stable run to run — a manifest that reorders itself on every backup is one
// nobody can diff.
//
// `source` excludes more than the two obvious values: 'default' is what the
// server would do anyway, and 'client'/'session'/'override' are this connection
// talking to itself. Recording either kind would be writing back configuration
// the source does not actually have.
const pgClusterQuery = `SELECT 'S='||name||'='||coalesce(setting,'') FROM pg_settings ` +
	`WHERE source NOT IN ('default','client','session','override') ` +
	`UNION ALL ` +
	`SELECT 'D='||pg_encoding_to_char(encoding)||'|'||datcollate||'|'||datctype||'|'||datname ` +
	`FROM pg_database WHERE datallowconn ` +
	`ORDER BY 1`

// pgSecretSettingNames are the substrings that mark a setting whose VALUE is a
// credential, or a shell command that habitually contains one.
//
// primary_conninfo is the sharp case: on a replica it is a connection string
// with `password=` in it, and it is a non-default setting, so an unfiltered
// capture would write a live replication password into a plaintext sidecar that
// travels to every configured destination. The *_command settings are arbitrary
// shell — archive_command reaching an object store is the ordinary way to embed
// an access key.
//
// None of these describe cluster semantics, so withholding them costs a restore
// nothing at all.
var pgSecretSettingNames = []string{"password", "passphrase", "conninfo", "secret", "_command"}

// redactedPGSetting reports whether a setting's NAME says its value must not be
// recorded. Name-based on purpose: judging the value would mean inspecting it.
func redactedPGSetting(name string) bool {
	n := strings.ToLower(name)
	for _, s := range pgSecretSettingNames {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// pgClusterScript builds the psql invocation, reusing the same host/user
// resolution the dump and the import already share (pgClientOpts) so an embedded
// server on its own socket is reached the same way here as everywhere else.
//
// -X so a .psqlrc on the image cannot add output, -A -t so the rows arrive
// unadorned, and -q so psql says nothing of its own.
func pgClusterScript(d *EmbeddedDump) string {
	return "psql " + pgClientOpts(d) + ` -d postgres -X -q -A -t -c "` + pgClusterQuery + `"`
}

// ParsePGClusterConfig reads the tagged rows back. Exported so the parse is
// testable without a database, mirroring ParseTableCounts.
//
// A line it cannot read contributes nothing. That is the rule this whole feature
// exists to serve: a guessed setting is written into a cluster at initdb, where
// several of them can never be corrected — so an unreadable line must produce no
// value rather than a plausible one. Returns nil when nothing was understood, so
// a caller cannot mistake "read nothing" for "the source had nothing set".
func ParsePGClusterConfig(out string) *ClusterConfig {
	cfg := &ClusterConfig{Settings: map[string]string{}}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(strings.TrimSpace(line), "\r")
		switch {
		case strings.HasPrefix(line, "S="):
			// name=value, split on the FIRST '=': a value may contain more of
			// them (search_path, archive_command), a setting name never does.
			name, value, found := strings.Cut(line[2:], "=")
			if !found || name == "" {
				continue
			}
			if redactedPGSetting(name) {
				cfg.Redacted = append(cfg.Redacted, name)
				continue
			}
			cfg.Settings[name] = value
		case strings.HasPrefix(line, "D="):
			// encoding|collate|ctype|name — the NAME is last because it is the
			// only one of the four that could contain the separator, so the three
			// fixed fields are cut from the front and the remainder is the name.
			rest := line[2:]
			encoding, rest, ok := strings.Cut(rest, "|")
			if !ok {
				continue
			}
			collate, rest, ok := strings.Cut(rest, "|")
			if !ok {
				continue
			}
			ctype, name, ok := strings.Cut(rest, "|")
			if !ok || name == "" {
				continue
			}
			cfg.Databases = append(cfg.Databases, DBLocale{
				Name: name, Encoding: encoding, Collate: collate, Ctype: ctype,
			})
		}
	}
	if len(cfg.Settings) == 0 && len(cfg.Databases) == 0 && len(cfg.Redacted) == 0 {
		return nil
	}
	if len(cfg.Settings) == 0 {
		cfg.Settings = nil // omitempty: an empty map and no map should read alike
	}
	sort.Strings(cfg.Redacted)
	return cfg
}

// recordPGClusterConfig captures the cluster's configuration onto a completed
// dump record.
//
// Best-effort, and deliberately so: this is supplementary to a dump that has
// already succeeded, and failing a real backup because a diagnostic query did
// not run would trade the thing for a note about the thing. What it must not do
// is fail SILENTLY — a restore that quietly uses the target's defaults is the
// whole problem, so the one thing that always happens is that the operator is
// told which way it went.
func (e *Engine) recordPGClusterConfig(ctx context.Context, cli *client.Client, containerID string, d *EmbeddedDump, dump *DBDump, logID string) {
	if dump == nil || dump.Engine != "postgres" {
		return
	}
	out, err := dockercli.ExecCapture(ctx, cli, containerID, []string{"/bin/sh", "-c", pgClusterScript(d)})
	if err != nil {
		e.logf(logID, "WARN", "Cluster configuration not captured (%v) — the dump is complete, but a cross-host restore of it will use the target's defaults for settings like timezone and collation", err)
		return
	}
	cfg := ParsePGClusterConfig(string(out))
	if cfg == nil {
		e.logf(logID, "INFO", "This PostgreSQL server has no non-default settings to record")
		return
	}
	dump.ClusterConfig = cfg
	e.logf(logID, "INFO", "Recorded the cluster's configuration: %s", describeClusterConfig(cfg))
	for _, name := range cfg.Redacted {
		e.logf(logID, "INFO", "Setting %s is recorded by name only — its value is a credential and the manifest is not encrypted", name)
	}
}

// describeClusterConfig summarises what was captured, naming the settings an
// operator actually cares about rather than a bare count.
func describeClusterConfig(cfg *ClusterConfig) string {
	var parts []string
	// Matched case-insensitively: PostgreSQL spells the one that matters most
	// "TimeZone", not "timezone", and a summary that silently never mentions it
	// is how the setting behind 19 false table mismatches stays invisible.
	interesting := map[string]bool{
		"timezone": true, "log_timezone": true, "lc_collate": true, "lc_ctype": true,
		"shared_preload_libraries": true, "data_checksums": true,
	}
	for name, v := range cfg.Settings {
		if v != "" && interesting[strings.ToLower(name)] {
			parts = append(parts, name+"="+v)
		}
	}
	sort.Strings(parts)
	if n := len(cfg.Settings); n > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", n, plural(n, "non-default setting", "non-default settings")))
	}
	if n := len(cfg.Databases); n > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", n, plural(n, "database's encoding and collation", "databases' encoding and collation")))
	}
	return strings.Join(parts, ", ")
}

// Replaying a captured cluster configuration into a freshly initialised one
// (#39 restore half, #32).
//
// The restore wipes the data directory so the engine re-initialises cleanly for
// the import, which means this is the ONE moment the cluster's identity can be
// set: several of these settings are stamped into the volume at first boot and
// #32 measured what that costs — removing the environment variable afterwards
// and recreating the container changes nothing at all.

// pgReplaySettings are the settings a restore reproduces into a fresh cluster.
//
// An allow-list, not everything non-default, and the difference is not caution
// for its own sake. A stock PostgreSQL 16 reports fourteen non-default settings
// and most are what initdb itself wrote for the machine it ran on:
// shared_buffers is sized for the SOURCE's memory, max_connections can exceed
// what the target's shared memory allows, and unix_socket_directories would move
// the socket that this very restore's import is connecting through.
//
// What is here is the class that makes identical data compare as different, or
// behave differently: how a timestamp renders, how text sorts and searches, what
// locale numbers and money take. Everything outside it is recorded by the
// capture and reported below rather than dropped in silence.
var pgReplaySettings = map[string]bool{
	"timezone":                   true,
	"log_timezone":               true,
	"datestyle":                  true,
	"intervalstyle":              true,
	"lc_messages":                true,
	"lc_monetary":                true,
	"lc_numeric":                 true,
	"lc_time":                    true,
	"default_text_search_config": true,
}

// pgGUCName is the shape a setting name may take before it is put into a
// statement: an identifier, optionally namespaced. Names come from a manifest,
// which came from another machine, so this is an allow-list and not an escape —
// there is no value in accepting a name PostgreSQL could not have.
var pgGUCName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)?$`)

// alterSystemStatement builds one ALTER SYSTEM statement, or reports that the
// setting cannot safely be turned into one.
//
// One statement per call because ALTER SYSTEM refuses to run inside a
// transaction block, so they cannot be batched into a single -c.
//
// The value is written as a SQL string literal with its quotes doubled, which is
// both safe and correct for every setting: PostgreSQL accepts a quoted value for
// numeric and enum settings too. The name is validated rather than escaped.
func alterSystemStatement(name, value string) (string, bool) {
	if !pgGUCName.MatchString(name) {
		return "", false
	}
	return "ALTER SYSTEM SET " + name + " = '" + strings.ReplaceAll(value, "'", "''") + "'", true
}

// pgReplayScript wraps one statement in the same psql invocation the capture and
// the import use.
//
// The SQL is single-quoted for the shell, so nothing in a recorded value can be
// expanded by it — a setting like log_line_prefix is full of % and $ by design.
func pgReplayScript(d *EmbeddedDump, statement string) string {
	return "psql " + pgClientOpts(d) + " -d postgres -X -q -A -t -c '" + shellEscape(statement) + "'"
}

// pgTemplateLocaleScript asks the FRESH cluster what it was initialised as. The
// row is tagged the same way the capture tags its own, so one parser reads both.
const pgTemplateLocaleQuery = `SELECT 'D='||pg_encoding_to_char(encoding)||'|'||datcollate||'|'||datctype||'|'||datname ` +
	`FROM pg_database WHERE datname = 'template1'`

func pgTemplateLocaleScript(d *EmbeddedDump) string {
	return "psql " + pgClientOpts(d) + ` -d postgres -X -q -A -t -c "` + pgTemplateLocaleQuery + `"`
}

// localeMismatch compares what the source's databases were created as against
// what the target's cluster has just been initialised as, and describes the
// difference in the operator's terms.
//
// Reported, never fixed, because it cannot be: encoding, collation and ctype are
// chosen at initdb and no statement changes them afterwards. The data still
// loads and the restore still succeeds — but collation decides text index order,
// so a cluster built under a different one answers queries differently while
// looking perfectly healthy. That is worth a sentence at the moment it happens,
// not a discovery weeks later.
//
// Pure, and returns nothing when either side is unknown: a comparison that could
// not be made must not read as one that passed.
func localeMismatch(captured []DBLocale, target DBLocale) []string {
	if target.Encoding == "" && target.Collate == "" {
		return nil
	}
	var out []string
	for _, src := range captured {
		// template0/template1 are the cluster's own; the databases the dump
		// recreates inherit from template0, so comparing a user database against
		// the target's template is the comparison that predicts the outcome.
		if src.Name == "template0" || src.Name == "template1" {
			continue
		}
		for _, c := range []struct{ what, was, now string }{
			{"encoding", src.Encoding, target.Encoding},
			{"collation", src.Collate, target.Collate},
			{"ctype", src.Ctype, target.Ctype},
		} {
			if c.was == "" || c.now == "" || c.was == c.now {
				continue
			}
			out = append(out, fmt.Sprintf("%s: %s was %s on the source and is %s here", src.Name, c.what, c.was, c.now))
		}
	}
	return out
}

// pgClusterConfigFor returns the cluster configuration recorded beside this
// backup's PostgreSQL dump, if it carries one.
func pgClusterConfigFor(man *Manifest) *ClusterConfig {
	if man == nil {
		return nil
	}
	for i := range man.Databases {
		if man.Databases[i].Engine == "postgres" && man.Databases[i].ClusterConfig != nil {
			return man.Databases[i].ClusterConfig
		}
	}
	return nil
}

// replayPGClusterConfig sets the freshly initialised cluster up the way the
// source's was, before the dump is loaded into it.
//
// Called only from the two restore paths that WIPE and re-initialise. The subset
// restore deliberately does not call it: that one loads selected databases into
// a cluster the operator already has, and reconfiguring their live server
// because a backup remembers different settings is not what they asked for.
//
// Never fails a restore. A setting that will not apply — an unknown name on a
// different engine version, a read-only one — is one line in the log and the
// import proceeds; the data is what the operator came for, and a cluster with
// the target's timezone still holds every row.
func (e *Engine) replayPGClusterConfig(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions, d *EmbeddedDump) {
	cfg := pgClusterConfigFor(man)
	if cfg == nil {
		// Nothing recorded. Only worth saying on a move: restoring onto the
		// machine the backup came from, the defaults ARE the source's.
		if crossHostRestore(b, man, opts) {
			e.logf(b.ID, "WARN", "This backup predates cluster-configuration capture — the restored cluster uses this machine's defaults, so its timezone and collation may differ from the source's. Take a fresh backup to record them.")
		}
		return
	}

	applied, skipped := 0, []string{}
	for _, name := range sortedKeys(cfg.Settings) {
		if strings.EqualFold(name, pgPreloadSetting) {
			continue // reproduced below, on its own terms
		}
		if !pgReplaySettings[strings.ToLower(name)] {
			skipped = append(skipped, name)
			continue
		}
		statement, ok := alterSystemStatement(name, cfg.Settings[name])
		if !ok {
			e.logf(b.ID, "WARN", "Not replaying %s — its name is not one PostgreSQL could have produced, so it is not put into a statement", name)
			continue
		}
		if _, err := dockercli.ExecCapture(ctx, cli, opts.TargetID, []string{"/bin/sh", "-c", pgReplayScript(d, statement)}); err != nil {
			e.logf(b.ID, "WARN", "Could not set %s to %q on the restored cluster (%v) — it keeps this machine's value", name, cfg.Settings[name], err)
			continue
		}
		applied++
	}
	if applied > 0 {
		// A reload, not a restart: every setting replayed here takes effect on
		// reload, and restarting the server now would race the import.
		if _, err := dockercli.ExecCapture(ctx, cli, opts.TargetID, []string{"/bin/sh", "-c",
			pgReplayScript(d, "SELECT pg_reload_conf()")}); err != nil {
			e.logf(b.ID, "WARN", "Applied %d cluster setting(s) but could not reload the configuration (%v) — they take effect when the database next restarts", applied, err)
		} else {
			e.logf(b.ID, "INFO", "Restored %d of the source cluster's settings before loading the dump (%s)", applied, describeClusterConfig(cfg))
		}
	}
	if len(skipped) > 0 {
		// Named, not silently dropped: these were recorded, and an operator who
		// tuned their source deserves to know the tuning did not travel.
		e.logf(b.ID, "INFO", "%d recorded setting(s) were not replayed because their right value depends on this machine rather than on the backup: %s",
			len(skipped), strings.Join(skipped, ", "))
	}
	for _, name := range cfg.Redacted {
		e.logf(b.ID, "INFO", "%s was recorded by name only — its value is a credential and was never put in the backup, so set it here yourself if this cluster needs it", name)
	}

	e.replayPGPreload(ctx, cli, b, opts, d, cfg)
	e.reportPGLocaleMismatch(ctx, cli, b, opts.TargetID, cfg, d)
}

// reportPGLocaleMismatch says when the fresh cluster was initialised with a
// different character-set identity than the source had.
//
// Split out because it is a different kind of statement from the replay above:
// there is nothing to do about it here. It is said loudly and once.
func (e *Engine) reportPGLocaleMismatch(ctx context.Context, cli *client.Client, b *store.Backup, targetID string, cfg *ClusterConfig, d *EmbeddedDump) {
	if len(cfg.Databases) == 0 {
		return
	}
	out, err := dockercli.ExecCapture(ctx, cli, targetID, []string{"/bin/sh", "-c", pgTemplateLocaleScript(d)})
	if err != nil {
		return // could not ask; a comparison not made is not a comparison passed
	}
	target := ParsePGClusterConfig(string(out))
	if target == nil || len(target.Databases) == 0 {
		return
	}
	diffs := localeMismatch(cfg.Databases, target.Databases[0])
	if len(diffs) == 0 {
		return
	}
	e.logf(b.ID, "ERROR", "This cluster was initialised with a different character-set identity than the source had, and that CANNOT be changed on an existing cluster — it is fixed at creation. The dump still loads and every row arrives, but collation decides how text sorts, so indexes here are ordered differently than they were: %s", strings.Join(diffs, "; "))
	e.logf(b.ID, "ERROR", "To match the source exactly, re-create this container with the matching locale set for its first boot (POSTGRES_INITDB_ARGS), then restore again.")
}

// sortedKeys returns a map's keys in order, so a replay applies settings in the
// same sequence every time and its log reads the same way twice.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Reproducing shared_preload_libraries, and getting back when it goes wrong.
//
// This setting is left out of pgReplaySettings because it is the one that can
// stop the server from starting: PostgreSQL loads these libraries before it will
// accept a connection, so a name whose .so is not in this image turns a healthy
// restore into a container that boots, fails, and boots again. R5 §7.1 is that
// exact failure — a source preloading "vectors" restored onto an image without
// the extension.
//
// So it is handled in three parts that the rest of the replay does not need:
// ask this machine whether it CAN load each library before setting anything,
// restart while the cost of being wrong is still nil, and — if the server does
// not come back — take the setting out of postgresql.auto.conf from outside.

// pgPreloadSetting is PostgreSQL's own name for it, and the name that appears in
// postgresql.auto.conf.
const pgPreloadSetting = "shared_preload_libraries"

// pgLibName is the shape a library entry may take. Names go into SQL and, after
// the probe, into a setting other sessions will read, so this is an allow-list:
// a real entry is a bare module name, optionally under $libdir.
var pgLibName = regexp.MustCompile(`^(\$libdir/)?[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// pgPreloadLibs splits a recorded shared_preload_libraries value into entries.
// PostgreSQL reports it as a comma-separated list and permits quoting and
// spacing inside it, so both are stripped here.
func pgPreloadLibs(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		part = strings.Trim(strings.TrimSpace(part), `"'`)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// pgLoadProbeScript asks the TARGET whether it could load one library at all.
//
// LOAD is the precise question, not an approximation of it: it dlopen()s the
// same file shared_preload_libraries would, from the same $libdir, so a name
// that survives it is present on this machine AND links against this major
// version. Checking pg_available_extensions instead would answer a different
// question — auto_explain and pg_stat_statements are preloadable modules whose
// extension may not be installed, and an extension can exist whose library was
// built for another version.
//
// Some modules refuse to be loaded this way, and that refusal is itself proof
// the file is there, which is why only the errors that mean an unusable FILE
// count as a failure.
//
// Never exits non-zero: ExecCapture discards its output when a command fails,
// and here the output IS the answer.
func pgLoadProbeScript(d *EmbeddedDump, lib string) string {
	sql := "LOAD '" + lib + "'"
	return "psql " + pgClientOpts(d) + " -d postgres -X -q -A -t -c '" + shellEscape(sql) + "' 2>&1 || true"
}

// pgLibMissing reads a LOAD probe and reports whether preloading this library
// would leave the server unable to start.
//
// Matches the three ways the file itself is unusable — absent, built for another
// PostgreSQL, or missing a symbol it links against. Every other error means the
// library loaded far enough to object to something, which is a library that
// preloads.
func pgLibMissing(out string) bool {
	lower := strings.ToLower(out)
	for _, fatal := range []string{"could not access file", "no such file", "incompatible library", "undefined symbol"} {
		if strings.Contains(lower, fatal) {
			return true
		}
	}
	return false
}

// clusterSetting reads one recorded setting by name, case-insensitively, because
// PostgreSQL reports names in its own casing ("TimeZone") and a lookup that
// assumes lower case silently finds nothing.
func clusterSetting(cfg *ClusterConfig, name string) string {
	for k, v := range cfg.Settings {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// pgDataDir is where this cluster keeps postgresql.auto.conf: the application's
// own data directory when the database is embedded in one, the engine's standard
// location when the container IS the database.
func pgDataDir(d *EmbeddedDump) string {
	if d != nil && d.DataDir != "" {
		return d.DataDir
	}
	return dockercli.DBDataDir("postgres")
}

// replayPGPreload reproduces the source's preloaded libraries, but only the ones
// this machine can actually load.
//
// Probing first is what makes this safe to do at all. The alternative — set it
// and find out — cannot be undone from inside: ALTER SYSTEM RESET needs a
// running server, and this setting is what stops the server running.
func (e *Engine) replayPGPreload(ctx context.Context, cli *client.Client, b *store.Backup, opts RestoreOptions, d *EmbeddedDump, cfg *ClusterConfig) {
	libs := pgPreloadLibs(clusterSetting(cfg, pgPreloadSetting))
	if len(libs) == 0 {
		return
	}

	var loadable, absent []string
	for _, lib := range libs {
		if !pgLibName.MatchString(lib) {
			absent = append(absent, lib)
			continue
		}
		out, err := dockercli.ExecCapture(ctx, cli, opts.TargetID, []string{"/bin/sh", "-c", pgLoadProbeScript(d, lib)})
		if err != nil || pgLibMissing(string(out)) {
			absent = append(absent, lib)
			continue
		}
		loadable = append(loadable, lib)
	}

	if len(absent) > 0 {
		e.logf(b.ID, "ERROR", "The source cluster preloaded %s, which this image cannot load — left out of %s so the database still starts. Data that does not depend on %s restores normally, but anything that does (an extension's own tables, its indexes, its background workers) will not work until this container runs an image that provides %s.",
			strings.Join(absent, ", "), pgPreloadSetting,
			plural(len(absent), "it", "them"), plural(len(absent), "it", "them"))
	}
	if len(loadable) == 0 {
		return
	}

	statement, ok := alterSystemStatement(pgPreloadSetting, strings.Join(loadable, ", "))
	if !ok {
		return
	}
	if _, err := dockercli.ExecCapture(ctx, cli, opts.TargetID, []string{"/bin/sh", "-c", pgReplayScript(d, statement)}); err != nil {
		e.logf(b.ID, "WARN", "Could not set %s to %q on the restored cluster (%v) — extensions the source preloaded will not be active here", pgPreloadSetting, strings.Join(loadable, ", "), err)
		return
	}
	e.verifyPGPreload(ctx, cli, b, opts, d, loadable)
}

// verifyPGPreload restarts the database to prove the setting it was just given
// is one it can start with, and takes it back out if not.
//
// Restarting here rather than letting the restore's own later restart discover
// it is the point of the whole exercise: at this moment nothing has been
// imported, so a failure costs one restart. Discovered after the import, the
// same failure leaves the operator with their data inside a container that will
// not run.
func (e *Engine) verifyPGPreload(ctx context.Context, cli *client.Client, b *store.Backup, opts RestoreOptions, d *EmbeddedDump, libs []string) {
	list := strings.Join(libs, ", ")
	e.logf(b.ID, "INFO", "Restarting the database to preload %s, as the source did — a preloaded library only takes effect at startup, so this checks it now, while the data is still safely in the backup.", list)
	if err := cli.ContainerRestart(ctx, opts.TargetID, container.StopOptions{}); err != nil {
		e.logf(b.ID, "WARN", "Could not restart to load %s (%v) — the setting is written and takes effect when this container next starts", list, err)
		return
	}
	if err := dockercli.WaitForDB(ctx, cli, opts.TargetID, "postgres"); err == nil {
		e.logf(b.ID, "INFO", "The restored cluster preloads %s, as the source's did", list)
		return
	}

	dataDir := pgDataDir(d)
	e.logf(b.ID, "ERROR", "The database did not come back after being told to preload %s, so that setting is being removed and the restore continues without it. The libraries are present but at least one of them cannot be preloaded in this image.", list)
	if err := dockercli.StripAutoConfSetting(ctx, cli, opts.TargetID, dataDir, pgPreloadSetting); err != nil {
		e.logf(b.ID, "ERROR", "Could not remove it automatically (%v). This container will not start until it is gone: delete the %s line from %s/%s and start the container again.", err, pgPreloadSetting, dataDir, dockercli.AutoConfFile)
		return
	}
	if err := cli.ContainerRestart(ctx, opts.TargetID, container.StopOptions{}); err != nil {
		e.logf(b.ID, "ERROR", "Removed the %s setting but could not restart (%v) — start this container manually and run the restore again", pgPreloadSetting, err)
		return
	}
	if err := dockercli.WaitForDB(ctx, cli, opts.TargetID, "postgres"); err != nil {
		e.logf(b.ID, "ERROR", "Removed the %s setting but the database still does not accept connections (%v) — the restore cannot import into it", pgPreloadSetting, err)
		return
	}
	e.logf(b.ID, "INFO", "The database is running again without %s, and the import continues. Set it yourself once this container runs an image that can preload %s.", pgPreloadSetting, list)
}
