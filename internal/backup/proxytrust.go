package backup

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// Proxy trust after a move (#22, PLAYBOOK §9.3).
//
// §9.3 sorts address-shaped settings into three roles with three treatments, and
// proxy trust is the one DockBack and the playbook disagreed about:
//
//	| proxy trust | trusted_proxies, TRUSTED_PROXIES | CLEAR |
//	| trusts a machine no longer in the path — header-spoofing risk |
//
// DockBack kept it, on the grounds that a new SITE ADDRESS says nothing about
// which upstream may set X-Forwarded-For, and that setting it from the wrong
// input lets a client spoof its source address past Nextcloud's brute-force
// protection and rate limits.
//
// Both are right about different restores, which is why the disagreement never
// resolved: the playbook is describing a MIGRATION, where the proxy is provably
// somewhere else, and DockBack is describing every restore, most of which are
// same-host and leave the proxy exactly where it was. Clearing on a same-host
// restore breaks X-Forwarded-For for a proxy that is still in front of the app;
// keeping on a cross-host restore trusts an address that now belongs to some
// other machine on the target's network.
//
// So this is an ENVIRONMENTAL rule in the sense of §2.1 — it applies because of
// a measured difference between two machines, not because a restore is
// happening — and it is decided from that difference rather than by picking a
// side universally. Every outcome, cleared or kept, prints the command that puts
// the old value back.

// proxyTrustAction is what this restore does about a recorded proxy-trust list.
type proxyTrustAction int

const (
	// proxyTrustKeep — nothing about the path in front of the app changed.
	proxyTrustKeep proxyTrustAction = iota
	// proxyTrustReport — the path changed, but this restore is not writing
	// settings, so the stale trust is named rather than removed.
	proxyTrustReport
	// proxyTrustClear — the path provably changed and this restore is already
	// rewriting the app's address settings through its own tool.
	proxyTrustClear
)

// proxyTrustVerdict is the decision and the reason, so a run log can state both.
type proxyTrustVerdict struct {
	Action proxyTrustAction
	Why    string
}

// proxyTrustPolicy resolves the §9.3 conflict from the two facts that decide it.
//
// crossHost is whether the container landed on a different node than the backup
// came from. addressChanged is whether the operator asked for a new site address
// on this restore — which is also what makes this restore one that writes
// settings at all.
//
// The reasoning per cell:
//
//   - same host, no address change: nothing moved. Clearing here would remove
//     working configuration to fix a problem that does not exist.
//   - same host, new address: the machine is the same, so the reverse proxy in
//     front of it is very likely the same one at the same address. An address
//     change is a DNS-and-vhost operation, not a network-path one.
//   - different host, no address change: the recorded proxy is on the old
//     network path and its address may belong to something else here — but
//     nothing was asked for, and this restore writes nothing. Named, not
//     removed.
//   - different host, new address: the app moved AND its address moved, so the
//     recorded upstream is provably not the one in front of it here. That is
//     §9.3's case exactly, and the restore is already writing settings through
//     the app's own reversible tool.
func proxyTrustPolicy(crossHost, addressChanged bool) proxyTrustVerdict {
	if !crossHost {
		if !addressChanged {
			return proxyTrustVerdict{Action: proxyTrustKeep,
				Why: "this restore did not move the container or change its address, so nothing about the network path in front of it changed"}
		}
		return proxyTrustVerdict{Action: proxyTrustKeep,
			Why: "the container is on the same machine, so the reverse proxy in front of it is almost certainly the same one at the same address — clearing it would break X-Forwarded-For for a proxy that is still there"}
	}
	if !addressChanged {
		return proxyTrustVerdict{Action: proxyTrustReport,
			Why: "the container moved to a different machine, so its recorded proxy is on the old network path — but no address change was asked for and this restore changes no settings, so it is reported rather than removed"}
	}
	return proxyTrustVerdict{Action: proxyTrustClear,
		Why: "the container moved to a different machine and its address changed with it, so the recorded proxy is provably not the one in front of it here — and an entry that trusts a machine no longer in the path is a header-spoofing risk"}
}

// occProxyRestoreLines renders the commands that put a proxy-trust list back,
// one indexed entry per line.
//
// This is what makes clearing safe to do automatically: occ has no undo, but
// setting the value again IS the undo, and the run log is where the old value
// survives the delete. Printed before the delete runs, so a failure halfway
// through still leaves the operator holding what was there.
func occProxyRestoreLines(values []string) string {
	if len(values) == 0 {
		return ""
	}
	lines := make([]string, 0, len(values))
	for i, v := range values {
		lines = append(lines, "  docker exec -u "+occUser+" <container> php occ config:system:set trusted_proxies "+strconv.Itoa(i)+" --value "+v)
	}
	return strings.Join(lines, "\n")
}

// applyNextcloudProxyTrust decides, states and — on a move — clears the recorded
// proxy-trust list.
//
// Reads first and prints the restore command before touching anything, so the
// old value is in the run log whatever happens next. Never fails the restore:
// the data is correct, and a proxy-trust entry is one command either way.
func (e *Engine) applyNextcloudProxyTrust(ctx context.Context, cli *client.Client, b *store.Backup, opts RestoreOptions, verdict proxyTrustVerdict) {
	if verdict.Action == proxyTrustKeep {
		e.logf(b.ID, "INFO", "trusted_proxies was NOT changed — %s. It is your reverse proxy's IP, not the site address. If the proxy moved too, set it with: docker exec -u %s <container> php occ config:system:set trusted_proxies 0 --value <proxy-ip>",
			verdict.Why, occUser)
		return
	}

	out, err := dockercli.ExecHook(ctx, cli, opts.TargetID,
		[]string{"php", "occ", "config:system:get", "trusted_proxies"}, occUser, occWorkDir)
	// occ exits non-zero for a key that is not set, which is the common case and
	// not a failure worth reporting as one.
	proxies := parseTrustedDomains(string(out))
	if err != nil || len(proxies) == 0 {
		e.logf(b.ID, "INFO", "Nextcloud records no trusted_proxies, so there is nothing stale to clear after the move")
		return
	}

	restore := occProxyRestoreLines(proxies)
	if verdict.Action == proxyTrustReport {
		e.logf(b.ID, "WARN", "Nextcloud still trusts %s as its reverse proxy, and %s. On this machine that address may belong to something else entirely, and anything that reaches it can then set X-Forwarded-For — which is what Nextcloud's brute-force protection and rate limits count against. "+
			"Clear it with `php occ config:system:delete trusted_proxies`, or put it back to what your new proxy is:\n%s",
			strings.Join(proxies, ", "), verdict.Why, restore)
		return
	}

	e.logf(b.ID, "WARN", "Clearing Nextcloud's trusted_proxies (%s) — %s. To put it back exactly as it was, or to point it at the proxy in front of it here:\n%s",
		strings.Join(proxies, ", "), verdict.Why, restore)
	if _, derr := dockercli.ExecHook(ctx, cli, opts.TargetID,
		[]string{"php", "occ", "config:system:delete", "trusted_proxies"}, occUser, occWorkDir); derr != nil {
		e.logf(b.ID, "WARN", "Could not clear trusted_proxies (%v) — it still trusts %s, which is a machine that is not in the path here. Remove it with: docker exec -u %s <container> php occ config:system:delete trusted_proxies",
			derr, strings.Join(proxies, ", "), occUser)
		return
	}
	e.logf(b.ID, "INFO", "trusted_proxies cleared. Nextcloud now takes each client's address from the connection itself; if a reverse proxy fronts it here, set trusted_proxies to THAT proxy's address or every visitor will be logged as the proxy.")

	// The official image writes TRUSTED_PROXIES from the environment into
	// config.php on every start, so a container that carries one puts back what
	// occ just removed. Same shape as the OVERWRITEHOST override above, and the
	// same reason it is worth saying: occ reports success either way.
	if env := nextcloudEnvOverrides(ctx, cli, opts.TargetID); env["TRUSTED_PROXIES"] != "" {
		e.logf(b.ID, "WARN", "The container's TRUSTED_PROXIES environment variable is still %s, and the official image writes it back into config.php on every start — so this clearing is undone the next time the container restarts. Change it in the compose file for this container.",
			env["TRUSTED_PROXIES"])
	}
}

// proxyTrustEnvKeys are the environment variables that grant an upstream the
// right to say who a client is.
//
// Matched as substrings so a prefixed form (NEXTCLOUD_TRUSTED_PROXIES,
// APP_TRUSTED_PROXIES) is caught too. Deliberately narrow: this list produces a
// security warning, and a false one teaches an operator to skim past the real
// ones.
var proxyTrustEnvKeys = []string{"TRUSTED_PROX", "PROXY_TRUST"}

// proxyTrustEnv returns the proxy-trust entries a container declares, as
// "KEY=value" strings ready to print.
//
// It does NOT reuse the address reporter's redaction, and the difference
// matters: that one drops everything after the first slash, because a webhook
// URL's PATH is its credential. A proxy-trust value's slash is a NETMASK, and
// `10.0.0.0/8` printed as `10.0.0.0/…` hides the entire difference between
// trusting one machine and trusting sixteen million.
//
// So the shape is filtered instead of the value truncated: anything carrying a
// credential's grammar — an `@`, a scheme, whitespace, quoting or shell
// metacharacters — is dropped whole rather than shortened, and everything else
// prints verbatim. A value like `*` or `REMOTE_ADDR` therefore still reaches the
// operator, which matters because those are the most dangerous settings of all.
//
// Pure, so both the matching and the filter are unit-testable.
func proxyTrustEnv(env []string) []string {
	var out []string
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || !proxyTrustEnvKey(key) || !reportableProxyTrust(value) {
			continue
		}
		out = append(out, key+"="+value)
	}
	return out
}

// proxyTrustEnvKey reports whether a variable's NAME says it grants proxy trust,
// and that its name does not also say it holds a secret — the same ordering
// addressLikeKey uses, so anything ambiguous is treated as a secret.
func proxyTrustEnvKey(key string) bool {
	up := strings.ToUpper(key)
	for _, secret := range secretKeySubstrings {
		if strings.Contains(up, secret) {
			return false
		}
	}
	for _, sub := range proxyTrustEnvKeys {
		if strings.Contains(up, sub) {
			return true
		}
	}
	return false
}

// reportableProxyTrust reports whether a value is safe to print verbatim: a
// list of addresses, netmasks or keywords, and nothing that could carry a
// credential.
func reportableProxyTrust(value string) bool {
	if value == "" || len(value) > maxReportedAddressValue {
		return false
	}
	if strings.ContainsAny(value, "@\"'`$\t\n<>|;&") || strings.Contains(value, "://") {
		return false
	}
	return true
}

// reportProxyTrustEnv names the proxy-trust variables a container still carries
// after landing on a different machine.
//
// Reported, never cleared. DockBack clears Nextcloud's because it knows what the
// setting is, where it lives and how to put it back; for an application it has
// no profile for, the variable's exact semantics — an IP, a CIDR, a count of
// hops, a comma-separated list, "*" — differ per framework, and blanking one it
// has guessed at is how a working restore becomes an app that rejects every
// request or trusts every client.
func (e *Engine) reportProxyTrustEnv(b *store.Backup, env []string) {
	found := proxyTrustEnv(env)
	if len(found) == 0 {
		return
	}
	e.logf(b.ID, "WARN", "This container was restored onto a different machine and still grants proxy trust to the previous one. %s:\n  %s\n"+
		"Whatever is at that address here can set X-Forwarded-For and be believed — which is what rate limits, brute-force protection and audit logs count against. "+
		"Clear it, or set it to the reverse proxy that fronts this container now. DockBack does not change it for you: what the value means differs per application, and guessing wrong either rejects every request or trusts every client.",
		plural(len(found), "Check this variable", "Check these variables"), strings.Join(found, "\n  "))
}

// Preserving the original config before DockBack's own writes (#22).
//
// occ is reversible in principle — setting a value again is the undo — but that
// is only true for the operator who knows what the value WAS. The run log
// records every old value this module replaces; the copy beside config.php is
// the same guarantee for everything else in the file, made before the first
// write rather than reconstructed afterwards.
//
// It follows the convention the compose/.env writer already established: the
// canonical name stays on the file the application reads, and the displaced copy
// takes a suffixed one. Nothing is ever deleted.

// nextcloudConfigFile is the file occ edits, relative to occWorkDir.
const nextcloudConfigFile = "config/config.php"

// backupNextcloudConfig copies config.php aside before DockBack's first write.
//
// The name deliberately does NOT end in `.config.php`: Nextcloud loads every
// file matching that glob out of the config directory, so a copy named that way
// would be read back as live configuration instead of sitting beside it.
//
// Best-effort. A failed copy is reported and the restore continues: every change
// this module makes is individually reversible through occ and its old value is
// in the run log, so refusing the address fix over a missing safety net would
// trade a reachable site for a copy of a file.
func (e *Engine) backupNextcloudConfig(ctx context.Context, cli *client.Client, b *store.Backup, targetID string) {
	aside := nextcloudConfigFile + ".dockback-" + strconv.FormatInt(time.Now().Unix(), 10) + ".bak"
	if _, err := dockercli.ExecHook(ctx, cli, targetID,
		[]string{"cp", "-p", nextcloudConfigFile, aside}, occUser, occWorkDir); err != nil {
		e.logf(b.ID, "WARN", "Could not copy Nextcloud's config.php aside before changing it (%v) — the changes below are still individually reversible with occ, and each one's old value is in this log", err)
		return
	}
	e.logf(b.ID, "INFO", "Copied Nextcloud's configuration to %s/%s before changing anything — the original is beside it, and nothing was deleted.", occWorkDir, aside)
}
