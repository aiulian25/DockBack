package dockercli

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// TLS certificate inventory (F143).
//
// A handful of applications carry certificates inside their own data — a
// reverse proxy most obviously, where the certificate files and the routing
// table that references them are the deployment. For those, "did the files come
// back?" is not the same question as "did the volume come back", and the answer
// nobody wants to discover from a browser is that a certificate is missing,
// unreadable, or expired.
//
// Only the PUBLIC half is ever read. A certificate is a public document — it is
// handed to every client that connects — so copying it out to parse its validity
// dates adds no exposure. The private key beside it is the opposite, and it is
// never read: its presence is established by testing that the file exists, and
// its bytes never leave the container.

// maxCertFiles caps how many certificate files one scan returns, so a directory
// full of them cannot spawn unbounded work.
const maxCertFiles = 60

// maxCertFileSize caps the size of a file the scan will copy, as a `find -size`
// literal. A real certificate chain is a few kilobytes; anything approaching a
// megabyte is not one.
const maxCertFileSize = "1024k"

// certFileName holds the sidecar's output to a numbered .pem, so a (trusted)
// sidecar can only ever produce the shape we expect — defence in depth, the
// same discipline the SQLite snapshot extraction uses.
var certFileName = regexp.MustCompile(`^[0-9]+\.pem$`)

// CertFile is one certificate file read out of a container's volumes (F143).
type CertFile struct {
	// Path is the file's absolute path inside the container.
	Path string
	// PEM is the certificate's own bytes — the public half, nothing else.
	PEM []byte
	// HasKey reports that a private key file sits beside it. Established by
	// testing for the file; its contents are never read.
	HasKey bool
}

// ReadCertificates copies the certificate files under the given container
// directories out of a target's volumes, via a READ-ONLY sidecar (F143).
//
// Best-effort and bounded: an unreachable node, a sidecar image without the
// tools, or a root that does not exist yields fewer results or none, never an
// error that should fail a backup or a restore. The caller distinguishes "found
// nothing" from "could not look", because those call for opposite reactions.
func ReadCertificates(ctx context.Context, c *client.Client, targetID string, roots []string) ([]CertFile, error) {
	if len(roots) == 0 {
		return nil, nil
	}
	if err := ensureSidecar(ctx, c); err != nil {
		return nil, err
	}

	// Collect candidates first, into a file, so the numbering loop below is NOT a
	// pipeline: busybox ash runs the right-hand side of a pipe in a subshell, and
	// a counter incremented there would be lost.
	var script strings.Builder
	script.WriteString("mkdir -p /out; { for d in")
	for _, r := range roots {
		script.WriteString(" '")
		script.WriteString(shellEscape(r))
		script.WriteString("'")
	}
	script.WriteString(`; do [ -d "$d" ] || continue; ` +
		`find "$d" -type f -size -` + maxCertFileSize +
		` \( -name 'fullchain.pem' -o -name 'cert.pem' -o -name 'certificate.pem' -o -name '*.crt' \) 2>/dev/null; ` +
		`done; } | head -n ` + fmt.Sprint(maxCertFiles) + ` > /tmp/certlist 2>/dev/null; ` +
		`i=0; while IFS= read -r f; do i=$((i+1)); ` +
		`cp "$f" "/out/$i.pem" 2>/dev/null || continue; ` +
		// The private key's presence, and only its presence. Nothing reads it.
		`dd=$(dirname "$f"); k=0; ` +
		`[ -f "$dd/privkey.pem" ] && k=1; ` +
		`for kf in "$dd"/*.key; do [ -f "$kf" ] && k=1; done; ` +
		`printf '%s\t%s\t%s\n' "$i" "$f" "$k" >> /out/index.txt; ` +
		`done < /tmp/certlist; tar -cf - -C /out .`)

	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: []string{"sh", "-c", script.String()}, AttachStdout: true, AttachStderr: true, Labels: sidecarLabels()},
		&container.HostConfig{VolumesFrom: []string{targetID + ":ro"}}, nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("certificate scan sidecar create: %w", err)
	}
	sidecarID := created.ID
	defer removeContainer(c, sidecarID)

	att, err := c.ContainerAttach(ctx, sidecarID, container.AttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		return nil, fmt.Errorf("certificate scan attach: %w", err)
	}
	if err := c.ContainerStart(ctx, sidecarID, container.StartOptions{}); err != nil {
		att.Close()
		return nil, fmt.Errorf("certificate scan start: %w", err)
	}
	var stdout, stderr bytes.Buffer
	copyDone := make(chan error, 1)
	go func() { _, e := stdcopy.StdCopy(&stdout, &stderr, att.Reader); copyDone <- e }()
	select {
	case <-copyDone:
	case <-ctx.Done():
		att.Close()
		return nil, ctx.Err()
	}
	att.Close()

	return parseCertTar(stdout.Bytes())
}

// parseCertTar turns the sidecar's tar (index.txt + numbered .pem files) into
// the file list. Split out so the parsing is testable without Docker.
func parseCertTar(raw []byte) ([]CertFile, error) {
	type entry struct {
		path   string
		hasKey bool
	}
	index := map[string]entry{}
	pems := map[string][]byte{}

	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("certificate scan output: %w", err)
		}
		if h.FileInfo().IsDir() {
			continue
		}
		base := filepath.Base(h.Name)
		switch {
		case base == "index.txt":
			b, _ := io.ReadAll(io.LimitReader(tr, 1<<20))
			for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				parts := strings.Split(line, "\t")
				if len(parts) < 3 {
					continue
				}
				index[strings.TrimSpace(parts[0])] = entry{path: parts[1], hasKey: strings.TrimSpace(parts[2]) == "1"}
			}
		case certFileName.MatchString(base):
			b, _ := io.ReadAll(io.LimitReader(tr, 1<<20))
			pems[strings.TrimSuffix(base, ".pem")] = b
		}
	}

	var out []CertFile
	for n, e := range index {
		pem, ok := pems[n]
		if !ok || len(pem) == 0 || e.path == "" {
			continue
		}
		out = append(out, CertFile{Path: e.path, PEM: pem, HasKey: e.hasKey})
	}
	return out, nil
}

// certScanTimeout bounds a certificate scan. It reads a handful of small files,
// so anything beyond this is a stuck sidecar rather than slow work.
const certScanTimeout = 2 * time.Minute

// ReadCertificatesBounded is ReadCertificates with its own timeout, so a caller
// on a long-lived context cannot hang on it.
func ReadCertificatesBounded(ctx context.Context, c *client.Client, targetID string, roots []string) ([]CertFile, error) {
	cctx, cancel := context.WithTimeout(ctx, certScanTimeout)
	defer cancel()
	return ReadCertificates(cctx, c, targetID, roots)
}
