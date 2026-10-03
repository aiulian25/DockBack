package dockercli

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/docker/docker/api/types"
)

// Domain remap for a cross-host restore (F195).
//
// The profile registry knows how a handful of applications record their address
// and rewrites exactly the right settings. Every other container gets this: the
// same mechanism as the machine-IP remap, applied to a DOMAIN. The operator
// says "car.example.uk is now car.new.uk" and every environment value that
// carries the old name — a base URL, a CORS list, a trusted-origin entry —
// gets the new one, with the scheme, port and path around it untouched.
//
// A literal from → to rewrite is chosen over guessing which variable "is the
// address" on purpose. APP_URL and OLLAMA_BASE_URL both end in URL; one is the
// container's own address and the other is an upstream on a different machine,
// and no rule over the NAME can tell them apart. The old domain can: only
// values that actually carry it change, so an upstream pointing elsewhere is
// never touched.

// hostnameRe is the grammar a remap endpoint must fit: DNS labels joined by
// dots, no wildcard, no scheme, no port, no path. These values end up inside a
// regexp and in shell-adjacent surfaces, so anything outside the grammar is
// rejected rather than escaped into place.
var hostnameRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+$`)

// ValidRemapDomain reports whether a value can serve as a domain-remap
// endpoint.
func ValidRemapDomain(d string) error {
	d = strings.TrimSpace(d)
	if d == "" {
		return fmt.Errorf("domain remap needs both a current and a new domain")
	}
	if !hostnameRe.MatchString(d) {
		return fmt.Errorf("%q is not a plain domain name — no scheme, port, path or wildcard, e.g. cloud.example.com", d)
	}
	return nil
}

// domainBoundaryRegexp matches the domain as a whole host: not preceded or
// followed by a character that would make it part of a LONGER name.
//
// The look-around rules are the whole point. Replacing car.example.uk must not
// touch oscar.example.uk (prefix) or car.example.uk.internal (suffix) — and it
// deliberately does not touch www.car.example.uk either: a subdomain is a
// DIFFERENT host, and whether it moves with the parent is not something a
// backup tool can know. Exact hosts only; every change is logged; run again
// with the subdomain if it moved too.
func domainBoundaryRegexp(domain string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[^A-Za-z0-9.-])` + regexp.QuoteMeta(domain) + `($|[^A-Za-z0-9.-])`)
}

func replaceDomain(s, from, to string, re *regexp.Regexp) (string, int) {
	// Adjacent occurrences share their boundary character ("a.uk,a.uk"), and a
	// single regexp pass consumes it with the first match, skipping the second.
	// Passes repeat until nothing changes; the bound is paranoia, not need.
	total := 0
	for range [4]int{} {
		n := 0
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			n++
			return strings.Replace(m, from, to, 1)
		})
		total += n
		if n == 0 {
			break
		}
	}
	return s, total
}

// RemapContainerHostDomain rewrites a container's saved inspect JSON so values
// carrying the old domain carry the new one. Environment values only — a
// domain does not appear in port bindings, and labels are Compose's and the
// image's to manage.
func RemapContainerHostDomain(inspectJSON []byte, from, to string) ([]byte, int, error) {
	if err := ValidRemapDomain(from); err != nil {
		return inspectJSON, 0, err
	}
	if err := ValidRemapDomain(to); err != nil {
		return inspectJSON, 0, err
	}
	if strings.EqualFold(from, to) {
		return inspectJSON, 0, nil
	}
	var insp types.ContainerJSON
	if err := json.Unmarshal(inspectJSON, &insp); err != nil {
		return inspectJSON, 0, err
	}
	if insp.Config == nil {
		return inspectJSON, 0, nil
	}
	re := domainBoundaryRegexp(from)
	changes := 0
	for i, e := range insp.Config.Env {
		if n, c := replaceDomain(e, from, to, re); c > 0 {
			insp.Config.Env[i] = n
			changes += c
		}
	}
	if changes == 0 {
		return inspectJSON, 0, nil
	}
	out, err := json.MarshalIndent(&insp, "", "  ")
	if err != nil {
		return inspectJSON, 0, err
	}
	return out, changes, nil
}

// RemapTextHostDomain is the same rewrite over plain text, for the archived
// compose fallback.
func RemapTextHostDomain(data []byte, from, to string) ([]byte, int) {
	if ValidRemapDomain(from) != nil || ValidRemapDomain(to) != nil || strings.EqualFold(from, to) {
		return data, 0
	}
	re := domainBoundaryRegexp(from)
	out, n := replaceDomain(string(data), from, to, re)
	if n == 0 {
		return data, 0
	}
	return []byte(out), n
}
