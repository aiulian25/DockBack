package dockercli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

// FindContainerByName returns the id of a container with the exact name (any
// state) and whether one exists. Docker reports names with a leading "/".
// Restore uses this because a backup manifest stores the container's *old* id,
// which is stale once the container has been recreated (new id, same name).
func FindContainerByName(ctx context.Context, c *client.Client, name string) (string, bool) {
	list, err := c.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return "", false
	}
	for _, ct := range list {
		for _, n := range ct.Names {
			if strings.TrimPrefix(n, "/") == name {
				return ct.ID, true
			}
		}
	}
	return "", false
}

// cloneNetwork is the throwaway bridge an isolated restore-as-copy clone attaches
// to, so it can't reach or shadow the live stack (F10).
const cloneNetwork = "dockback-restore"

// RemoveCloneContainer force-removes a throwaway clone container AND its anonymous
// volumes (F62 standby rehearsal teardown). The isolated clone's mounts are all
// fresh anonymous volumes (stripForClone), so RemoveVolumes cleans them up leaving
// zero residue; the shared `dockback-restore` bridge is intentionally left in place
// (other clones/restores may still be using it).
func RemoveCloneContainer(ctx context.Context, c *client.Client, id string) error {
	return c.ContainerRemove(ctx, id, container.RemoveOptions{Force: true, RemoveVolumes: true})
}

// CloneOptions, when non-nil, makes RecreateContainer build an ISOLATED clone
// instead of restoring in place (F10): a NEW name, and — when Isolate is set —
// fresh EMPTY volumes (never the original's data), no published host ports, and
// only the throwaway `dockback-restore` bridge network. This brings a backup up
// ALONGSIDE the live container (to inspect data or test an upgrade) without
// touching it.
type CloneOptions struct {
	NewName string // the clone's container name (required in clone mode)
	Isolate bool   // fresh volumes + no host ports + isolated network
	// Labels are set on the created clone, on top of whatever survived the strip
	// (F219). Used to stamp a test clone with its expiry so the reaper can find
	// it later — the container itself carries its own lifetime, so nothing is
	// lost if the control plane is restarted or restored from a backup.
	Labels map[string]string
}

// RecreateContainer rebuilds a container from its saved `docker inspect` JSON
// (full disaster recovery — PLAN §4.8/§9). It re-pulls the image, ensures any
// custom networks exist, recreates the container with the original config
// (ports, env, binds, named volumes, healthcheck, restart policy, user,
// command, etc.), reconnects extra networks, and returns the new container ID.
// Docker auto-creates missing bind-mount host directories on create/start; the
// caller restores the data afterward. When clone is non-nil it instead builds an
// isolated copy under clone.NewName (F10).
// nets carries the recorded network topology (F89). Empty for a legacy backup,
// in which case missing networks are created as plain bridges exactly as before.
// Returned warnings name anything that could not be honored, for the restore log.
func RecreateContainer(ctx context.Context, c *client.Client, inspectJSON []byte, imageDigest string, clone *CloneOptions, nets []NetworkSpec) (string, string, []string, error) {
	var warnings []string
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	var insp types.ContainerJSON
	if err := json.Unmarshal(inspectJSON, &insp); err != nil {
		return "", "", warnings, fmt.Errorf("parsing saved container config: %w", err)
	}
	if insp.Config == nil || insp.HostConfig == nil {
		return "", "", warnings, errors.New("backup manifest has no container config to recreate from")
	}
	name := strings.TrimPrefix(insp.Name, "/")
	isolate := false
	if clone != nil && clone.NewName != "" {
		// Clone mode: the target is the NEW name, so the force-remove below can only
		// ever remove a stale PRIOR CLONE of this name — never the original.
		name = clone.NewName
		isolate = clone.Isolate
	}

	// Remove any existing container with this name (e.g. a corrupted or stale
	// one from a prior deploy) so ContainerCreate doesn't fail with a name
	// conflict ("name ... is already in use").
	if existing, ok := FindContainerByName(ctx, c, name); ok {
		_ = c.ContainerRemove(ctx, existing, container.RemoveOptions{Force: true})
	}

	// Provision the image. Prefer one already present locally (e.g. an air-gapped
	// `docker load` of the saved image.tar the caller did first — PLAN §8.4),
	// else pull the recorded digest before the tag so a re-pushed tag can't drift
	// the restored image (PLAN §0.3). Create from the exact reference chosen.
	// F89: recorded network topology, keyed by name. Empty for a legacy backup —
	// specFor then returns a bare spec and behaviour is exactly as it was.
	specByName := map[string]NetworkSpec{}
	for _, n := range nets {
		specByName[n.Name] = n
	}
	specFor := func(name string) NetworkSpec {
		if s, ok := specByName[name]; ok {
			return s
		}
		return NetworkSpec{Name: name}
	}

	candidates := imageRefCandidates(imageDigest, insp.Config.Image)
	// #36: asked BEFORE anything pulls, because afterwards the answer is gone.
	// Only a clone can act on it — a real restore's image belongs to the
	// container it just recreated and is never anybody's to reclaim.
	hadImageAlready := isolate && anyImagePresent(ctx, c, candidates)
	chosen, err := ensureImageAvailable(ctx, c, candidates...)
	if err != nil {
		return "", "", warnings, fmt.Errorf("providing image for %q: %w", name, err)
	}
	insp.Config.Image = chosen

	// A hostname Docker derived from the OLD container's id is not a choice, and
	// replaying it pins the new container to a name matching a container that no
	// longer exists — one that nothing on the network resolves, and that the
	// application's own siblings cannot reach it by. Blanked, Docker derives a
	// fresh one from the new id, which is what would have happened originally.
	//
	// This runs before the network aliases are built, so the stale name is not
	// advertised as an alias either (see cleanAliases).
	if DerivedHostname(insp.Config.Hostname, insp.ID) {
		insp.Config.Hostname = ""
	}

	// Isolated clone: strip the spec down to fresh volumes + no ports, and attach
	// ONLY the throwaway bridge — so it can't touch the original's data, clash on a
	// host port, or shadow the live stack's DNS.
	if isolate {
		stripForClone(&insp)
		applyCloneLabels(&insp, clone.Labels)
		// #36: a clone that had to FETCH its image records which one, on itself.
		// The label rather than a database row for the same reason the expiry is a
		// label: the thing to clean up is on the host, and the record of what has
		// to survive the control plane being restarted or replaced.
		if !hadImageAlready {
			stampClonePulledImage(ctx, c, &insp, chosen)
		}
		ensureNetwork(ctx, c, cloneNetwork)
		insp.HostConfig.NetworkMode = container.NetworkMode(cloneNetwork)
		resp, cerr := c.ContainerCreate(ctx, insp.Config, insp.HostConfig, nil, nil, name)
		if cerr != nil {
			return "", "", warnings, fmt.Errorf("creating clone %q: %w", name, cerr)
		}
		return resp.ID, name, warnings, nil
	}

	// Ensure the primary network (NetworkMode) exists before create.
	primary := string(insp.HostConfig.NetworkMode)
	if isCustomNetwork(primary) {
		warnings = append(warnings, ensureNetworkSpec(ctx, c, specFor(primary))...)
	}

	// Attach the primary network WITH its original aliases at create time, so
	// inter-service DNS (e.g. paper-db, nextcloudredis) keeps resolving after a
	// stack restore.
	var netCfg *network.NetworkingConfig
	if insp.NetworkSettings != nil {
		if ep, ok := insp.NetworkSettings.Networks[primary]; ok && ep != nil {
			es := &network.EndpointSettings{Aliases: cleanAliases(ep.Aliases, insp.Config.Hostname)}
			// F89: restore the STATIC address the container was pinned to. Without
			// this a service referenced by IP comes back on a random one.
			if ipam := endpointIPAM(specFor(primary)); ipam != nil {
				es.IPAMConfig = ipam
			}
			netCfg = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{primary: es}}
		}
	}
	resp, err := c.ContainerCreate(ctx, insp.Config, insp.HostConfig, netCfg, nil, name)
	if err != nil && netCfg != nil {
		// Some daemons reject create-time endpoints for certain net modes; retry
		// letting HostConfig.NetworkMode drive, then connect with aliases below.
		netCfg = nil
		resp, err = c.ContainerCreate(ctx, insp.Config, insp.HostConfig, nil, nil, name)
	}
	// F178: the logging driver must not cost you the restore.
	//
	// A container is recreated with the driver it was captured with, and a target
	// host that cannot provide it — a plugin that is not installed there, a name
	// this daemon does not know — refuses to create the container at all. In a
	// stack restore that ends the whole run at whichever service hits it first.
	//
	// That trade is wrong in an obvious direction. A logging driver decides where
	// this container's stdout is written; it has nothing to do with its data, and
	// the daemon's default works everywhere. So: drop it, retry, and say plainly
	// what was dropped and what to do about it. A running container with default
	// logging beats a stack that would not come back.
	if err != nil && logDriverUnavailable(err) {
		captured := insp.HostConfig.LogConfig.Type
		// Two fallbacks, because there are two ways this fails and they need
		// different answers.
		//
		// If the container was captured WITH a driver this host lacks, clearing it
		// hands the choice to the daemon's default, which is the closest thing to
		// what the host itself wants.
		//
		// If that also fails — or if nothing was captured at all — then the
		// DAEMON'S OWN DEFAULT is the broken one, and deferring to it again cannot
		// work. json-file is named explicitly at that point: it is built into
		// every daemon, needs no plugin, and is what an operator expects to find
		// when `docker logs` still works.
		attempts := []struct {
			cfg  container.LogConfig
			note string
		}{}
		if captured != "" {
			attempts = append(attempts, struct {
				cfg  container.LogConfig
				note string
			}{container.LogConfig{}, fmt.Sprintf(
				"this container was captured using the %q logging driver, which this host cannot provide — it was recreated with the daemon's default logging instead. "+
					"Its data is unaffected and nothing else changed. Install that driver here and recreate the container if its logs need to keep going to the same place.", captured)})
		}
		attempts = append(attempts, struct {
			cfg  container.LogConfig
			note string
		}{container.LogConfig{Type: "json-file"}, fmt.Sprintf(
			"this host's DAEMON could not provide a logging driver for this container (captured driver: %s), so it was recreated with json-file logging explicitly. "+
				"That points at the daemon's own log-driver setting on this machine — check the \"log-driver\" entry in /etc/docker/daemon.json. "+
				"Its data is unaffected and nothing else changed.", quotedOrNone(captured))})

		for _, a := range attempts {
			insp.HostConfig.LogConfig = a.cfg
			resp, err = c.ContainerCreate(ctx, insp.Config, insp.HostConfig, netCfg, nil, name)
			if err != nil && netCfg != nil {
				netCfg = nil
				resp, err = c.ContainerCreate(ctx, insp.Config, insp.HostConfig, nil, nil, name)
			}
			if err == nil {
				warnings = append(warnings, a.note)
				break
			}
			if !logDriverUnavailable(err) {
				break // a different problem now; report that one, not this one
			}
		}
	}
	if err != nil {
		return "", "", warnings, fmt.Errorf("creating container %q: %w", name, err)
	}

	// Reconnect any additional networks the container was attached to, with
	// their aliases preserved.
	if insp.NetworkSettings != nil {
		for nname, ep := range insp.NetworkSettings.Networks {
			if nname == primary && netCfg != nil {
				continue // already attached with aliases at create
			}
			spec := specFor(nname)
			if isCustomNetwork(nname) {
				warnings = append(warnings, ensureNetworkSpec(ctx, c, spec)...)
			}
			var aliases []string
			if ep != nil {
				aliases = cleanAliases(ep.Aliases, insp.Config.Hostname)
			}
			es := &network.EndpointSettings{Aliases: aliases}
			if ipam := endpointIPAM(spec); ipam != nil {
				es.IPAMConfig = ipam
			}
			if cerr := c.NetworkConnect(ctx, nname, resp.ID, es); cerr != nil && es.IPAMConfig != nil {
				// The recorded address may be taken, or outside the subnet this host
				// gave the network. Reattaching WITHOUT it beats leaving the container
				// off its own stack network — but say so, because a service addressed
				// by IP will not work until it is corrected.
				es.IPAMConfig = nil
				if rerr := c.NetworkConnect(ctx, nname, resp.ID, es); rerr == nil {
					warnings = append(warnings, fmt.Sprintf(
						"could not reattach %s at %s (%v) — connected with a dynamic address instead",
						nname, spec.IPv4, cerr))
				} else {
					warnings = append(warnings, fmt.Sprintf("could not reattach network %s (%v)", nname, rerr))
				}
			}
		}
	}
	return resp.ID, name, warnings, nil
}

// quotedOrNone renders a captured logging driver for a message, distinguishing
// "the container asked for this and it is missing" from "the container asked for
// nothing and the daemon's own default is what broke".
func quotedOrNone(driver string) string {
	if strings.TrimSpace(driver) == "" {
		return "none recorded, so the daemon's default was used"
	}
	return strconv.Quote(driver)
}

// logDriverUnavailable reports whether a create failed because the target daemon
// cannot provide the container's logging driver (F178).
//
// Matched on the daemon's message, because the API returns a plain 400 with no
// machine-readable code for this. Deliberately narrow: it gates a RETRY that
// silently changes where logs go, so anything it does not clearly recognise is
// left to fail with the original error rather than quietly reconfigured.
func logDriverUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "logging plugin"): // "error looking up logging plugin X: plugin X not found"
		return true
	case strings.Contains(msg, "no log driver named"): // "logger: no log driver named 'X' is registered"
		return true
	case strings.Contains(msg, "error creating logger"):
		return true
	case strings.Contains(msg, "log driver") && strings.Contains(msg, "not "):
		return true // "log driver X is not supported / not found / not registered"
	}
	return false
}

// stripForClone rewrites an inspected container spec so recreating it produces an
// ISOLATED clone that can never touch the original's data or ports (F10): every
// mount destination becomes a FRESH anonymous volume (the original named volumes
// and host binds are dropped, so a restore fills the CLONE's own volumes, not the
// live container's), all published ports are removed (no host-port clash), the
// restart policy is disabled (a broken clone shouldn't loop), and stack links are
// dropped. The caller switches it onto the isolated bridge network.
func stripForClone(insp *types.ContainerJSON) {
	hc := insp.HostConfig
	// Fresh empty volume at every original mount destination; drop all real
	// volume/bind sources so nothing points at the live container's data.
	if insp.Config.Volumes == nil {
		insp.Config.Volumes = map[string]struct{}{}
	}
	for _, m := range insp.Mounts {
		if m.Destination != "" {
			insp.Config.Volumes[m.Destination] = struct{}{}
		}
	}
	hc.Binds = nil
	hc.Mounts = nil
	hc.VolumesFrom = nil
	// No host ports (avoid clashing with the running original).
	hc.PortBindings = nil
	hc.PublishAllPorts = false
	// Don't restart-loop a clone that can't reach its (absent) dependencies.
	hc.RestartPolicy = container.RestartPolicy{Name: "no"}
	// Legacy links reference the live stack by name — drop them.
	hc.Links = nil
	// F219: and drop the labels that make this container SOMEONE ELSE.
	//
	// A clone inherited the original's compose identity, so Docker, and DockBack
	// reading Docker, saw a second member of the live stack: it appeared in the
	// stack's service list, counted as a member with no backup of its own, and
	// was a candidate for the next app-consistent stack backup. It inherited the
	// dockback.* policy labels too, which is how a throwaway copy acquires a
	// backup schedule.
	//
	// Neither is what "an isolated copy" means. This is the same job the rest of
	// this function does — no shared data, no shared ports, no shared network —
	// applied to identity, which is the one that leaks into the control plane
	// rather than into the host.
	for k := range insp.Config.Labels {
		if strings.HasPrefix(k, "com.docker.compose.") || strings.HasPrefix(k, "dockback.") {
			delete(insp.Config.Labels, k)
		}
	}
}

// applyCloneLabels stamps caller-supplied labels onto the clone. Applied AFTER
// stripForClone, so a marker DockBack sets for itself can never be swept away by
// the strip above.
func applyCloneLabels(insp *types.ContainerJSON, labels map[string]string) {
	if len(labels) == 0 {
		return
	}
	if insp.Config.Labels == nil {
		insp.Config.Labels = map[string]string{}
	}
	for k, v := range labels {
		insp.Config.Labels[k] = v
	}
}

// ClonePulledImageLabel names the image a test clone had to pull, so the reaper
// can give it back when the clone goes (#36).
//
// The com.dockback.* namespace, like the expiry label beside it: bookkeeping
// DockBack writes for itself, not the user-facing policy namespace.
const ClonePulledImageLabel = "com.dockback.clone_pulled_image"

// ClonePulledImage reads the image id a clone recorded, empty when it pulled
// nothing — which is the ordinary case, since a clone usually runs beside the
// original whose image is already here.
func ClonePulledImage(labels map[string]string) string {
	return strings.TrimSpace(labels[ClonePulledImageLabel])
}

// anyImagePresent reports whether ANY of the candidate references already
// resolves locally.
//
// Any, not all: ensureImageAvailable takes the first one that is present, so a
// single hit means nothing was fetched.
func anyImagePresent(ctx context.Context, c *client.Client, refs []string) bool {
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		if _, _, err := c.ImageInspectWithRaw(ctx, ref); err == nil {
			return true
		}
	}
	return false
}

// stampClonePulledImage records the resolved image ID on the clone.
//
// By ID, because that is the only thing safe to remove later — untagging leaves
// the layers, which is #36 itself. An image whose id cannot be read is simply
// not recorded: without an id there is nothing to give back safely.
func stampClonePulledImage(ctx context.Context, c *client.Client, insp *types.ContainerJSON, ref string) {
	img, _, err := c.ImageInspectWithRaw(ctx, ref)
	if err != nil || img.ID == "" {
		return
	}
	applyCloneLabels(insp, map[string]string{ClonePulledImageLabel: img.ID})
}

// imageRefCandidates lists the image references to try, in priority order, so
// restore reproduces the IDENTICAL image (PLAN §0.3): the digest-pinned ref
// (repo@sha256:…) first when recorded, then the original tag for older backups
// or for an air-gapped image loaded under its tag.
func imageRefCandidates(digest, tag string) []string {
	var out []string
	if strings.Contains(digest, "@sha256:") {
		out = append(out, digest)
	}
	if tag != "" {
		out = append(out, tag)
	}
	return out
}

// ShortIDAlias reports whether an alias looks like a Docker container short id —
// exactly 12 lowercase hex characters.
//
// Docker auto-adds a container's short id as an alias, and a recreated container
// gets a NEW id, so a RECORDED one is always somebody else's. Matched by SHAPE
// rather than against one known id, because stale ids accumulate: the recorded
// inspect of a restored container carries the id it was restored FROM, the next
// restore preserves that and adds its own, and there is no list anywhere of the
// ones that came before.
//
// Known cost, accepted: an alias somebody chose that is coincidentally exactly
// 12 lowercase hex characters ("deadbeefcafe") is dropped too. Docker resolves
// such a name ambiguously against real short ids anyway, and no report in the
// evidence base has ever shown one.
func ShortIDAlias(a string) bool {
	if len(a) != 12 {
		return false
	}
	for _, r := range a {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// DerivedHostname reports whether a hostname is Docker's OWN default rather
// than one somebody chose.
//
// With no `hostname:` given, Docker uses the container id's first 12 characters.
// That value describes the container it was generated for and nothing else, so
// writing it back — into a reconstructed compose file, or into a recreated
// container's config — pins a name to an id the new container will not have.
//
// A deliberately-chosen hostname that happens to equal the short id is
// indistinguishable from the default and is treated as derived. That costs
// nothing: Docker gives the new container its own short id as its hostname
// either way.
//
// One definition, used by BOTH paths — the compose reconstruction and the
// recreate — because the two disagreeing about what counts as a chosen name is
// how a restored stack ends up resolvable by one route and not the other.
func DerivedHostname(hostname, containerID string) bool {
	return hostname == "" || (ShortIDAlias(hostname) && strings.HasPrefix(containerID, hostname))
}

// cleanAliases ensures the container hostname is resolvable and drops the
// auto-added short-id aliases, which name containers that no longer exist.
//
// The hostname is prepended and never filtered, deliberately. Docker's embedded
// DNS registers a container's hostname on every user-defined network — that is
// the whole mechanism the wikijs restore depended on — and RecreateContainer
// recreates with the recorded Config.Hostname, so the restored container really
// does answer to it. A short-id-shaped hostname needs no filtering here: the
// recreate has already blanked it (DerivedHostname), so this is only ever
// called with a name somebody chose.
func cleanAliases(aliases []string, hostname string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(a string) {
		if a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	if hostname != "" {
		add(hostname)
	}
	for _, a := range aliases {
		if ShortIDAlias(a) {
			continue
		}
		add(a)
	}
	return out
}

func isCustomNetwork(n string) bool {
	switch n {
	case "", "default", "bridge", "host", "none":
		return false
	}
	return !strings.HasPrefix(n, "container:")
}

// endpointIPAM builds the endpoint address config for a recorded static
// assignment, or nil when the container had none (a dynamically-addressed
// container must stay dynamic — pinning a lease it never asked for would make it
// fail to restore the moment that address is in use).
func endpointIPAM(spec NetworkSpec) *network.EndpointIPAMConfig {
	if spec.IPv4 == "" && spec.IPv6 == "" {
		return nil
	}
	return &network.EndpointIPAMConfig{IPv4Address: spec.IPv4, IPv6Address: spec.IPv6}
}

// ensureNetwork creates a bridge network with the given name if it doesn't
// already exist (best-effort).
func ensureNetwork(ctx context.Context, c *client.Client, name string) {
	if _, err := c.NetworkInspect(ctx, name, network.InspectOptions{}); err == nil {
		return
	}
	_, _ = c.NetworkCreate(ctx, name, network.CreateOptions{Driver: "bridge"})
}
