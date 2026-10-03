package dockercli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
)

// HostIPFromDockerAddress extracts a literal IPv4/IPv6 host from a DOCKER_HOST-style
// node address (e.g. "ssh://user@10.168.1.178:22", "tcp://10.0.0.5:2375"). It
// returns "" when the address has no host, is unparseable, or is addressed by a
// name rather than an IP (e.g. "tcp://socket-proxy:2375", "unix:///var/run/...").
// Used to prefill the source/target machine IPs for a cross-host restore remap.
func HostIPFromDockerAddress(addr string) string {
	if addr == "" {
		return ""
	}
	u, err := url.Parse(addr)
	if err != nil {
		return ""
	}
	h := u.Hostname()
	if net.ParseIP(h) != nil {
		return h
	}
	return ""
}

// ResolveHostIP is HostIPFromDockerAddress plus a DNS lookup for a node
// registered by HOSTNAME (F197).
//
// "We already have the machine's IP" is true even then — it is one resolution
// away, and a node addressed as tcp://hp.lan:2375 is exactly the node whose
// operator most needs the remap derived for them. IPv4 is preferred because the
// remap's job is rewriting the RFC1918 addresses people pin in compose files;
// an IPv6-only answer is returned rather than nothing.
//
// Empty when nothing resolves. The caller decides what that means — for an
// opt-in remap it must be a refusal, never a silent no-op.
func ResolveHostIP(ctx context.Context, addr string) string {
	if ip := HostIPFromDockerAddress(addr); ip != "" {
		return ip
	}
	u, err := url.Parse(addr)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	if host == "" {
		// A bare "hp:2375" parses with no scheme and no hostname.
		if h, _, splitErr := net.SplitHostPort(addr); splitErr == nil {
			host = h
		} else {
			host = addr
		}
	}
	if host == "" || strings.ContainsAny(host, "/ ") {
		return ""
	}
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(rctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return ""
	}
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			return v4.String()
		}
	}
	return ips[0].String()
}

// ipBoundaryRegexp matches an exact IP token, not a longer IP that merely contains
// it — so remapping "10.168.1.1" never rewrites the "10.168.1.1" inside
// "10.168.1.10". \b sits at both ends; the interior dots are matched literally.
func ipBoundaryRegexp(ip string) *regexp.Regexp {
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(ip) + `\b`)
}

// RemapTextHostIP replaces every exact occurrence of fromIP with toIP in a text
// blob (used for the reconstructed compose file), returning the new bytes and the
// number of substitutions. A no-op (0 changes, original bytes) when the IPs are
// invalid, equal, or absent.
func RemapTextHostIP(data []byte, fromIP, toIP string) ([]byte, int) {
	if net.ParseIP(fromIP) == nil || net.ParseIP(toIP) == nil || fromIP == toIP {
		return data, 0
	}
	re := ipBoundaryRegexp(fromIP)
	n := len(re.FindAll(data, -1))
	if n == 0 {
		return data, 0
	}
	return re.ReplaceAll(data, []byte(toIP)), n
}

// RemapContainerHostIP rewrites a container's saved `docker inspect` JSON so that a
// SOURCE machine IP is replaced by the TARGET machine IP wherever it appears in the
// bits that break a cross-host restore:
//
//   - published-port host IPs (HostConfig.PortBindings[*].HostIP) — a port pinned
//     to an IP that doesn't exist on the new host fails to bind, so the container
//     won't start; remapping to the target's own IP preserves the LAN-specific
//     binding rather than widening it to all interfaces;
//   - environment values (Config.Env) — self-referential advertise/URL vars;
//   - extra host entries (HostConfig.ExtraHosts, "name:ip").
//
// It returns the (possibly rewritten) JSON and the number of substitutions. On any
// problem — invalid IPs, equal IPs, unparseable JSON — it returns the original
// bytes with 0 changes (and an error only for genuinely bad input), so a restore is
// never blocked by a best-effort remap.
func RemapContainerHostIP(inspectJSON []byte, fromIP, toIP string) ([]byte, int, error) {
	if net.ParseIP(fromIP) == nil || net.ParseIP(toIP) == nil {
		return inspectJSON, 0, fmt.Errorf("remap needs two valid IP addresses")
	}
	if fromIP == toIP {
		return inspectJSON, 0, nil
	}
	var insp types.ContainerJSON
	if err := json.Unmarshal(inspectJSON, &insp); err != nil {
		return inspectJSON, 0, err
	}
	re := ipBoundaryRegexp(fromIP)
	changes := 0

	if insp.HostConfig != nil {
		// Published-port host IPs — exact match (a HostIP is a single address).
		for _, binds := range insp.HostConfig.PortBindings {
			for i := range binds {
				if binds[i].HostIP == fromIP {
					binds[i].HostIP = toIP
					changes++
				}
			}
		}
		// Extra hosts: "name:ip".
		for i, eh := range insp.HostConfig.ExtraHosts {
			if n := re.ReplaceAllString(eh, toIP); n != eh {
				insp.HostConfig.ExtraHosts[i] = n
				changes++
			}
		}
	}
	if insp.Config != nil {
		for i, e := range insp.Config.Env {
			if n := re.ReplaceAllString(e, toIP); n != e {
				insp.Config.Env[i] = n
				changes++
			}
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
