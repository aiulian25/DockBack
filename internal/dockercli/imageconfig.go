package dockercli

import (
	"context"
	"sort"
	"strings"

	"github.com/docker/docker/client"
)

// Image-declared configuration (F95).
//
// Distinct from the CONTAINER's resolved values (already in inspect.json): this
// is what the IMAGE itself declares, so two versions of the same image can be
// compared to see what the newer one now expects.

// ImageConfig is an image's declared defaults, reduced to what actually predicts
// a broken start.
type ImageConfig struct {
	// EnvKeys are the KEYS the image declares, never their values. A default
	// value is not needed to answer "does the new image expect something the old
	// one didn't", and keeping values out means the manifest cannot carry a
	// secret an image happens to bake in.
	EnvKeys     []string `json:"env_keys,omitempty"`
	Entrypoint  []string `json:"entrypoint,omitempty"`
	Cmd         []string `json:"cmd,omitempty"`
	Volumes     []string `json:"volumes,omitempty"`
	Healthcheck bool     `json:"healthcheck,omitempty"`
	// Version is the image's own declared version label. With a floating tag the
	// reference is identical across upgrades and only the digest moves, so the
	// label is the only place a human-readable version can come from.
	Version string `json:"version,omitempty"`
	// User is the image's own USER instruction — "redis", "33", "" for root
	// (#24). Recorded so a numeric `user:` in a compose file can be told from a
	// container merely restating what the image already runs as: the first is a
	// host-specific id that means nothing on another machine, the second is the
	// image's own default and is the same number everywhere.
	User string `json:"user,omitempty"`
}

// InspectImageConfig reads an image's declared configuration.
func InspectImageConfig(ctx context.Context, c *client.Client, ref string) (ImageConfig, error) {
	insp, _, err := c.ImageInspectWithRaw(ctx, ref)
	if err != nil {
		return ImageConfig{}, err
	}
	var cfg ImageConfig
	if insp.Config == nil {
		return cfg, nil
	}
	cfg.EnvKeys = EnvKeys(insp.Config.Env)
	cfg.Entrypoint = append([]string(nil), insp.Config.Entrypoint...)
	cfg.Cmd = append([]string(nil), insp.Config.Cmd...)
	for v := range insp.Config.Volumes {
		cfg.Volumes = append(cfg.Volumes, v)
	}
	sort.Strings(cfg.Volumes)
	cfg.Healthcheck = insp.Config.Healthcheck != nil && len(insp.Config.Healthcheck.Test) > 0
	cfg.User = strings.TrimSpace(insp.Config.User)
	for _, k := range []string{"org.opencontainers.image.version", "version", "org.label-schema.version"} {
		if v := strings.TrimSpace(insp.Config.Labels[k]); v != "" {
			cfg.Version = v
			break
		}
	}
	return cfg, nil
}

// ImageEnv returns the environment an image bakes in, as full KEY=VALUE
// entries, so a compose reconstruction can tell the container's own
// configuration from what every container of this image carries.
//
// Values and all — deliberately, and deliberately NOT through InspectImageConfig
// beside it. That one keeps only key NAMES because its result is written into
// the manifest, where an image-baked secret would then live forever. This
// result is never persisted: it exists for the length of one comparison inside
// the generator and is discarded, so it can carry the values the comparison
// needs.
//
// Nil on any failure. The caller then keeps every variable, which is exactly
// the behaviour before this existed — a reconstruction is never worth failing a
// backup over.
func ImageEnv(ctx context.Context, c *client.Client, imageRef string) []string {
	if strings.TrimSpace(imageRef) == "" {
		return nil
	}
	insp, _, err := c.ImageInspectWithRaw(ctx, imageRef)
	if err != nil || insp.Config == nil {
		return nil
	}
	return append([]string(nil), insp.Config.Env...)
}

// EnvKeys extracts the KEY half of `KEY=VALUE` entries, sorted and de-duplicated.
//
// Values are deliberately discarded at the boundary rather than filtered later:
// container environments routinely hold passwords and API tokens, and the safest
// place to drop a secret is before it is ever copied.
func EnvKeys(env []string) []string {
	if len(env) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(env))
	for _, e := range env {
		k, _, ok := strings.Cut(e, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
