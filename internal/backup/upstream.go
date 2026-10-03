package backup

import (
	"context"
	"fmt"
	"strings"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// Upstream (dependency) addresses (F160/F161).
//
// F114 answers "where is this application served?" — the address clients use to
// reach it. This is the other direction, and it behaves in the opposite way:
// the address of a service the application reaches OUT to.
//
// The distinction matters because the two break at opposite moments. Move the
// application and its own address changes, while every dependency address it
// holds is still correct. Move a DEPENDENCY and nothing about the application
// changed at all — and it quietly stops working.
//
// Tautulli is the clean case. It stores the address of the media server it reads
// from, plus a token that is bound to that server's identity rather than to any
// address. So moving Tautulli needs nothing: the token, the identity and the
// address all still point at a server that has not gone anywhere. Moving the
// media server needs exactly one value changed, and nothing re-authenticated.
//
// The failure it causes is the quiet kind. The application starts, its
// healthcheck passes, its interface loads — and it collects nothing, because the
// thing it collects from is no longer where it was told.
//
// Two halves:
//
//   - A rewrite, applied ONLY when the operator supplies a new address. The edit
//     happens INSIDE the container, key by key within a named section, and is
//     verified afterwards; the file's contents never pass through DockBack,
//     which matters because this class of file is exactly where an application
//     keeps its credentials.
//   - A probe after the restore is healthy, which proves the dependency is
//     reachable with the credentials that were restored. Advisory: a dependency
//     being down is not a defect in the backup, and refusing a recovery over it
//     would be refusing the wrong thing.

// upstreamFor returns the dependency-address declaration for an image, or nil.
func upstreamFor(image string) *UpstreamAddress {
	p := ProfileFor(image)
	if p == nil {
		return nil
	}
	return p.Upstream
}

// iniSetScript builds the in-container edit: set each key, within one section,
// to a value — leaving every other line of the file exactly as it was.
//
// Written as awk rather than sed because a configuration file has SECTIONS, and
// a key name is only unique inside one of them. sed sees lines; awk can track
// which section it is in, which is the difference between changing the value the
// operator asked for and changing one that happens to share its name.
//
// The value arrives through `-v`, never interpolated into the program text, so
// it cannot become part of the script. It has already passed ValidSiteAddress,
// so this is the second of two barriers rather than the only one.
func iniSetScript(file, section string, assignments map[string]string) string {
	q := "'" + shellEscape(file) + "'"
	var b strings.Builder
	b.WriteString("[ -f " + q + " ] || { echo 'DockBack: no such file' >&2; exit 3; }; set -e; ")
	b.WriteString("command -v awk >/dev/null 2>&1 || { echo 'DockBack: no awk' >&2; exit 4; }; ")

	for key, val := range assignments {
		// The awk program: track the current section header, and rewrite only the
		// matching key inside the wanted one. Quotes around the value are
		// preserved when the original had them, because some applications write
		// their INI values quoted and read them back the same way.
		prog := `BEGIN{sec="";done=0}
/^[ \t]*\[/{s=$0;gsub(/^[ \t]*\[[ \t]*/,"",s);gsub(/[ \t]*\][ \t]*$/,"",s);sec=s}
{
  if (sec==SEC && $0 ~ "^[ \t]*" KEY "[ \t]*=") {
    q=""; if ($0 ~ /=[ \t]*"/) q="\"";
    print KEY " = " q VAL q; done=1; next
  }
  print
}
END{if(!done) exit 9}`
		b.WriteString("awk -v SEC='" + shellEscape(section) + "' -v KEY='" + shellEscape(key) +
			"' -v VAL='" + shellEscape(val) + "' '" + prog + "' " + q + " > " + q + ".dockback-tmp; ")
		b.WriteString("cat " + q + ".dockback-tmp > " + q + "; rm -f " + q + ".dockback-tmp; ")
	}
	// Prove it: every key now reads back with the value that was asked for. An
	// edit that silently matched nothing is the failure this guards against.
	for key, val := range assignments {
		b.WriteString("grep -q '^" + shellEscape(key) + `[ \t]*=[ \t]*"\?` + shellEscape(val) + `"\?[ \t]*$' ` + q +
			" || { echo 'DockBack: " + shellEscape(key) + " did not take the new value' >&2; exit 5; }; ")
	}
	b.WriteString("exit 0")
	return b.String()
}

// applyUpstreamAddress points an application at a dependency that has moved
// (F160), before the container is started.
//
// Runs only when the operator supplied a new address — the common move, where
// the dependency has not gone anywhere, needs nothing and gets nothing. A
// failure here is reported and does not fail the restore: the data is back and
// correct, and one setting pointing at the old address is a thing the operator
// can fix in the application's own interface in a minute.
func (e *Engine) applyUpstreamAddress(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions) {
	up := upstreamFor(manifestImage(man, b))
	if up == nil || strings.TrimSpace(opts.NewUpstreamAddress) == "" {
		return
	}
	newAddr := strings.TrimSpace(opts.NewUpstreamAddress)
	if err := ValidSiteAddress(newAddr); err != nil {
		e.logf(b.ID, "WARN", "The new %s address was not applied: %v", up.Service, err)
		return
	}

	assignments := map[string]string{}
	for _, k := range up.URLKeys {
		assignments[k] = strings.TrimSuffix(newAddr, "/")
	}
	for _, k := range up.HostKeys {
		assignments[k] = upstreamHost(newAddr)
	}
	if len(assignments) == 0 {
		return
	}

	e.logf(b.ID, "INFO", "Pointing %s at its new %s address (%s) — only the address changes; the credentials and the recorded identity are left exactly as restored",
		profileName(man, b), up.Service, newAddr)
	out, err := dockercli.ExecHook(ctx, cli, opts.TargetID,
		[]string{"/bin/sh", "-c", iniSetScript(up.File, up.Section, assignments)}, "", "")
	if err != nil {
		e.logf(b.ID, "WARN", "Could not update the %s address in %s (%v%s) — the restore is complete and correct; set it in the application's own settings instead",
			up.Service, up.File, err, detailSuffix(strings.TrimSpace(string(out))))
		return
	}
	e.logf(b.ID, "INFO", "%s now points at %s. Nothing was re-authenticated: the stored credential is bound to that service's identity, not to its address", up.Service, newAddr)
}

// upstreamHost reduces an address to the bare host an "ip"-style key wants,
// dropping the scheme and any port — a key named for a host that is handed a
// URL is a value the application will never match.
func upstreamHost(addr string) string {
	h := AddressHost(addr)
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return strings.Trim(h, "[]")
}

// profileName is the application's name for a message, falling back to the
// container's own name when no profile claims the image.
func profileName(man *Manifest, b *store.Backup) string {
	if p := ProfileFor(manifestImage(man, b)); p != nil {
		return p.Name
	}
	if man != nil && man.TargetName != "" {
		return man.TargetName
	}
	return "the application"
}

// probeUpstream confirms, after the restore is healthy, that the dependency is
// reachable with the credentials that came back (F161).
//
// Advisory in both directions and deliberately so. A dependency that is down, or
// on a network this target cannot reach, says nothing about whether the backup
// restored correctly — and failing a recovery over it would be refusing the
// wrong thing entirely. What it does is turn "it looks fine" into either "it is
// genuinely talking to the right server" or a specific thing to go and look at.
//
// The credential never leaves the container: the probe reads it from the
// application's own configuration, uses it, and prints a verdict.
func (e *Engine) probeUpstream(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions) {
	up := upstreamFor(manifestImage(man, b))
	if up == nil || len(up.Probe) == 0 {
		return
	}
	out, err := dockercli.ExecHook(ctx, cli, opts.TargetID, up.Probe, "", "")
	verdict := strings.TrimSpace(string(out))
	if err != nil {
		e.logf(b.ID, "INFO", "Could not check whether %s is reachable from the restored container (%v%s) — the restore itself is complete; check the connection in the application's settings",
			up.Service, err, detailSuffix(verdict))
		return
	}
	if up.ProbeOK != "" && strings.Contains(verdict, up.ProbeOK) {
		e.logf(b.ID, "INFO", "%s is reachable and identified itself as the same server this backup was connected to — nothing needed re-authenticating", up.Service)
		return
	}
	e.logf(b.ID, "WARN", "%s did not confirm as expected (%s). Left unresolved, %s. The restored data is fine — this is about the connection to %s, which you can set in the application's own settings",
		up.Service, shortVerdict(verdict), strings.TrimRight(up.Symptom, "."), up.Service)
}

// maxVerdictLen bounds what a probe's output can put into a run log. A probe is
// specified to print a verdict; anything longer is a program misbehaving, and a
// log is not the place to find out how much it can print.
const maxVerdictLen = 200

func shortVerdict(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "it printed nothing"
	}
	if len(s) > maxVerdictLen {
		return s[:maxVerdictLen] + "…"
	}
	return s
}

// UpstreamPrompt is the label for the optional new-dependency-address field a
// restore dialog offers, or "" for an application that depends on nothing whose
// address it records (F160).
func UpstreamPrompt(image string) string {
	up := upstreamFor(image)
	if up == nil {
		return ""
	}
	return fmt.Sprintf("New address for %s (leave blank if %s has not moved)", up.Service, up.Service)
}
