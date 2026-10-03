package dockercli

import (
	"context"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"
)

// MaxComposeFetchBytes caps a single host-file read over SSH (F57). The genuine
// compose file is small; the cap stops a hostile/huge path from streaming
// unbounded data into memory.
const MaxComposeFetchBytes int64 = 1 << 20 // 1 MiB

// fetchPathRe is the ONLY shape of host path we will read over SSH: letters,
// digits, dot, underscore, slash, space, hyphen. It deliberately excludes every
// shell metacharacter (`;` `|` `&` `$` backtick `'` `"` `\` `<` `>` newline …), so
// the validated path can be single-quoted into the remote command with no
// possibility of injection.
var fetchPathRe = regexp.MustCompile(`^[A-Za-z0-9._/ -]+$`)

// ValidateFetchPath is the exported face of the host-file path rules, so a
// reader that does NOT go over SSH still enforces exactly the same grammar.
//
// One rule, one implementation. The alternative — a second validator beside the
// sidecar reader — would mean the path a backup is willing to read depends on
// which transport a node happens to use, and the looser of the two would be the
// one nobody noticed.
func ValidateFetchPath(p string) error { return validateFetchPath(p) }

// validateFetchPath enforces the strict host-path rules for an SSH file read (F57):
// absolute, no ".." segment, no disallowed/shell-metacharacter bytes, bounded
// length. Pure, so every injection/traversal shape is unit-tested.
func validateFetchPath(p string) error {
	if p == "" {
		return fmt.Errorf("empty path")
	}
	if len(p) > 4096 {
		return fmt.Errorf("path too long")
	}
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("path must be absolute")
	}
	if !fetchPathRe.MatchString(p) {
		return fmt.Errorf("path contains disallowed characters")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("path must not contain '..'")
		}
	}
	return nil
}

// TransportFor returns a node's configured transport ("ssh"/"tcp-proxy"/…), or ""
// when the node is unknown. Lets the engine decide whether a host-file read is even
// possible for a node (only SSH nodes hold host credentials).
func (r *Registry) TransportFor(nodeID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if spec, ok := r.specs[nodeID]; ok {
		return spec.Transport
	}
	return ""
}

// FetchNodeFile reads a small host file from an SSH-transport node over the SAME
// pinned, authenticated SSH connection the Docker tunnel uses (F57). It is the ONLY
// entry point for reading host files — it enforces: SSH transport only, a strictly
// validated path, a read cap, and a non-shell `cat` of a single-quoted path (safe
// because the validator forbids quotes/metacharacters). Read-only; it never writes
// to the host. Returns an error for any non-SSH node so the host-FS boundary of the
// socket-proxy transports stays exactly where it is.
func (r *Registry) FetchNodeFile(ctx context.Context, nodeID, path string, maxBytes int64) ([]byte, error) {
	if err := validateFetchPath(path); err != nil {
		return nil, err
	}
	if maxBytes <= 0 || maxBytes > MaxComposeFetchBytes {
		maxBytes = MaxComposeFetchBytes
	}

	r.mu.Lock()
	spec, ok := r.specs[nodeID]
	tunnel := r.tunnels[nodeID]
	r.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown node %q", nodeID)
	}
	if spec.Transport != TransportSSH {
		return nil, fmt.Errorf("host-file read is only supported over the SSH transport")
	}
	// Reuse the live tunnel when present (its ssh.Client multiplexes sessions with
	// the Docker socket channels); otherwise build a throwaway one that still pins
	// the host key via the registry's verifier.
	throwaway := false
	if tunnel == nil {
		t, err := newSSHTunnel(spec.Address, ParseSSHCreds(spec.Secret), r.pinningVerifier(spec.ID))
		if err != nil {
			return nil, err
		}
		tunnel = t
		throwaway = true
	}
	cli, err := tunnel.sshClient()
	if err != nil {
		if throwaway {
			tunnel.close()
		}
		return nil, err
	}
	if throwaway {
		defer tunnel.close()
	}

	sess, err := cli.NewSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close()

	stdout, err := sess.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	sess.Stderr = &stderr
	// The path is validated (no shell metacharacters/quotes possible) and
	// single-quoted, so this cannot be broken out of by any legal path. `--`
	// prevents a leading '-' being read as a flag.
	if err := sess.Start("cat -- '" + path + "'"); err != nil {
		return nil, err
	}

	// Read up to maxBytes+1 so an oversized file is detected without buffering it all.
	data, rerr := io.ReadAll(io.LimitReader(stdout, maxBytes+1))
	if rerr != nil {
		return nil, rerr
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("file exceeds the %d-byte cap", maxBytes)
	}
	// Full file read within the cap — confirm the remote command actually succeeded
	// (a missing file / permission error exits non-zero with empty stdout).
	if werr := sess.Wait(); werr != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, fmt.Errorf("reading %s: %w (%s)", path, werr, msg)
	}
	return data, nil
}

// sshClient returns the tunnel's live *ssh.Client, connecting it if needed. Used to
// open an ancillary session (F57 host-file read) over the same pinned connection.
func (t *sshTunnel) sshClient() (*ssh.Client, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.client != nil {
		return t.client, nil
	}
	tcp, err := (&net.Dialer{Timeout: dialTimeout, KeepAlive: tcpKeepAlive}).Dial("tcp", t.hostport)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", t.hostport, err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(tcp, t.hostport, t.cfg)
	if err != nil {
		tcp.Close()
		return nil, fmt.Errorf("ssh handshake %s: %w", t.hostport, err)
	}
	t.client = ssh.NewClient(sshConn, chans, reqs)
	return t.client, nil
}
