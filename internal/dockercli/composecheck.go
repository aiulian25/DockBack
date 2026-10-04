package dockercli

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
)

// Checking compose files with the real Compose CLI (step 15). In the
// 2026-10-04 recovery every compose file a restore wrote was run through
// `docker compose config` by hand before anyone dared `up -d`. A restore now
// does that itself, in a sidecar built from the Docker CLI image: whether the
// file is valid, what Compose warns about, and, by comparing configuration
// hashes with the running containers' labels, whether `up -d` would change
// anything.

// composeCLIRef is the Docker CLI image with Compose, pinned to its multi-arch
// index so every node runs the same bytes. Borrowed for one restore's checks
// and given back unless the node already had it (#36).
const composeCLIRef = "docker:29.8.2-cli@sha256:b1805116a6a86cc591b5d5f60a910a0715cdcc9d18d866ad68b1457ead25c35c"

const (
	composeCheckTimeout  = 3 * time.Minute
	composeCheckLifetime = "300"
	// composeScratchDir holds a compose file that is not on the host yet.
	composeScratchDir      = "/tmp/dockback-compose"
	composeScratchFile     = "docker-compose.yml"
	composeScratchEnv      = ".env"
	composeConfigHashLabel = "com.docker.compose.config-hash"
)

// ComposeCheck is what Compose said about one compose file.
type ComposeCheck struct {
	Version  string            // the Compose version that checked it
	Valid    bool              // `docker compose config` accepted it
	Problem  string            // why it did not, in Compose's words
	Warnings []string          // what Compose warned about while reading it
	Hashes   map[string]string // service → configuration hash, when valid
}

// ComposeChecker holds the borrowed Compose CLI image for one restore.
type ComposeChecker struct {
	c     *client.Client
	image DrillImage
}

// OpenComposeChecker makes the Compose CLI image available on the node.
func OpenComposeChecker(ctx context.Context, c *client.Client) (*ComposeChecker, error) {
	img, err := EnsureDrillImage(ctx, c, composeCLIRef)
	if err != nil {
		return nil, fmt.Errorf("obtaining the Compose CLI image: %w", err)
	}
	return &ComposeChecker{c: c, image: img}, nil
}

// Close gives the image back when these checks are what fetched it.
func (k *ComposeChecker) Close() { ReleaseDrillImage(context.Background(), k.c, k.image, "") }

// CheckFolder checks a compose file where it sits on the host. The folder is
// mounted read-only at its own path, so relative paths, env_file entries and
// the .env resolve exactly as they will for `docker compose` there.
func (k *ComposeChecker) CheckFolder(ctx context.Context, dir, file, project string) (*ComposeCheck, error) {
	if _, _, err := validateHostStackDir(dir); err != nil {
		return nil, err
	}
	mounts := []mount.Mount{{Type: mount.TypeBind, Source: dir, Target: dir, ReadOnly: true}}
	return k.run(ctx, mounts, nil, dir, file, project)
}

// CheckFiles checks a compose file and its .env that are not on the host yet,
// in a scratch folder inside the sidecar.
func (k *ComposeChecker) CheckFiles(ctx context.Context, compose, env []byte, project string) (*ComposeCheck, error) {
	files := map[string][]byte{composeScratchFile: compose, composeScratchEnv: env}
	return k.run(ctx, nil, files, composeScratchDir, composeScratchFile, project)
}

func (k *ComposeChecker) run(ctx context.Context, mounts []mount.Mount, files map[string][]byte, dir, file, project string) (*ComposeCheck, error) {
	ctx, cancel := context.WithTimeout(ctx, composeCheckTimeout)
	defer cancel()
	created, err := k.c.ContainerCreate(ctx,
		&container.Config{Image: composeCLIRef, Cmd: []string{"sleep", composeCheckLifetime}, Labels: sidecarLabels()},
		&container.HostConfig{Mounts: mounts}, nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("compose check sidecar: %w", err)
	}
	defer removeContainer(k.c, created.ID)
	if err := k.c.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("compose check sidecar start: %w", err)
	}
	for name, body := range files {
		if err := ExecStdin(ctx, k.c, created.ID, []string{"sh", "-c", "mkdir -p " + shQuote(dir) + " && cat > " + shQuote(dir+"/"+name)}, bytes.NewReader(body)); err != nil {
			return nil, fmt.Errorf("placing %s for the check: %w", name, err)
		}
	}
	check := &ComposeCheck{}
	if out, verr := ExecCapture(ctx, k.c, created.ID, []string{"docker", "compose", "version", "--short"}); verr == nil {
		check.Version = strings.TrimSpace(string(out))
	}
	args := []string{"-p", project, "--project-directory", dir, "-f", dir + "/" + file}
	// stderr folded into stdout: Compose prints its warnings there even on success.
	var said bytes.Buffer
	cerr := ExecStream(ctx, k.c, created.ID, append([]string{"sh", "-c", `docker compose "$@" config -q 2>&1`, "sh"}, args...), &said)
	messages := composeMessages(said.String())
	if cerr != nil {
		check.Problem = strings.Join(messages, "; ")
		if check.Problem == "" {
			check.Problem = cerr.Error()
		}
		return check, nil
	}
	check.Valid, check.Warnings = true, messages
	var hashes bytes.Buffer
	if err := ExecStream(ctx, k.c, created.ID, append(append([]string{"docker", "compose"}, args...), "config", "--hash", "*"), &hashes); err != nil {
		return nil, fmt.Errorf("reading the configuration hashes: %w", err)
	}
	check.Hashes = parseConfigHashes(hashes.String())
	return check, nil
}

// RunningConfigHashes reads the configuration hash Compose stamped on each of a
// project's containers, by service.
func RunningConfigHashes(ctx context.Context, c *client.Client, project string) (map[string]string, error) {
	list, err := c.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", labelProject+"="+project))})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, ctr := range list {
		if svc, hash := ctr.Labels[labelService], ctr.Labels[composeConfigHashLabel]; svc != "" && hash != "" {
			out[svc] = hash
		}
	}
	return out, nil
}

// parseConfigHashes reads `docker compose config --hash` output: one
// "service hash" pair per line. Pure.
func parseConfigHashes(out string) map[string]string {
	hashes := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) == 2 {
			hashes[fields[0]] = fields[1]
		}
	}
	return hashes
}

// composeLogMsg pulls the message out of Compose's logrus-style lines
// (time="…" level=warning msg="…").
var composeLogMsg = regexp.MustCompile(`msg="((?:[^"\\]|\\.)*)"`)

// composeMessages turns Compose's output into plain sentences, one per line it
// printed. Pure.
func composeMessages(out string) []string {
	var messages []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if m := composeLogMsg.FindStringSubmatch(line); m != nil {
			line = strings.ReplaceAll(m[1], `\"`, `"`)
		}
		messages = append(messages, line)
	}
	return messages
}
