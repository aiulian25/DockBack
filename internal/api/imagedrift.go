package api

import (
	"context"
	"strings"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
	"github.com/docker/docker/client"
)

// Image-requirement drift for the restore dialog (F95).
//
// A recreate replays the backup's configuration into whatever image actually
// runs. When the recorded digest is still available that is the identical image
// and nothing can drift — so this reports only the case that genuinely bites:
// the digest has gone and the TAG now resolves to a newer build.

// imageDriftFor reports what the image a restore would really use declares that
// this backup's configuration does not provide.
//
// Best-effort throughout: an image that cannot be inspected yields nothing
// rather than a false warning, because a restore dialog that cries wolf is one
// the operator learns to skip.
func (s *Server) imageDriftFor(ctx context.Context, cli *client.Client, m *backup.Manifest) []string {
	if m == nil || m.Image == "" {
		return nil
	}
	// Which reference would the restore actually choose? The digest wins when it
	// is still obtainable, and that is the same image — no drift possible.
	if m.ImageDigest != "" && strings.Contains(m.ImageDigest, "@sha256:") {
		if _, err := dockercli.InspectImageConfig(ctx, cli, m.ImageDigest); err == nil {
			return nil
		}
	}

	next, err := dockercli.InspectImageConfig(ctx, cli, m.Image)
	if err != nil {
		return nil // cannot see the new image; say nothing rather than guess
	}

	oldVersion := ""
	if m.ImageConfig != nil {
		oldVersion = m.ImageConfig.Version
	}
	// The change note comes FIRST: it is the honest headline when the declared
	// config shows nothing, which is exactly what happened in the incident that
	// motivated this — a hard requirement added with no default to declare.
	out := []string{backup.ImageChangeNote(oldVersion, next.Version)}
	// F158: a version difference is only alarming if the on-disk format cares.
	// For an application whose format is stable across the whole major version,
	// saying so here is what stops a routine tag move from reading as a risk —
	// and names the edge that IS real.
	if compat := backup.StorageCompatFor(m.Image); compat != "" {
		out = append(out, compat)
	}
	if m.ImageConfig == nil {
		// A pre-F95 backup recorded nothing to compare against. The version note
		// above still stands; claiming more would be inventing it.
		return out
	}
	return append(out, backup.ImageConfigDrift(*m.ImageConfig, backup.ImageConfig{
		EnvKeys: next.EnvKeys, Entrypoint: next.Entrypoint, Cmd: next.Cmd,
		Volumes: next.Volumes, Healthcheck: next.Healthcheck, Version: next.Version,
	}, m.ContainerEnvKeys)...)
}
