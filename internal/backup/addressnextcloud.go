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

// Nextcloud address reconciliation (F114).
//
// Nextcloud records the addresses it will answer for in `trusted_domains`, which
// lives in config.php INSIDE a captured volume. A faithful restore therefore
// writes the OLD machine's address straight back, and the site answers every
// request with "You are accessing the server from an untrusted domain." The
// backup is intact, the container is healthy, and the app is unreachable.
//
// This is fixed through occ — Nextcloud's own supported tool — and never by
// editing config.php. DockBack does not rewrite an application's state by hand;
// a regex through a PHP file is how a working restore becomes a broken one.
//
// It is safe to automate for exactly one reason: setting a config value is undone
// by setting it again. Contrast BookStack's content rewrite, which is
// irreversible and therefore stays guidance-only.

// occ runs as the web user from Nextcloud's document root, the way the
// documentation specifies. Every invocation is argv — no shell is constructed at
// any point — so the operator-supplied address cannot be interpreted as
// anything but a single argument.
const (
	occUser    = "www-data"
	occWorkDir = "/var/www/html"
)

// maxTrustedDomains bounds how far the index scan will go. A real deployment has
// a handful; a runaway list means something else is wrong and is not something to
// append to.
const maxTrustedDomains = 64

// applyNextcloudAddress points a restored Nextcloud at its new address.
//
// Order matters: trusted_domains first (without it the site refuses every
// request, including the ones the later settings affect), then the two
// URL-generation settings that must agree with it.
//
// Never fails the restore. The data is restored and correct; an unreachable
// address is a setting the operator can fix in one command, and discarding a good
// restore over it would be the wrong trade. Every failure path prints that
// command.
func (e *Engine) applyNextcloudAddress(ctx context.Context, cli *client.Client, b *store.Backup, opts RestoreOptions, crossHost bool) {
	addr := strings.TrimSpace(opts.NewSiteAddress)
	if err := ValidSiteAddress(addr); err != nil {
		e.logf(b.ID, "WARN", "Not changing Nextcloud's trusted domains: %v", err)
		return
	}
	host := AddressHost(addr)
	if host == "" {
		return
	}

	// 0) #22: the file every step below edits, copied aside first. occ is
	//    reversible for the values this module replaces — each old one is printed
	//    as it is replaced — and the copy is that same guarantee for the rest of
	//    the file, taken before the first write rather than reconstructed after.
	e.backupNextcloudConfig(ctx, cli, b, opts.TargetID)

	// 1) Read the current list. Indexes are line positions, which is also how occ
	//    addresses them for writing.
	out, err := dockercli.ExecHook(ctx, cli, opts.TargetID,
		[]string{"php", "occ", "config:system:get", "trusted_domains"}, occUser, occWorkDir)
	if err != nil {
		e.logf(b.ID, "WARN", "Could not read Nextcloud's trusted domains (%v) — the restore is fine, but Nextcloud will refuse requests at %s until you run:\n%s",
			err, host, occSetHint(host))
		return
	}
	domains := parseTrustedDomains(string(out))

	// 2) Decide replace-vs-add. Replacing the stale entry in place leaves the
	//    trust list exactly as large as it was, which is the honest operation for
	//    a move. Adding is a widening — of exactly one named host, which is what
	//    was asked for — and is logged as such rather than done quietly.
	idx, action := trustedDomainSlot(domains, host, opts.RemapFromIP)
	if action == trustedDomainAlready {
		e.logf(b.ID, "INFO", "Nextcloud already trusts %s — trusted_domains left unchanged", host)
	} else {
		if idx < 0 || idx >= maxTrustedDomains {
			e.logf(b.ID, "WARN", "Nextcloud's trusted_domains list is unexpectedly long — not modifying it. Run this by hand:\n%s", occSetHint(host))
			return
		}
		if _, serr := dockercli.ExecHook(ctx, cli, opts.TargetID,
			[]string{"php", "occ", "config:system:set", "trusted_domains", strconv.Itoa(idx), "--value", host},
			occUser, occWorkDir); serr != nil {
			e.logf(b.ID, "WARN", "Could not set Nextcloud's trusted domain (%v) — the restore is fine, but Nextcloud will refuse requests at %s until you run:\n%s",
				serr, host, occSetHint(host))
			return
		}
		switch action {
		case trustedDomainReplace:
			e.logf(b.ID, "INFO", "Replaced Nextcloud trusted_domains[%d] (%s) with %s — the list is the same size as before", idx, domains[idx], host)
		default:
			e.logf(b.ID, "WARN", "Added %s to Nextcloud trusted_domains at index %d. This WIDENS the trust list by one host — the stale entries were left in place; remove any for a decommissioned machine by hand.", host, idx)
		}
	}

	// 3) The URL-generation settings, which must agree with the trust list or
	//    Nextcloud emits links to the old address from cron and email.
	//    Best-effort and individually reported: one failing must not hide the
	//    other, and neither is worth aborting for.
	scheme := "https"
	if strings.HasPrefix(addr, "http://") {
		scheme = "http"
	}
	for _, s := range []struct{ key, value string }{
		{"overwritehost", host},
		{"overwrite.cli.url", scheme + "://" + host},
		{"overwriteprotocol", scheme},
	} {
		if _, serr := dockercli.ExecHook(ctx, cli, opts.TargetID,
			[]string{"php", "occ", "config:system:set", s.key, "--value", s.value},
			occUser, occWorkDir); serr != nil {
			e.logf(b.ID, "WARN", "Could not set Nextcloud %s (%v) — set it with: docker exec -u %s <container> php occ config:system:set %s --value %s",
				s.key, serr, occUser, s.key, s.value)
			continue
		}
		e.logf(b.ID, "INFO", "Set Nextcloud %s to %s", s.key, s.value)
	}

	// 3b) F175: and check nothing is quietly outranking what was just written.
	//
	//     The official image's config/reverse-proxy.config.php reads OVERWRITEHOST
	//     from the ENVIRONMENT on every request and is loaded AFTER config.php, so
	//     an environment variable beats occ every time. When the container is
	//     recreated the env binding above has already corrected it; when it is
	//     not, occ reports success and the site keeps redirecting to the old
	//     machine. That gap is exactly how this went unnoticed, so it is named.
	if envAddr := nextcloudEnvOverrides(ctx, cli, opts.TargetID); len(envAddr) > 0 {
		for _, k := range []string{"OVERWRITEHOST", "OVERWRITECLIURL", "OVERWRITEPROTOCOL"} {
			v, ok := envAddr[k]
			if !ok || v == "" {
				continue
			}
			if k == "OVERWRITEHOST" && strings.EqualFold(AddressHost(v), host) {
				continue // agrees; nothing to warn about
			}
			if k == "OVERWRITECLIURL" && strings.EqualFold(AddressHost(v), host) {
				continue
			}
			if k == "OVERWRITEPROTOCOL" && strings.EqualFold(v, scheme) {
				continue
			}
			e.logf(b.ID, "WARN", "The container's %s environment variable is still %s, and Nextcloud reads it on every request — it OVERRIDES what was just set with occ. "+
				"Change it in the compose file for this container and recreate it, or the site will keep using the old address.", k, v)
		}
	}

	// 4) Read the list back, so the run log records what the trust list ACTUALLY
	//    became rather than what was intended.
	if back, rerr := dockercli.ExecHook(ctx, cli, opts.TargetID,
		[]string{"php", "occ", "config:system:get", "trusted_domains"}, occUser, occWorkDir); rerr == nil {
		e.logf(b.ID, "INFO", "Nextcloud now trusts: %s", strings.Join(parseTrustedDomains(string(back)), ", "))
	}

	// 5) #22 / PLAYBOOK §9.3: proxy trust answers a different question from the
	//    site address — which upstream may set X-Forwarded-For — so it is decided
	//    from whether the network path in front of this container actually
	//    changed, not from the fact that an address did. See proxyTrustPolicy.
	e.applyNextcloudProxyTrust(ctx, cli, b, opts, proxyTrustPolicy(crossHost, true))
}

// reportNextcloudRecordedAddress states the address a restored Nextcloud still
// answers for, when the operator supplied no new one and the app has just landed
// on a different machine (F175).
//
// The silence this replaces was the whole problem. A cross-host restore finishes
// green — every service up, every check passed — while the application still
// carries the previous machine's address in overwritehost, so it answers each
// request with a redirect to a host that is somewhere else entirely. Nothing in
// the run log mentioned it, because nothing had been asked for.
//
// Strictly read-only: two occ gets and a sentence. Supplying the new address in
// the restore dialog remains the way to CHANGE it — this exists so that not
// supplying one is an informed choice rather than a surprise a week later.
func (e *Engine) reportNextcloudRecordedAddress(ctx context.Context, cli *client.Client, b *store.Backup, opts RestoreOptions) {
	// The environment first, because when it is set it is what decides — and
	// because the occ command this would otherwise recommend does not work
	// against it.
	if envAddr := nextcloudEnvOverrides(ctx, cli, opts.TargetID); envAddr["OVERWRITEHOST"] != "" {
		e.logf(b.ID, "WARN", "Nextcloud was restored onto a different machine and its container still carries OVERWRITEHOST=%s from the previous one. "+
			"Nextcloud reads that on every request, so it will redirect visitors there whatever address they arrive on — and occ cannot override it. "+
			"The data is correct; this is one line in your compose file. Change OVERWRITEHOST (and OVERWRITECLIURL / NEXTCLOUD_TRUSTED_DOMAINS beside it) "+
			"and recreate the container, or restore again with the new address filled in and DockBack will set them for you.", envAddr["OVERWRITEHOST"])
		return
	}

	get := func(key string) string {
		out, err := dockercli.ExecHook(ctx, cli, opts.TargetID,
			[]string{"php", "occ", "config:system:get", key}, occUser, occWorkDir)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	domains := parseTrustedDomains(get("trusted_domains"))
	overwrite := strings.TrimSpace(strings.Join(parseTrustedDomains(get("overwritehost")), ""))

	// #22: this container is on a different machine, so its recorded proxy is on
	// the old network path — but nothing was asked for and this path writes
	// nothing, so the stale trust is named rather than removed.
	e.applyNextcloudProxyTrust(ctx, cli, b, opts, proxyTrustPolicy(true, false))

	switch {
	case overwrite != "":
		// The precise cause of a redirect loop to the old machine, named.
		e.logf(b.ID, "WARN", "Nextcloud was restored onto a different machine and still records %s as its address (overwritehost). "+
			"It will redirect visitors there, whatever address they arrive on. The data is correct — this is one setting. "+
			"Restore again with the new address filled in, or run:\n%s", overwrite, occMoveHint())
	case len(domains) > 0:
		e.logf(b.ID, "WARN", "Nextcloud was restored onto a different machine and still trusts only: %s. "+
			"It will refuse requests at any other address. Restore again with the new address filled in, or run:\n%s",
			strings.Join(domains, ", "), occMoveHint())
	default:
		// Could not read either. Say that, rather than implying it was checked.
		e.logf(b.ID, "WARN", "Nextcloud was restored onto a different machine and no new address was given. "+
			"Its recorded address could not be read, so check it before relying on the site:\n%s", occMoveHint())
	}
}

// nextcloudEnvOverrides reads the address variables the official image consults
// on every request (F175).
//
// Narrowed to those keys deliberately: a container environment routinely holds
// database passwords and admin credentials, and these values are printed into a
// run log that operators paste into help requests.
func nextcloudEnvOverrides(ctx context.Context, cli *client.Client, targetID string) map[string]string {
	ictx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	insp, err := cli.ContainerInspect(ictx, targetID)
	if err != nil || insp.Config == nil {
		return nil
	}
	all := envMap(insp.Config.Env)
	out := map[string]string{}
	// TRUSTED_PROXIES is read for the same reason as the others: the official
	// image writes it into config.php on every start, so it decides what occ
	// appears to have changed (#22).
	for _, k := range []string{"OVERWRITEHOST", "OVERWRITECLIURL", "OVERWRITEPROTOCOL", "NEXTCLOUD_TRUSTED_DOMAINS", "TRUSTED_PROXIES"} {
		if v := strings.TrimSpace(all[k]); v != "" {
			out[k] = v
		}
	}
	return out
}

// occMoveHint is the full set of settings a move needs, not just the trust list.
// occSetHint below fixes an unreachable site; this one also fixes a site that
// loads and then redirects somewhere else, which is a different symptom with a
// different cause.
func occMoveHint() string {
	x := "  docker exec -u " + occUser + " <container> php occ config:system:"
	return x + "set trusted_domains 1 --value <new-host>\n" +
		x + "set overwritehost --value <new-host>\n" +
		x + "set overwrite.cli.url --value https://<new-host>\n" +
		x + "delete overwritehost   # instead of the above, if the site is reached directly at whatever address the visitor used"
}

// occSetHint is the single manual command that fixes an unreachable Nextcloud,
// used on every failure path so the operator is never left with only a problem.
func occSetHint(host string) string {
	return "  docker exec -u " + occUser + " <container> php occ config:system:set trusted_domains 1 --value " + host
}

// parseTrustedDomains reads occ's list output, one entry per line, dropping the
// blank lines and any framing occ adds. Pure, so the index arithmetic that
// decides what gets overwritten is unit-tested without a container.
func parseTrustedDomains(out string) []string {
	var domains []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
		if line == "" || strings.HasPrefix(line, "Config value") {
			continue
		}
		domains = append(domains, line)
	}
	return domains
}

// trustedDomainAction is what applying the new host to the list amounts to.
type trustedDomainAction int

const (
	trustedDomainAlready trustedDomainAction = iota // present already: nothing to do
	trustedDomainReplace                            // overwrite a stale entry in place
	trustedDomainAdd                                // append one entry (a widening)
)

// trustedDomainSlot picks which index the new host should be written to.
//
// Preference order, and the reasoning behind it:
//
//  1. Already present → do nothing. Re-running a restore must converge, not grow
//     the list every time.
//  2. The old machine's address, when the operator told us what it was via the
//     IP remap → REPLACE it. Net trust is unchanged: one host swapped for
//     another, which is exactly what a move is.
//  3. Otherwise → append. This widens the list by one named host. It is what the
//     operator asked for, but it is not the same operation as (2) and the caller
//     reports it differently.
//
// "localhost" is never chosen as the entry to replace: Nextcloud's own occ and
// cron reach the instance that way, and taking it away breaks them.
func trustedDomainSlot(domains []string, host, oldAddr string) (int, trustedDomainAction) {
	for i, d := range domains {
		if strings.EqualFold(strings.TrimSpace(d), host) {
			return i, trustedDomainAlready
		}
	}
	if old := AddressHost(oldAddr); old != "" && !strings.EqualFold(old, "localhost") {
		for i, d := range domains {
			if strings.EqualFold(strings.TrimSpace(d), old) {
				return i, trustedDomainReplace
			}
		}
	}
	return len(domains), trustedDomainAdd
}
