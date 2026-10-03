package backup

import (
	"bytes"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	units "github.com/docker/go-units"

	"dockback/internal/dockercli"
	yaml "go.yaml.in/yaml/v3"
)

// composeProjectLayout extracts the container's ON-HOST compose project location
// from its labels: the working directory Compose ran in and the basename of its
// first config file. Both are empty for a container started without compose (a
// plain `docker run`), in which case restore falls back to a user-supplied base
// directory. The config_files label may be a comma-separated list and/or an
// absolute path — we take the first entry's basename.
func composeProjectLayout(labels map[string]string) (workingDir, composeFile string) {
	if labels == nil {
		return "", ""
	}
	workingDir = strings.TrimSpace(labels["com.docker.compose.project.working_dir"])
	if cf := strings.TrimSpace(labels["com.docker.compose.project.config_files"]); cf != "" {
		first := strings.TrimSpace(strings.Split(cf, ",")[0])
		if first != "" {
			composeFile = path.Base(first)
		}
	}
	return workingDir, composeFile
}

// stackEnvSourcePath is where `docker compose` reads the interpolating .env
// from: the recorded project working directory when the labels carry one,
// else beside the first config file (compose's own default). Pure.
//
// The two coincide for an ordinary `docker compose up` in a stack folder, and
// diverge the moment --project-directory is used or a tool runs compose from
// its own layout — which is when deriving from the config file's directory
// captures the wrong file, or none.
func stackEnvSourcePath(configFiles, workingDir string) string {
	if wd := strings.TrimSpace(workingDir); wd != "" {
		return path.Join(wd, ".env")
	}
	first := strings.TrimSpace(strings.Split(configFiles, ",")[0])
	if first == "" {
		return ""
	}
	return path.Join(path.Dir(first), ".env")
}

// composeHeader explains, in the file itself, that this is a reconstruction.
const composeHeader = `# docker-compose.yml — reconstructed by DockBack from the container's runtime
# configuration (docker inspect) at backup time. The original compose file lives
# on the host and is not reachable through the socket-proxy, so this is a
# best-effort, FUNCTIONAL equivalent meant for restore-by-hand (see manifest.json).
# Review before use: image tags, secrets in 'environment', bind-mount paths and
# 'external' networks may need adjusting for the target host.
`

// composeFromInspect synthesizes a single-service Compose file from a container
// inspect, satisfying PLAN §0.3 ("Always: Compose files") and §9.3 (open,
// self-describing, hand-restorable format) without needing host filesystem
// access. imageRef is the resolved image reference recorded in the manifest;
// it falls back to the inspected config image when empty.
// nets carries the recorded topology (F89) so the reconstructed file declares
// real network definitions instead of the blanket `external: true` it used to.
// Nil for a legacy backup, which reproduces the previous output exactly.
//
// siblings names the OTHER services that will share the finished document (#16).
// It gates `depends_on`, which is only meaningful when the services it points at
// are in the same file: compose refuses to start a project whose dependency is
// undefined, so a one-service reconstruction that declared one would stop being
// runnable by hand. Nil for the single-service documents, populated by the stack
// merge.
func composeFromInspect(insp types.ContainerJSON, imageRef string, netDefs []NetworkRef, siblings map[string]bool, imageEnv []string) ([]byte, error) {
	if insp.Config == nil {
		return nil, fmt.Errorf("inspect has no config")
	}
	cfg := insp.Config
	name := strings.TrimPrefix(insp.Name, "/")

	image := imageRef
	if image == "" {
		image = cfg.Image
	}
	service := map[string]any{"image": image}

	if name != "" {
		service["container_name"] = name
	}

	// The name the rest of the stack reaches this container by (#N1).
	//
	// Docker registers a container's hostname in its embedded DNS on every
	// user-defined network, so `hostname: wiki-js-db` makes that a resolvable
	// name for every sibling — which is exactly how an application's DB_HOST
	// finds its database. A reconstruction that drops it produces a file that
	// starts cleanly and then fails with ENOTFOUND the moment one service looks
	// up another: the app is up, the database is up, and nothing connects them.
	//
	// The restore path never had this problem — it replays Config.Hostname
	// verbatim, and folds it into the network aliases besides (cleanAliases) —
	// which is why only the hand-restore artifact lost the name.
	netMode := ""
	if insp.HostConfig != nil {
		netMode = string(insp.HostConfig.NetworkMode)
	}
	if !defaultHostname(cfg.Hostname, insp.ID) && !borrowedNetworkNamespace(netMode) {
		service["hostname"] = cfg.Hostname
		if cfg.Domainname != "" {
			service["domainname"] = cfg.Domainname
		}
	}

	// The retry COUNT is part of the policy, not decoration (#N2).
	//
	// `on-failure:5` restarts a crashing container five times and then leaves it
	// down; bare `on-failure` retries forever. Emitting the name alone turns a
	// deliberate give-up point into an unbounded loop — the same class of silent
	// change as losing the hostname, and invisible until something crashes.
	//
	// Only `on-failure` can carry a count: Docker rejects a non-zero
	// MaximumRetryCount on any other policy at create time, so there is no other
	// case to handle here.
	if insp.HostConfig != nil {
		if r := string(insp.HostConfig.RestartPolicy.Name); r != "" && r != "no" {
			if r == "on-failure" && insp.HostConfig.RestartPolicy.MaximumRetryCount > 0 {
				r = fmt.Sprintf("on-failure:%d", insp.HostConfig.RestartPolicy.MaximumRetryCount)
			}
			service["restart"] = r
		}
	}

	// Only what THIS container configures (#N7).
	//
	// `docker inspect` merges the image's own environment into Config.Env, so
	// emitting all of it copied PATH, HOME, PS1, NODE_ENV, LSIO_FIRST_PARTY and a
	// dozen S6_* variables into every file — the image's internals, written down
	// as if the operator had chosen them.
	//
	// Beyond the noise it PINS them: a compose file that sets PATH overrides
	// whatever the next image version bakes in, so an upgrade quietly runs with
	// the old image's PATH. No hand-written compose file behaves that way, and
	// removing this is the removal of an accidental pin rather than a new risk.
	if env := containerOwnEnv(cfg.Env, imageEnv); len(env) > 0 {
		service["environment"] = env
	}
	if ports := portList(insp); len(ports) > 0 {
		service["ports"] = ports
	}
	if vols := volumeList(insp); len(vols) > 0 {
		service["volumes"] = vols
	}
	if len(cfg.Cmd) > 0 {
		service["command"] = []string(cfg.Cmd)
	}
	if len(cfg.Entrypoint) > 0 {
		service["entrypoint"] = []string(cfg.Entrypoint)
	}
	if labels := userLabels(cfg.Labels); len(labels) > 0 {
		service["labels"] = labels
	}
	if cfg.User != "" {
		service["user"] = cfg.User
	}
	// #16: the probe the container actually runs. Emitted whether the source
	// wrote it or a restore added it, because a compose file that omits the
	// healthcheck is a compose file that recreates the race on the next `up`.
	if probe := composeHealthcheck(cfg.Healthcheck); probe != nil {
		service["healthcheck"] = probe
	}
	if deps := composeDependsOn(cfg.Labels, siblings); len(deps) > 0 {
		service["depends_on"] = deps
	}
	if cfg.WorkingDir != "" && cfg.WorkingDir != "/" {
		service["working_dir"] = cfg.WorkingDir
	}
	if insp.HostConfig != nil {
		if len(insp.HostConfig.CapAdd) > 0 {
			service["cap_add"] = []string(insp.HostConfig.CapAdd)
		}
		if len(insp.HostConfig.CapDrop) > 0 {
			service["cap_drop"] = []string(insp.HostConfig.CapDrop)
		}
		if insp.HostConfig.Privileged {
			service["privileged"] = true
		}
	}
	composeHostConfig(service, insp)

	// Hoisted above the networks emission: the alias filter needs to know this
	// service's own name, because compose re-registers it and repeating it as an
	// alias is noise.
	svcName := ""
	if cfg.Labels != nil {
		svcName = cfg.Labels["com.docker.compose.service"]
	}
	if svcName == "" {
		svcName = name
	}
	if svcName == "" {
		svcName = "app"
	}

	// How this container is attached (#N5).
	//
	// `network_mode` and `networks` are mutually exclusive — compose refuses a
	// service that declares both — so host and none networking take the whole
	// branch. Without this a host-networked container emitted no networking at
	// all and `compose up` silently put it on the default bridge, which is the
	// same class of quiet loss as dropping the hostname.
	//
	// `container:<id>` mode is deliberately NOT emitted. The recorded value
	// embeds the OLD container's full id, which is wrong the moment anything is
	// recreated, and the service name it should reference cannot be derived
	// offline. Those services keep today's behaviour — nothing emitted — and the
	// API restore reproduces the mode from HostConfig verbatim.
	nets := networkList(insp)
	switch {
	case netMode == "host" || netMode == "none":
		service["network_mode"] = netMode
	case len(nets) > 0:
		service["networks"] = composeServiceNetworks(nets, insp, netDefs, svcName, name, cfg.Hostname)
	}

	doc := map[string]any{
		"services": map[string]any{svcName: service},
	}
	// Declare each attached network. With a recorded definition (F89) the real
	// subnet and flags are emitted, so `docker compose up` on a fresh host
	// reproduces the topology rather than inventing a default bridge. Without one
	// (a legacy backup) they stay `external: true` — referenced, not recreated —
	// which is the only safe assumption when nothing about them is known.
	if len(nets) > 0 && netMode != "host" && netMode != "none" {
		byName := map[string]NetworkRef{}
		for _, n := range netDefs {
			byName[n.Name] = n
		}
		decl := map[string]any{}
		for _, n := range nets {
			def, ok := byName[n]
			if !ok || (def.Subnet == "" && !def.Internal && def.Driver == "") {
				decl[n] = map[string]any{"external": true}
				continue
			}
			d := map[string]any{}
			if def.Driver != "" {
				d["driver"] = def.Driver
			}
			if def.Internal {
				d["internal"] = true
			}
			if def.Attachable {
				d["attachable"] = true
			}
			if pools := composePools(def); len(pools) > 0 {
				d["ipam"] = map[string]any{"config": pools}
			}
			decl[n] = d
		}
		doc["networks"] = decl
	}

	body, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return append([]byte(composeHeader), body...), nil
}

// defaultHostname reports whether this hostname is Docker's OWN default rather
// than one somebody chose. Emitting that would add a line to every service in
// every reconstruction and pin a name that means nothing.
//
// The rule itself lives in dockercli.DerivedHostname, because the RECREATE path
// applies it too and the two must agree: a name one path treats as chosen and
// the other as derived is a stack that comes back resolvable by one route and
// not the other.
func defaultHostname(hostname, containerID string) bool {
	return dockercli.DerivedHostname(hostname, containerID)
}

// borrowedNetworkNamespace reports whether this container's hostname belongs to
// something other than the service itself.
//
// `network_mode: host` puts it in the machine's namespace, where the hostname is
// the HOST's and writing it into a compose file would rename the machine's
// service on any other host. `container:<id>` borrows another container's
// namespace, so the name is that container's business. Docker refuses
// --hostname with either mode, so a file carrying both would not start.
func borrowedNetworkNamespace(netMode string) bool {
	return netMode == "host" || strings.HasPrefix(netMode, "container:")
}

// containerOwnEnv keeps the environment entries this container configures and
// drops the ones it merely inherited from its image.
//
// The comparison is on the WHOLE entry, key and value together. A key the image
// declares whose value differs is a deliberate override and is kept — that is
// how `S6_VERBOSITY=2` survives against an image default of 1. A key set to
// exactly the image's own value is dropped: it is reproducible from the image by
// definition, which is the same judgement a person writing the file by hand
// would make.
//
// A nil imageEnv keeps everything. That is the pre-existing behaviour, and it is
// what a caller that could not read the image gets — a reconstruction listing
// too much is a great deal better than one that dropped a variable the
// application needed because a lookup failed.
func containerOwnEnv(env, imageEnv []string) map[string]string {
	if len(imageEnv) == 0 {
		return envMap(env)
	}
	baked := make(map[string]bool, len(imageEnv))
	for _, entry := range imageEnv {
		baked[entry] = true
	}
	out := make(map[string]string, len(env))
	for _, entry := range env {
		if baked[entry] {
			continue
		}
		if k, v, ok := strings.Cut(entry, "="); ok {
			out[k] = v
		}
	}
	return out
}

// composeHostConfig emits the resource limits and host options a hand-restore
// needs (#N6).
//
// Before this the reconstruction carried three HostConfig fields — cap_add,
// cap_drop and privileged — and dropped everything else. The measured diff
// against a real original showed `mem_limit: 512m`, `cpu_shares: 768` and
// `security_opt: [no-new-privileges:true]` all gone.
//
// The last one is why this is not cosmetics. A file that silently loses
// `no-new-privileges`, `read_only` or a `cap_drop` set describes a container
// hardened less than the one it claims to reproduce, and somebody recreating
// from it gets the weaker container with no indication anything changed. The
// API restore was never affected — it replays HostConfig verbatim — so this is
// exactly the gap between "restored by DockBack" and "rebuilt by hand from what
// DockBack wrote down".
//
// Every DEFAULT is skipped, so a file says only what its container actually
// asked for. Docker stamps values into every inspect whether or not anyone chose
// them — 64 MiB of shm, `private` IPC, the `runc` runtime — and emitting those
// would add a dozen lines to every service and pin choices nobody made.
//
// LogConfig is deliberately absent. The daemon writes its own default driver
// into every inspect, and nothing distinguishes "the operator chose json-file"
// from "this daemon's default is json-file", so emitting it would pin a
// non-choice on every service. The restore path already handles a missing
// driver with fallbacks.
func composeHostConfig(service map[string]any, insp types.ContainerJSON) {
	hc := insp.HostConfig
	if hc == nil {
		return
	}
	set := func(key string, v any) { service[key] = v }

	// Memory. Rendered the way people write it — the original said 512m.
	if hc.Memory > 0 {
		set("mem_limit", composeBytes(hc.Memory))
	}
	if hc.MemoryReservation > 0 {
		set("mem_reservation", composeBytes(hc.MemoryReservation))
	}
	// Swap needs two rules the plain "> 0" test gets wrong in both directions.
	//
	// -1 is "unlimited", a deliberate choice a positive-only test would drop,
	// leaving the file describing a limit the container does not have.
	//
	// And 2x the memory limit is Docker's DERIVED DEFAULT for a container given
	// only --memory, measured: --memory 512m yields MemorySwap 1g with nobody
	// asking. Emitting it pins a value computed from mem_limit, so editing the
	// memory limit later silently leaves the swap limit behind. Skipping it is
	// safe in both readings: if it was the default it should not be there, and if
	// somebody set it explicitly to exactly 2x then Docker re-derives the same
	// number anyway — no behaviour is lost either way.
	if hc.MemorySwap != 0 && !derivedSwapDefault(hc.Memory, hc.MemorySwap) {
		set("memswap_limit", composeBytes(hc.MemorySwap))
	}

	// CPU.
	if hc.NanoCPUs > 0 {
		set("cpus", strconv.FormatFloat(float64(hc.NanoCPUs)/1e9, 'f', -1, 64))
	}
	if hc.CPUShares > 0 {
		set("cpu_shares", hc.CPUShares)
	}
	if hc.CpusetCpus != "" {
		set("cpuset", hc.CpusetCpus)
	}

	// Hardening. Emitted verbatim, including an inline seccomp profile if that is
	// what the container carries: a truncated security option would be worse than
	// a long one, and the header already says to review before use.
	if len(hc.SecurityOpt) > 0 {
		set("security_opt", append([]string(nil), hc.SecurityOpt...))
	}
	if hc.ReadonlyRootfs {
		set("read_only", true)
	}
	if hc.Init != nil && *hc.Init {
		set("init", true)
	}
	if len(hc.GroupAdd) > 0 {
		set("group_add", append([]string(nil), hc.GroupAdd...))
	}

	// Host wiring.
	if len(hc.ExtraHosts) > 0 {
		set("extra_hosts", append([]string(nil), hc.ExtraHosts...))
	}
	if len(hc.DNS) > 0 {
		set("dns", append([]string(nil), hc.DNS...))
	}
	if len(hc.DNSSearch) > 0 {
		set("dns_search", append([]string(nil), hc.DNSSearch...))
	}
	if len(hc.DNSOptions) > 0 {
		set("dns_opt", append([]string(nil), hc.DNSOptions...))
	}
	if devices := composeDevices(hc.Devices); len(devices) > 0 {
		set("devices", devices)
	}

	// Namespaces. Only `host` is a choice; the engine stamps the rest.
	if string(hc.PidMode) == "host" {
		set("pid", "host")
	}
	if string(hc.IpcMode) == "host" {
		set("ipc", "host")
	}
	if string(hc.UsernsMode) == "host" {
		set("userns_mode", "host")
	}

	// The rest.
	if hc.ShmSize > 0 && hc.ShmSize != defaultShmSize {
		set("shm_size", composeBytes(hc.ShmSize))
	}
	if len(hc.Sysctls) > 0 {
		set("sysctls", copyStringMap(hc.Sysctls))
	}
	if tmpfs := composeTmpfs(hc.Tmpfs); len(tmpfs) > 0 {
		set("tmpfs", tmpfs)
	}
	if ulimits := composeUlimits(hc.Ulimits); len(ulimits) > 0 {
		set("ulimits", ulimits)
	}
	if hc.Runtime != "" && hc.Runtime != "runc" {
		set("runtime", hc.Runtime)
	}
	if insp.Config != nil {
		if insp.Config.StopSignal != "" {
			set("stop_signal", insp.Config.StopSignal)
		}
		if insp.Config.StopTimeout != nil {
			set("stop_grace_period", fmt.Sprintf("%ds", *insp.Config.StopTimeout))
		}
	}
}

// derivedSwapDefault reports whether a swap limit is the one Docker computes on
// its own — twice the memory limit — rather than one anybody chose.
//
// Only meaningful alongside a memory limit: with no memory limit there is
// nothing to derive from, so any swap value present is a real choice.
func derivedSwapDefault(memory, swap int64) bool {
	return memory > 0 && swap == memory*2
}

// defaultShmSize is the 64 MiB the engine stamps into every inspect that did not
// ask for a size. Emitting it would put a line nobody chose on every service.
const defaultShmSize = 67108864

// composeBytes renders a byte count the way people write it in compose files: an
// exact gibi/mebi/kibi multiple as "2g"/"512m"/"64k", anything else as the raw
// integer, which compose also accepts.
//
// Readability is the whole point — `536870912` is correct and hostile, and the
// original file this is reconstructing said `512m`.
func composeBytes(v int64) any {
	if v <= 0 {
		return v // includes -1, "unlimited", which compose reads as-is
	}
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case v%gib == 0:
		return strconv.FormatInt(v/gib, 10) + "g"
	case v%mib == 0:
		return strconv.FormatInt(v/mib, 10) + "m"
	case v%kib == 0:
		return strconv.FormatInt(v/kib, 10) + "k"
	}
	return v
}

// composeDevices renders device mappings as compose writes them. The permission
// suffix is emitted only when it is NOT the default `rwm`, so an ordinary device
// stays a two-part entry.
func composeDevices(devices []container.DeviceMapping) []string {
	out := make([]string, 0, len(devices))
	for _, d := range devices {
		if d.PathOnHost == "" || d.PathInContainer == "" {
			continue
		}
		entry := d.PathOnHost + ":" + d.PathInContainer
		if d.CgroupPermissions != "" && d.CgroupPermissions != "rwm" {
			entry += ":" + d.CgroupPermissions
		}
		out = append(out, entry)
	}
	return out
}

// composeTmpfs renders the tmpfs map as the list compose expects — `path` alone
// when the mount has no options, `path:opts` when it does.
//
// Sorted, because a Go map has no order and an artifact that reshuffles between
// two captures of an unchanged container reads as a change that did not happen.
func composeTmpfs(tmpfs map[string]string) []string {
	out := make([]string, 0, len(tmpfs))
	for path, opts := range tmpfs {
		if path == "" {
			continue
		}
		if opts == "" {
			out = append(out, path)
			continue
		}
		out = append(out, path+":"+opts)
	}
	sort.Strings(out)
	return out
}

// composeUlimits renders ulimits in compose's mapping form. A limit whose soft
// and hard halves are equal is still written as both, because that is what the
// container asked for and compose's short form means something subtly different.
func composeUlimits(ulimits []*units.Ulimit) map[string]any {
	out := map[string]any{}
	for _, u := range ulimits {
		if u == nil || u.Name == "" {
			continue
		}
		out[u.Name] = map[string]any{"soft": u.Soft, "hard": u.Hard}
	}
	return out
}

// copyStringMap detaches a map from the inspect it came from, so the reconstruction
// can never mutate the caller's copy.
func copyStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// composeServiceNetworks renders the service-level attachment.
//
// A plain name list when no endpoint carries configuration — which is the common
// case and keeps existing reconstructions byte-stable — and compose's MAPPING
// form as soon as one does, because a list structurally cannot hold an alias or
// an address. That is the shape the wikijs stack needed and the reason its
// reconstruction could not describe itself.
//
// Two sources, in that order: the manifest's recorded NetworkRef (F89, which the
// app-consistent path also populates since #N3), then the live inspect as a
// fallback for a legacy backup whose manifest predates either. The address read
// is IPAMConfig — the address the container ASKED for — never the leased
// IPAddress, for the reason InspectNetworks states: pinning a lease nobody
// requested makes a dynamic container fail to start the moment it is taken.
//
// Note on ipv4_address: compose requires the network declaration to carry a
// subnet for it to be accepted. The doc-level declarations emit ipam pools
// whenever one was recorded, so a current backup is self-consistent; a legacy
// one with an address but no recorded subnet can produce a file compose rejects.
// That is visible and self-describing rather than silent, and the header already
// says to review before use.
func composeServiceNetworks(names []string, insp types.ContainerJSON, netDefs []NetworkRef, serviceName, containerName, hostname string) any {
	byName := map[string]NetworkRef{}
	for _, n := range netDefs {
		byName[n.Name] = n
	}
	mapped := map[string]any{}
	anyConfig := false
	for _, name := range names {
		entry := map[string]any{}
		def, recorded := byName[name]

		aliases := def.Aliases
		ipv4, ipv6 := def.IPv4, def.IPv6
		if !recorded && insp.NetworkSettings != nil {
			if ep := insp.NetworkSettings.Networks[name]; ep != nil {
				aliases = ep.Aliases
				if ep.IPAMConfig != nil {
					ipv4, ipv6 = ep.IPAMConfig.IPv4Address, ep.IPAMConfig.IPv6Address
				}
			}
		}

		kept := make([]string, 0, len(aliases))
		for _, a := range aliases {
			if composeAlias(a, serviceName, containerName, hostname) {
				kept = append(kept, a)
			}
		}
		if len(kept) > 0 {
			entry["aliases"] = kept
		}
		if ipv4 != "" {
			entry["ipv4_address"] = ipv4
		}
		if ipv6 != "" {
			entry["ipv6_address"] = ipv6
		}
		if len(entry) > 0 {
			anyConfig = true
		}
		mapped[name] = entry
	}
	if !anyConfig {
		return names
	}
	return mapped
}

// composeAlias reports whether an alias is worth writing into a compose file.
//
// Dropped are the names something else already registers, which would be noise
// at best and a stale lie at worst:
//
//   - the short-id shape, which names a container that no longer exists (#N4);
//   - this service's own compose name, which compose registers itself;
//   - the container name, which Docker registers itself;
//   - the hostname, which the file emits as `hostname:` (#N1) and Docker
//     registers from there on every user-defined network.
func composeAlias(a, serviceName, containerName, hostname string) bool {
	switch a {
	case "", serviceName, containerName, hostname:
		return false
	}
	return !dockercli.ShortIDAlias(a)
}

// mergeComposeDocs combines the single-service compose documents of a stack's
// members into one file (F190).
//
// Every service in a compose project records the SAME working directory and the
// SAME config filename, because that is where the project was defined. Each
// backup stores a one-service reconstruction of its own container — correct on
// its own, and wrong the moment five of them are written to one directory in
// turn: each overwrites the last, and what survives describes a single service
// out of five.
//
// So a stack restore merges them and writes once. Services are unioned by name;
// networks are unioned too, and a real definition beats a bare `external: true`,
// because only some members may have recorded one.
//
// A document that cannot be parsed is skipped rather than failing the merge: one
// unreadable member should cost its own service, not the other four.
//
// The merge is also the only place that can see a dependency and its target at
// once, so it is where `depends_on` is reconciled against the finished file
// (#16). The count of promoted conditions comes back so the caller can say what
// the written file asserts that the source did not.
func mergeComposeDocs(docs [][]byte) ([]byte, int, error) {
	services := map[string]any{}
	networks := map[string]any{}
	for _, raw := range docs {
		var doc map[string]any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			continue
		}
		if svcs, ok := doc["services"].(map[string]any); ok {
			for name, svc := range svcs {
				services[name] = svc
			}
		}
		if nets, ok := doc["networks"].(map[string]any); ok {
			for name, def := range nets {
				if prev, seen := networks[name]; seen && !isBareExternal(prev) {
					continue // already have a real definition; keep it
				}
				networks[name] = def
			}
		}
	}
	if len(services) == 0 {
		return nil, 0, fmt.Errorf("no services could be read from the stack's recorded compose files")
	}
	promoted := reconcileDependsOn(services)
	doc := map[string]any{"services": services}
	if len(networks) > 0 {
		doc["networks"] = networks
	}
	body, err := yaml.Marshal(doc)
	if err != nil {
		return nil, 0, err
	}
	return append([]byte(composeHeader), body...), promoted, nil
}

// Compose's dependency conditions. `service_started` waits only for the
// container to EXIST — R2 §Issue 16's race, where "the stack survived on the
// app's internal wait-for-db loop, which is luck, not design."
const (
	conditionStarted = "service_started"
	conditionHealthy = "service_healthy"
)

// composeDependsOn renders the recorded compose dependencies in long form, kept
// to the services that will share the finished document.
//
// The label reads "mariadb:service_started:false,redis:service_healthy:false" —
// name, condition, then flags this reconstruction has no use for. The recorded
// condition is emitted verbatim: a `service_completed_successfully` dependency
// on a one-shot init container is a real relationship, and inventing a different
// one in its place would misdescribe the stack.
func composeDependsOn(labels map[string]string, siblings map[string]bool) map[string]any {
	if siblings == nil || labels == nil {
		return nil
	}
	out := map[string]any{}
	for _, part := range strings.Split(labels["com.docker.compose.depends_on"], ",") {
		name, rest, _ := strings.Cut(strings.TrimSpace(part), ":")
		if name == "" || !siblings[name] {
			continue
		}
		condition, _, _ := strings.Cut(rest, ":")
		if condition == "" {
			condition = conditionStarted
		}
		out[name] = map[string]any{"condition": condition}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// composeHealthcheck renders a recorded probe in compose's shape. Durations are
// emitted as compose reads them ("10s"), and a zero one is left out so the
// file says only what the container actually specifies.
func composeHealthcheck(probe *container.HealthConfig) map[string]any {
	if probe == nil || len(probe.Test) == 0 {
		return nil
	}
	out := map[string]any{"test": []string(probe.Test)}
	if probe.Interval > 0 {
		out["interval"] = probe.Interval.String()
	}
	if probe.Timeout > 0 {
		out["timeout"] = probe.Timeout.String()
	}
	if probe.StartPeriod > 0 {
		out["start_period"] = probe.StartPeriod.String()
	}
	if probe.Retries > 0 {
		out["retries"] = probe.Retries
	}
	return out
}

// reconcileDependsOn makes the merged document's dependencies true of the
// document it is in, and promotes the ones that were racing (#16).
//
// Two passes' worth of work, one walk:
//
//   - A dependency on a service that is not in the file is dropped. Compose
//     refuses to start a project with an undefined dependency, so a member whose
//     live container could not be read must cost its own entry rather than the
//     whole file's usability.
//   - A `service_started` dependency on a DATA-TIER service that HAS a probe
//     becomes `service_healthy`. That is the fix R2 applied by hand, and the
//     reconstruction is an artifact — it documents what this stack should say,
//     and changes nothing about the containers that are already running.
//
// Data-tier only, by the same two heuristics the restore ORDER already trusts.
// Waiting for a database to be ready is what the evidence is about; making every
// service in a stack wait on every other one's probe is a different and much
// larger change to how the project starts.
func reconcileDependsOn(services map[string]any) int {
	promoted := 0
	for _, svc := range services {
		deps, ok := dependsOnMap(svc)
		if !ok {
			continue
		}
		for name, entry := range deps {
			target, present := services[name].(map[string]any)
			if !present {
				delete(deps, name)
				continue
			}
			settings, ok := entry.(map[string]any)
			if !ok || settings["condition"] != conditionStarted {
				continue
			}
			if _, hasProbe := target["healthcheck"]; !hasProbe || !dataTierService(name, target) {
				continue
			}
			settings["condition"] = conditionHealthy
			promoted++
		}
	}
	return promoted
}

// dependsOnMap returns a service's long-form dependencies, if it declares any.
func dependsOnMap(svc any) (map[string]any, bool) {
	fields, ok := svc.(map[string]any)
	if !ok {
		return nil, false
	}
	deps, ok := fields["depends_on"].(map[string]any)
	return deps, ok
}

// dataTierService reports whether a merged service is a database, by the image
// and the name — the same two heuristics that order a stack restore.
func dataTierService(name string, svc map[string]any) bool {
	image, _ := svc["image"].(string)
	return isDataImage(image) || isDataServiceName(name)
}

// isBareExternal reports whether a network declaration says only "this exists
// elsewhere" — the fallback used when nothing about the network was recorded.
func isBareExternal(def any) bool {
	m, ok := def.(map[string]any)
	if !ok {
		return false
	}
	if len(m) != 1 {
		return false
	}
	ext, ok := m["external"].(bool)
	return ok && ext
}

// BindBasesForProject reads the base directory a stack's bind mounts live
// under, from a manifest's recorded volumes (F192).
//
// The rule is the project-name segment: /volume1/docker/bookstack/db belongs to
// project "bookstack", so its base is /volume1/docker. Only sources that
// actually contain the project as a whole path segment vote — an unrelated bind
// (/etc/localtime, a media share) says nothing about where the stack lives and
// must not out-vote the ones that do.
func BindBasesForProject(vols []VolumeRef, project string) []string {
	if project == "" {
		return nil
	}
	needle := "/" + project + "/"
	var out []string
	for _, v := range vols {
		if v.Type != "bind" || v.Source == "" {
			continue
		}
		src := path.Clean(v.Source)
		if i := strings.Index(src+"/", needle); i > 0 {
			out = append(out, src[:i])
		}
	}
	return out
}

// PreferProjectLeaf swaps a reconstruction folder's leaf for the compose
// project's name when the recorded one is a deployment tool's internal id
// (F192).
//
// Portainer is the motivating case: it runs compose from /data/compose/26, so a
// faithful reconstruction lands the operator a folder literally named "26" —
// meaningless on sight, colliding with the next numeric id, and not even a path
// that exists outside the tool's own container. When the recorded leaf differs
// from the project name AND the operator gave a target base to build under, the
// project name wins. With no target base there is nothing safer to prefer, so
// the recorded directory stands.
func PreferProjectLeaf(dir, project, targetBase string) (string, bool) {
	if dir == "" || project == "" || path.Base(dir) == project {
		return dir, false
	}
	base := strings.TrimRight(strings.TrimSpace(targetBase), "/")
	if base == "" {
		return dir, false
	}
	return base + "/" + project, true
}

// splitComposeSecrets moves secret-valued environment entries out of a compose
// document into a .env file, leaving ${VAR} references behind (F194).
//
// The reconstruction is built from docker inspect, so its environment is fully
// resolved — including every password and key the operator's own compose pulled
// from a .env. Written as-is, that file both leaks on sight and diverges from
// how the operator actually works: their compose says ${DB_PASSWORD}, ours said
// the password. Splitting restores the shape people keep in version control —
// a shareable compose file beside a .env that never leaves the machine.
//
// Which keys move is decided by NAME, with the same secret vocabulary the
// address reporter uses (secretKeySubstrings) — one list, one opinion about
// what a secret looks like. A value that dotenv could misread — quotes, '#',
// '$', whitespace, a newline — stays inline rather than moved wrongly: a
// password that round-trips broken is worse than one left in place.
//
// Two services carrying the SAME key with DIFFERENT values get service-scoped
// names (BOOKSTACK_DB_PASSWORD), because one .env serves the whole file.
func splitComposeSecrets(doc []byte) (compose, envFile []byte, moved int) {
	var parsed map[string]any
	if err := yaml.Unmarshal(doc, &parsed); err != nil {
		return doc, nil, 0
	}
	services, ok := parsed["services"].(map[string]any)
	if !ok {
		return doc, nil, 0
	}
	envVals := map[string]string{} // env-file variable -> value
	svcNames := make([]string, 0, len(services))
	for name := range services {
		svcNames = append(svcNames, name)
	}
	sort.Strings(svcNames)
	for _, svcName := range svcNames {
		svc, ok := services[svcName].(map[string]any)
		if !ok {
			continue
		}
		env, ok := svc["environment"].(map[string]any)
		if !ok {
			continue
		}
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v, isStr := env[k].(string)
			if !isStr || !secretEnvKey(k) || !dotenvSafe(v) {
				continue
			}
			varName := k
			if prev, taken := envVals[varName]; taken && prev != v {
				varName = envVarName(svcName) + "_" + k
			}
			if prev, taken := envVals[varName]; taken && prev != v {
				continue // still colliding; leave this one inline rather than guess
			}
			envVals[varName] = v
			env[k] = "${" + varName + "}"
			moved++
		}
	}
	if moved == 0 {
		return doc, nil, 0
	}
	body, err := yaml.Marshal(parsed)
	if err != nil {
		return doc, nil, 0
	}
	// The compose header survives the round trip; the .env carries its own.
	head := ""
	if i := bytes.Index(doc, []byte("\nservices:")); i >= 0 && bytes.HasPrefix(doc, []byte("#")) {
		head = string(doc[:findHeaderEnd(doc)])
	}
	names := make([]string, 0, len(envVals))
	for n := range envVals {
		names = append(names, n)
	}
	sort.Strings(names)
	var envOut strings.Builder
	envOut.WriteString("# .env — written by DockBack beside the reconstructed compose file.\n")
	envOut.WriteString("# These values came out of the running containers' configuration; the compose\n")
	envOut.WriteString("# file references them as ${VAR}. Keep this file out of version control.\n")
	for _, n := range names {
		envOut.WriteString(n + "=" + envVals[n] + "\n")
	}
	return append([]byte(head), body...), []byte(envOut.String()), moved
}

// findHeaderEnd returns the end of the leading comment block.
func findHeaderEnd(doc []byte) int {
	end := 0
	for _, line := range bytes.SplitAfter(doc, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("#")) {
			break
		}
		end += len(line)
	}
	return end
}

// secretEnvKey reuses the one secret vocabulary this package has.
func secretEnvKey(k string) bool {
	up := strings.ToUpper(k)
	for _, sub := range secretKeySubstrings {
		if strings.Contains(up, sub) {
			return true
		}
	}
	return false
}

// dotenvSafe reports whether a value survives a dotenv round trip verbatim.
// Anything compose's .env parser could reinterpret stays inline instead.
func dotenvSafe(v string) bool {
	if v == "" {
		return false
	}
	return !strings.ContainsAny(v, "\n#$\"'` ") && strings.TrimSpace(v) == v
}

// envVarName renders a compose service name as an env-var prefix.
func envVarName(service string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(service) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

// userLabels drops Compose-managed labels (Compose re-adds its own on up).
func userLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range labels {
		if strings.HasPrefix(k, "com.docker.compose.") {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// portList renders host port bindings as "host:container[/proto]" entries.
func portList(insp types.ContainerJSON) []string {
	if insp.HostConfig == nil || len(insp.HostConfig.PortBindings) == 0 {
		return nil
	}
	var out []string
	for portProto, binds := range insp.HostConfig.PortBindings {
		pp := string(portProto) // "80/tcp"
		cport, proto, _ := strings.Cut(pp, "/")
		suffix := ""
		if proto != "" && proto != "tcp" {
			suffix = "/" + proto
		}
		for _, b := range binds {
			var entry string
			switch {
			case b.HostIP != "" && b.HostIP != "0.0.0.0":
				entry = fmt.Sprintf("%s:%s:%s%s", b.HostIP, b.HostPort, cport, suffix)
			case b.HostPort != "":
				entry = fmt.Sprintf("%s:%s%s", b.HostPort, cport, suffix)
			default:
				entry = cport + suffix
			}
			out = append(out, entry)
		}
	}
	sort.Strings(out)
	return out
}

// volumeList renders mounts as "source:dest[:ro]" — named volume by name, bind
// by host source path.
func volumeList(insp types.ContainerJSON) []string {
	if len(insp.Mounts) == 0 {
		return nil
	}
	var out []string
	for _, m := range insp.Mounts {
		var src string
		switch string(m.Type) {
		case "volume":
			src = m.Name
		case "bind":
			src = m.Source
		default:
			src = m.Source
		}
		if src == "" || m.Destination == "" {
			continue
		}
		entry := src + ":" + m.Destination
		if !m.RW {
			entry += ":ro"
		}
		out = append(out, entry)
	}
	sort.Strings(out)
	return out
}

// networkList returns the user-defined networks the container is attached to,
// excluding Docker's built-in modes.
// composePools renders a network's IPAM pools in compose's shape.
func composePools(def NetworkRef) []map[string]any {
	src := def.Pools
	if len(src) == 0 && def.Subnet != "" {
		src = []NetworkPoolRef{{Subnet: def.Subnet, Gateway: def.Gateway, IPRange: def.IPRange}}
	}
	out := make([]map[string]any, 0, len(src))
	for _, p := range src {
		if p.Subnet == "" {
			continue
		}
		e := map[string]any{"subnet": p.Subnet}
		if p.Gateway != "" {
			e["gateway"] = p.Gateway
		}
		if p.IPRange != "" {
			e["ip_range"] = p.IPRange
		}
		out = append(out, e)
	}
	return out
}

func networkList(insp types.ContainerJSON) []string {
	if insp.NetworkSettings == nil || len(insp.NetworkSettings.Networks) == 0 {
		return nil
	}
	var out []string
	for n := range insp.NetworkSettings.Networks {
		switch n {
		case "bridge", "host", "none":
			continue
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// F231 — the .env captured from the source host, brought back through the remaps.
//
// A compose file that reads ${DOMAIN} has no literal for a domain remap to
// rewrite; the value is in the .env beside it. Restoring the containers with a
// remapped environment and leaving that file behind meant the next
// `docker compose up` re-interpolated the OLD value and undid the move — which
// is exactly the shape of the bug this fixes, and why the remap has to reach
// this file rather than only the compose document.

// RemapEnvFile applies the same three text remaps the compose file gets. Returns
// the rewritten content and how many substitutions each made, so the caller can
// say what it changed rather than claiming it did something.
func RemapEnvFile(env []byte, fromIP, toIP, fromDomain, toDomain, fromPath, toPath string) (out []byte, ips, domains, paths int) {
	out = env
	if fromIP != "" && toIP != "" {
		if b, n := dockercli.RemapTextHostIP(out, fromIP, toIP); n > 0 {
			out, ips = b, n
		}
	}
	if fromDomain != "" && toDomain != "" {
		if b, n := dockercli.RemapTextHostDomain(out, fromDomain, toDomain); n > 0 {
			out, domains = b, n
		}
	}
	if fromPath != "" && toPath != "" && fromPath != toPath {
		if b, n := dockercli.RemapTextHostPath(out, fromPath, toPath); n > 0 {
			out, paths = b, n
		}
	}
	return out, ips, domains, paths
}

// envKeys lists the KEY names an env file defines, in order, skipping comments
// and blanks. Pure.
func envKeys(env []byte) []string {
	var out []string
	for _, line := range strings.Split(string(env), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if k, _, ok := strings.Cut(t, "="); ok {
			if k = strings.TrimSpace(k); k != "" {
				out = append(out, k)
			}
		}
	}
	return out
}

// MergeEnvFiles puts the operator's own .env first and appends only the
// generated entries it does not already define.
//
// Order matters and the operator's file wins: their DOMAIN is the one the stack
// is actually configured around, and DockBack's generated entries exist only to
// satisfy the ${VAR} references in the compose file IT wrote. A key defined in
// both keeps the operator's value — overwriting somebody's own configuration
// with a value extracted from a reconstruction would be the wrong way round.
//
// Pure, so the precedence is unit-testable.
func MergeEnvFiles(original, generated []byte) []byte {
	if len(strings.TrimSpace(string(original))) == 0 {
		return generated
	}
	if len(strings.TrimSpace(string(generated))) == 0 {
		return original
	}
	have := map[string]bool{}
	for _, k := range envKeys(original) {
		have[k] = true
	}
	var add []string
	for _, line := range strings.Split(string(generated), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		k, _, ok := strings.Cut(t, "=")
		if !ok || have[strings.TrimSpace(k)] {
			continue
		}
		add = append(add, line)
	}
	if len(add) == 0 {
		return original
	}
	out := strings.TrimRight(string(original), "\n")
	out += "\n\n# Added by DockBack: values the reconstructed compose file references.\n"
	out += strings.Join(add, "\n") + "\n"
	return []byte(out)
}
