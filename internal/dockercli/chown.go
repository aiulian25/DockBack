package dockercli

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// Ownership alignment for restored data (F117).
//
// The single most common way a restored self-hosted app comes up broken is not a
// bad backup — it is ownership. Images built on the LinuxServer base (and most
// others that expose PUID/PGID) drop privileges to that user and then cannot
// write their own database. SQLite reports it as "attempt to write a readonly
// database" or "database is locked", which reads like corruption and is not.
//
// A numeric restore preserves the SOURCE machine's uid:gid, which is exactly
// right when the target runs the same ids and exactly wrong when it does not —
// a Synology host whose user is 1026 restoring files owned 1000 gets a
// read-only library. So the ids the container will actually run as are read
// from the target, and the restored paths are aligned to them.

// ChownVolumePaths recursively sets ownership of paths inside a container's
// volumes to uid:gid, via a read-write sidecar. Returns the number of paths it
// attempted.
//
// uid/gid are ints, not strings, so nothing user-supplied can reach the command
// as anything but a number — and the command is argv, with no shell involved.
func ChownVolumePaths(ctx context.Context, c *client.Client, targetID string, paths []string, uid, gid int) (int, error) {
	if len(paths) == 0 {
		return 0, nil
	}
	if uid < 0 || gid < 0 {
		return 0, fmt.Errorf("refusing to change ownership to a negative id (%d:%d)", uid, gid)
	}
	if err := ensureSidecar(ctx, c); err != nil {
		return 0, err
	}

	// `chown -Rh` follows no symlinks, so a link inside the restored data cannot
	// redirect the change onto something outside it. Missing paths are tolerated:
	// a container legitimately may not have every recorded mount.
	args := []string{"chown", "-Rh", strconv.Itoa(uid) + ":" + strconv.Itoa(gid)}
	kept := 0
	for _, p := range paths {
		if p = strings.TrimSpace(p); p == "" || !strings.HasPrefix(p, "/") {
			continue
		}
		args = append(args, p)
		kept++
	}
	if kept == 0 {
		return 0, nil
	}

	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: args, AttachStdout: true, AttachStderr: true, Labels: sidecarLabels()},
		&container.HostConfig{VolumesFrom: []string{targetID}}, nil, nil, "")
	if err != nil {
		return 0, fmt.Errorf("chown sidecar create: %w", err)
	}
	sidecarID := created.ID
	defer removeContainer(c, sidecarID)

	att, err := c.ContainerAttach(ctx, sidecarID, container.AttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		return 0, fmt.Errorf("chown attach: %w", err)
	}
	defer att.Close()
	if err := c.ContainerStart(ctx, sidecarID, container.StartOptions{}); err != nil {
		return 0, fmt.Errorf("chown start: %w", err)
	}
	var stdout, stderr bytes.Buffer
	done := make(chan struct{})
	go func() { _, _ = stdcopy.StdCopy(&stdout, &stderr, att.Reader); close(done) }()

	waitCh, errCh := c.ContainerWait(ctx, sidecarID, container.WaitConditionNotRunning)
	select {
	case st := <-waitCh:
		<-done
		if st.StatusCode != 0 {
			// chown exits non-zero for a path it could not touch even when it
			// changed the rest, so the message carries what it actually said.
			return kept, fmt.Errorf("chown exited %d: %s", st.StatusCode, strings.TrimSpace(lastLine(stderr.String())))
		}
	case e := <-errCh:
		return kept, e
	case <-time.After(10 * time.Minute):
		return kept, fmt.Errorf("changing ownership timed out")
	}
	return kept, nil
}

// lastLine returns the final non-empty line, so an error message is the useful
// part rather than a wall of per-file noise.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// runAsEnvKeys are the environment variables an image uses to say which user it
// will drop privileges to, most conventional first.
//
// PUID/PGID is the LinuxServer convention and by far the most common; the others
// cover images that adopted the idea with their own names. The pair must come
// from the SAME convention — mixing PUID with GROUP_ID would produce a plausible
// pair of numbers that no image ever runs as.
var runAsEnvKeys = [][2]string{
	{"PUID", "PGID"},
	// F182: paperless-ngx's convention, and the one a Synology deployment is most
	// likely to have set — its shares are owned by an account like 1026:100, so
	// the compose file carries those ids and a move to an ordinary Linux host
	// changes them to 1000:1000. Without this pair the alignment found nothing to
	// align to and said nothing, and the restored documents stayed owned by a uid
	// that does not exist on the new machine.
	{"USERMAP_UID", "USERMAP_GID"},
	{"UID", "GID"},
	{"USER_ID", "GROUP_ID"},
}

// Matching the env-configurable UID model by SEMANTICS rather than by name (#5).
//
// The explicit list above is four conventions somebody thought to write down.
// R4's takeaway is that the list is the wrong shape: "Detect the env-configurable
// model by semantics, not name: any variable pair matching `*UID`/`*GID`
// (`PUID`, `USERMAP_UID`, `UID`, `RUN_AS_UID`)." USERMAP_UID had to be added by
// hand after a restore left a Synology's documents owned by a uid that did not
// exist on the new machine — the list found nothing to align to and said nothing.
// The next convention would have cost the same discovery.
//
// The PAIR is what makes this safe to generalise. A key ending in UID proves
// nothing on its own — SQUID=3128 is a port, LIQUID_UID would be anybody's
// guess — but a key ending in UID whose same-prefix GID counterpart is also
// present, with both holding non-negative integers, is an image declaring which
// user it drops to. Nothing else is shaped like that.

// genericRunAsPair finds a `<prefix>UID` / `<prefix>GID` pair the explicit list
// does not know about.
//
// Deliberately the SAME helper behind both public functions, so the one that
// READS the ids and the one that REWRITES them can never settle on different
// variables — an alignment that chowns to one pair while the recreate rewrites
// another is worse than either alone.
//
// Deterministic: candidates are sorted, so an environment declaring two such
// pairs gives the same answer on every run rather than whichever the map
// iteration reached first.
func genericRunAsPair(values map[string]string) (uidKey, gidKey string, uid, gid int, ok bool) {
	candidates := make([]string, 0, len(values))
	for key := range values {
		if strings.HasSuffix(key, uidSuffix) {
			candidates = append(candidates, key)
		}
	}
	sort.Strings(candidates)
	for _, candidate := range candidates {
		counterpart := strings.TrimSuffix(candidate, uidSuffix) + gidSuffix
		u, uerr := strconv.Atoi(strings.TrimSpace(values[candidate]))
		g, gerr := strconv.Atoi(strings.TrimSpace(values[counterpart]))
		// Both halves, both numbers, neither negative. A lone half is not a pair,
		// and a value that is not an id is not something to chown to.
		if uerr != nil || gerr != nil || u < 0 || g < 0 {
			continue
		}
		return candidate, counterpart, u, g, true
	}
	return "", "", 0, 0, false
}

const (
	uidSuffix = "UID"
	gidSuffix = "GID"
)

// RunAsEnvPair names the two variables this container uses to say which user it
// runs as, so a restore can REWRITE them rather than only read them (F189).
//
// Same convention rules as RunAsIDs: the pair has to come from one convention,
// and an image declaring none returns ok=false — there is nothing to rewrite,
// and inventing a pair would be telling an image to honour a variable it has
// never heard of.
func RunAsEnvPair(env []string) (uidKey, gidKey string, ok bool) {
	m := make(map[string]string, len(env))
	for _, e := range env {
		if k, v, found := strings.Cut(e, "="); found {
			m[k] = strings.TrimSpace(v)
		}
	}
	for _, pair := range runAsEnvKeys {
		if m[pair[0]] != "" && m[pair[1]] != "" {
			return pair[0], pair[1], true
		}
	}
	// #5: a convention nobody wrote down. Only after the explicit list, so a
	// container declaring PUID keeps being rewritten through PUID.
	if uidKey, gidKey, _, _, found := genericRunAsPair(m); found {
		return uidKey, gidKey, true
	}
	return "", "", false
}

// RunAsIDs reads the uid/gid an image will run its application as, from the
// container's own environment (F117).
//
// Returns ok=false when the image declares none — most images don't, and for
// those there is nothing to align to and nothing to guess.
func RunAsIDs(env []string) (uid, gid int, key string, ok bool) {
	m := make(map[string]string, len(env))
	for _, e := range env {
		if k, v, found := strings.Cut(e, "="); found {
			m[k] = strings.TrimSpace(v)
		}
	}
	for _, pair := range runAsEnvKeys {
		uv, gv := m[pair[0]], m[pair[1]]
		if uv == "" || gv == "" {
			continue
		}
		u, uerr := strconv.Atoi(uv)
		g, gerr := strconv.Atoi(gv)
		// A non-numeric or negative value is not something to act on: chowning to
		// a guess is worse than leaving ownership alone.
		if uerr != nil || gerr != nil || u < 0 || g < 0 {
			continue
		}
		return u, g, pair[0] + "/" + pair[1], true
	}
	// #5: same fallback, same helper — the key that matched is reported either
	// way, so the run log names the variable it actually read.
	if uidKey, gidKey, u, g, found := genericRunAsPair(m); found {
		return u, g, uidKey + "/" + gidKey, true
	}
	return 0, 0, "", false
}
