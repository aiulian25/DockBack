package backup

import (
	"strings"
)

// Addresses an application carries in its environment, for applications DockBack
// has no profile knowledge of (F175).
//
// The profile registry names the variables it can REWRITE — OVERWRITEHOST for
// Nextcloud, APP_URL for BookStack, and so on — and a restore given a new
// address corrects those automatically. That list will always be shorter than
// the set of applications people run.
//
// The failure does not care whether DockBack knows the application. Any
// container whose environment records where it lives keeps recording the
// PREVIOUS machine after a move: the restore is faithful, every service comes
// up, and the site answers by redirecting to a host that is somewhere else. What
// makes it expensive is the silence — nothing in the run log mentions an address
// at all, so the operator looks at DNS, at the proxy, at the tunnel, and only
// eventually at a variable that has been sitting in the compose file the whole
// time.
//
// So the generic half REPORTS rather than rewrites. Reporting needs no knowledge
// of the application and cannot break one; rewriting an arbitrary variable
// because its name looks like an address is exactly the kind of guess that turns
// a good restore into a broken app.

// addressKeySuffixes are the name endings that mark a variable as recording
// where something lives: APP_URL, JELLYFIN_PublishedServerUrl, ADVERTISE_IP,
// OVERWRITEHOST, PAPERLESS_CSRF_TRUSTED_ORIGINS.
//
// Matched against the END of the name's LAST word rather than anywhere in it,
// because a plain substring match is how DISK_USAGE_URI_CACHE and
// CLIENT_HOSTS_DENIED end up in a list nobody then reads — while the ending rule
// still catches the run-together names applications really use, OVERWRITEHOST
// among them. A trailing "S" is stripped first, so the plural forms every trust
// list uses are covered by the singular entry.
var addressKeySuffixes = []string{
	"URL", "URI", "HOST", "HOSTNAME", "DOMAIN", "ORIGIN", "ADDR", "ADDRESS",
	"ENDPOINT", "FQDN", "IP", "SITE",
}

// secretKeySubstrings mark a variable whose value must never reach the run log,
// which is a surface operators paste into help requests. Matched anywhere in the
// name and checked FIRST, so anything ambiguous is treated as a secret.
//
// "AUTH" is deliberately NOT here. It appears in NEXTAUTH_URL — an ordinary
// address, and one of the exact variables a move invalidates — while the secret
// beside it is NEXTAUTH_SECRET, which "SECRET" already catches. The value filter
// below is what stops a credential-bearing AUTH URL from being printed.
var secretKeySubstrings = []string{
	"PASS", "SECRET", "TOKEN", "KEY", "CRED", "SALT", "HASH", "DSN", "PRIVATE", "SIGNING", "LICENSE",
}

// maxReportedAddressValue bounds what is printed. A short address is the thing
// this is for; a long value is a connection string or a blob, and neither
// belongs in a log.
const maxReportedAddressValue = 120

// addressLikeEnv returns the environment entries that record an address, as
// "KEY=value" strings ready to print.
//
// declared names the keys a profile already handles, which are excluded — they
// have either been corrected already or been reported by name, and saying them
// twice makes the list that matters harder to read.
func addressLikeEnv(env []string, declared map[string]bool) []string {
	var out []string
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" || v == "" || declared[k] {
			continue
		}
		if !addressLikeKey(k) {
			continue
		}
		shown, ok := reportableAddressList(v)
		if !ok {
			continue
		}
		out = append(out, k+"="+shown)
	}
	return out
}

// addressLikeKey reports whether a variable's NAME says it records a location,
// and that its name does not also say it holds a secret.
func addressLikeKey(k string) bool {
	up := strings.ToUpper(k)
	for _, s := range secretKeySubstrings {
		if strings.Contains(up, s) {
			return false
		}
	}
	// The last word of the name, with camelCase treated as word boundaries so
	// PublishedServerUrl ends in "URL" the same way PUBLISHED_SERVER_URL does.
	last := lastWord(k)
	last = strings.TrimSuffix(last, "S") // HOSTS, ORIGINS, DOMAINS
	for _, suffix := range addressKeySuffixes {
		// A suffix of the last word, not of the whole name: that is what
		// separates OVERWRITEHOST and SERVERURL — run-together names an
		// application really does use — from DISK_USAGE_URI_CACHE, where the
		// address word is in the middle and the variable is about something else.
		if strings.HasSuffix(last, suffix) {
			return true
		}
	}
	return false
}

// lastWord returns the final word of an environment variable name, uppercased.
// Words break on non-letters and on a lowercase-to-uppercase transition.
func lastWord(k string) string {
	start := 0
	runes := []rune(k)
	for i, r := range runes {
		lower := r >= 'a' && r <= 'z'
		upper := r >= 'A' && r <= 'Z'
		switch {
		case !lower && !upper:
			start = i + 1
		case upper && i > 0 && runes[i-1] >= 'a' && runes[i-1] <= 'z':
			start = i
		}
	}
	return strings.ToUpper(string(runes[start:]))
}

// reportableAddressList handles a value holding SEVERAL addresses — a CORS
// origin list, a set of allowed hosts — by judging each one and keeping the ones
// worth reporting.
//
// The whole variable is dropped when nothing in it survives. That is what stops
// DJANGO_ALLOWED_HOSTS=localhost,127.0.0.1 from taking up a line: it is a real
// list of real addresses, and not one of them changes when the container moves.
func reportableAddressList(v string) (string, bool) {
	sep := ","
	if !strings.Contains(v, ",") && strings.Contains(v, " ") {
		sep = " "
	}
	var kept []string
	for _, part := range strings.Split(v, sep) {
		if shown, ok := reportableAddress(strings.TrimSpace(part)); ok {
			kept = append(kept, shown)
		}
	}
	if len(kept) == 0 {
		return "", false
	}
	return strings.Join(kept, sep), true
}

// reportableAddress returns the part of a value that is safe and useful to
// print, and whether it is an address at all.
//
// Only the scheme and the authority survive. A path is dropped, and its absence
// marked with a slash and an ellipsis, for a reason worth stating plainly: a
// Slack or Discord webhook is a URL whose PATH is the credential, and it lives
// under a key called WEBHOOK_URL. Printing the host and dropping the rest keeps
// every line useful for finding a stale machine name while making it impossible
// for a secret carried in a path or a query string to reach the log.
func reportableAddress(v string) (string, bool) {
	if len(v) > maxReportedAddressValue || strings.ContainsAny(v, " \t\n\"'`$") {
		return "", false
	}
	scheme, rest := "", v
	if i := strings.Index(v, "://"); i >= 0 {
		scheme, rest = v[:i+3], v[i+3:]
	}
	authority, tail, hadPath := strings.Cut(rest, "/")
	if q := strings.IndexAny(authority, "?#"); q >= 0 {
		authority, hadPath = authority[:q], true
	}
	if authority == "" || strings.Contains(authority, "@") {
		return "", false // credentials in the authority, or no authority at all
	}
	if !namesAMachine(authority) {
		return "", false
	}
	out := scheme + authority
	if hadPath && strings.TrimSpace(tail) != "" {
		out += "/…"
	} else if hadPath {
		out += "/"
	}
	return out, true
}

// namesAMachine reports whether an authority identifies a particular machine —
// the only kind of address a move can invalidate.
//
// Everything it rejects was measured against the real environments of the
// containers on a working host, where the first version of this reported roughly
// twice as many lines as it should have. Noise is not a cosmetic problem here:
// the run log is where the one stale address has to be noticed, and a list where
// most entries are irrelevant is a list that gets skimmed.
//
//   - A wildcard (0.0.0.0, ::, *) is "listen on everything". It is a bind
//     address, never a site address, and it is correct on every machine.
//   - Loopback is the same on every machine by definition.
//   - A single-label host is a service name on the compose network — redis,
//     mediaapp-es, socket-proxy. Docker resolves it identically on the new host,
//     which is the entire point of naming it that way.
//
// What survives is a dotted name, an IP, or an IPv6 literal: something that
// points at one machine in particular.
func namesAMachine(authority string) bool {
	host := authority
	if strings.HasPrefix(host, "[") { // [2001:db8::1]:8443
		if end := strings.Index(host, "]"); end > 0 {
			return end > 1 // a bracketed literal is an address
		}
		return false
	}
	if h, _, found := strings.Cut(host, ":"); found {
		host = h
	}
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "", "0.0.0.0", "::", "*", "localhost", "127.0.0.1", "::1", "host.docker.internal":
		return false
	}
	return strings.Contains(host, ".")
}

// declaredAddressKeys is the set of env keys a profile already rewrites, so the
// generic report does not repeat them.
func declaredAddressKeys(p *AppProfile) map[string]bool {
	out := map[string]bool{}
	if p == nil {
		return out
	}
	for _, bind := range p.Address {
		if bind.Kind != BindEnv {
			continue
		}
		for _, k := range bind.Keys {
			out[k] = true
		}
	}
	return out
}

// AddressEnvKeys names the environment variables a backed-up container records
// an address in, from the manifest alone (F177).
//
// The post-restore report is the one that can show VALUES, because it reads the
// live container. This one runs before anything is touched, from the keys the
// manifest carries — values are dropped at that boundary precisely because a
// container environment holds passwords, and the manifest sidecar is readable.
//
// Keys alone are still worth saying. "This container records an address in
// APP_URL" told before a cross-host restore is the difference between filling in
// the new address in the dialog and finding out a week later why the site
// redirects to a machine that has moved.
//
// Empty for a backup too old to have recorded environment keys — no evidence is
// not the same as nothing to report, and it must not render as the latter.
func AddressEnvKeys(man *Manifest) []string {
	if man == nil {
		return nil
	}
	var out []string
	for _, k := range man.ContainerEnvKeys {
		if addressLikeKey(strings.TrimSpace(k)) {
			out = append(out, k)
		}
	}
	return out
}
