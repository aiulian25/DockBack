package dockercli

import (
	"bytes"
	"context"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// Post-restore forensics (F91).
//
// When a restore's health gate failed, DockBack said "the container did not
// become healthy" and stopped there — capturing no evidence at all. Diagnosing a
// real failure therefore meant SSHing to the host and running `docker logs`, and
// the actual cause (`ImproperlyConfigured: PAPERLESS_SECRET_KEY is not set`) was
// only ever visible there.
//
// The container's own last words are the single most useful artefact at that
// moment, so they are pulled into the run log and the alert instead.

// maxLogTailBytes caps what is read back. Trimmed from the FRONT when it
// overflows: a container that fails on startup says why in its final lines, so
// the tail is the part worth keeping.
const maxLogTailBytes = 8 << 10

// ContainerLogTail returns the last `lines` of a container's combined
// stdout/stderr, demuxed from Docker's stream framing.
//
// Best-effort by design — every caller is already on a failure path, and a
// logs-read problem must never become the reported cause of that failure.
func ContainerLogTail(ctx context.Context, c *client.Client, id string, lines int) (string, error) {
	if id == "" || lines <= 0 {
		return "", nil
	}
	// Its own short deadline: the container may be unhealthy or wedged, and this
	// runs while an operator is watching a restore fail.
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	rc, err := c.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true, ShowStderr: true, Tail: strconv.Itoa(lines),
	})
	if err != nil {
		return "", err
	}
	defer rc.Close()

	// A container started with TTY has no stream framing, so StdCopy fails on it.
	// Read once and demux only if the header is actually present.
	raw, err := io.ReadAll(io.LimitReader(rc, 4*maxLogTailBytes))
	if err != nil && len(raw) == 0 {
		return "", err
	}
	out := demuxDockerLog(raw)
	return trimToTail(strings.TrimRight(out, "\n"), maxLogTailBytes), nil
}

// demuxDockerLog strips Docker's 8-byte stream headers when present, and passes
// TTY output (which has none) through unchanged.
func demuxDockerLog(raw []byte) string {
	// A framed stream starts with a stream byte of 0-2 followed by three zero
	// bytes. Anything else is raw TTY output.
	if len(raw) >= 8 && raw[0] <= 2 && raw[1] == 0 && raw[2] == 0 && raw[3] == 0 {
		var out, errOut bytes.Buffer
		if _, derr := stdcopy.StdCopy(&out, &errOut, bytes.NewReader(raw)); derr == nil {
			// stderr last: a crash message is what the reader is looking for.
			return out.String() + errOut.String()
		}
	}
	return string(raw)
}

// trimToTail keeps the last max bytes, cutting at a line boundary so the excerpt
// never starts mid-word.
func trimToTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[len(s)-max:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
		s = s[i+1:]
	}
	return "…(earlier output trimmed)\n" + s
}

// LastLines returns at most n trailing lines of s — used to keep an alert body
// short while the run log keeps the full excerpt.
func LastLines(s string, n int) string {
	if s == "" || n <= 0 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// secretAssign matches `KEY=value` / `KEY: value` so a credential printed in a
// startup banner can be masked.
var secretAssign = regexp.MustCompile(`(?i)\b([A-Za-z0-9_.\-]*(?:password|passwd|secret|token|api[_-]?key|access[_-]?key|credential)s?[A-Za-z0-9_.\-]*)\s*([=:])\s*("?[^\s"',;]+"?)`)

// urlCredential matches credentials embedded in a URL (postgres://user:pass@host).
var urlCredential = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^\s:/@]+):([^\s@]+)@`)

// RedactLogSecrets masks credential-looking values in text destined to LEAVE the
// machine.
//
// Container startup output routinely contains connection strings and API keys,
// and a notification goes to a Gotify server, an SMTP relay or an arbitrary
// webhook URL. The full, unredacted excerpt still goes to the run log, which
// stays on the operator's own instance — so nothing is lost for diagnosis.
//
// Heuristic and therefore imperfect: it catches the common shapes, not every
// possible one. That is why it guards the OUTBOUND copy only, rather than being
// relied on as a boundary.
func RedactLogSecrets(s string) string {
	s = secretAssign.ReplaceAllString(s, "$1$2«redacted»")
	return urlCredential.ReplaceAllString(s, "$1:«redacted»@")
}
