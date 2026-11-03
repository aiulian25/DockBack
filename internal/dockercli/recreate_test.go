package dockercli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	nat "github.com/docker/go-connections/nat"
)

// F10: stripForClone must isolate a restore-as-copy so it can NEVER touch the
// original's data or ports — fresh empty volumes at every mount destination, no
// host-port bindings, no restart loop, no stack links.
func TestStripForClone(t *testing.T) {
	insp := &types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{
			HostConfig: &container.HostConfig{
				PortBindings:    nat.PortMap{"80/tcp": []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "8080"}}},
				PublishAllPorts: true,
				Binds:           []string{"appdata:/data", "/host/path:/config"},
				Mounts:          []mount.Mount{{Type: mount.TypeVolume, Source: "extra", Target: "/extra"}},
				VolumesFrom:     []string{"other"},
				Links:           []string{"db:db"},
				RestartPolicy:   container.RestartPolicy{Name: "always"},
			},
		},
		Config: &container.Config{Image: "app:1"},
		Mounts: []types.MountPoint{
			{Destination: "/data"},
			{Destination: "/config"},
			{Destination: "/var/run/docker.sock"},
		},
	}

	stripForClone(insp)
	hc := insp.HostConfig

	if len(hc.PortBindings) != 0 || hc.PublishAllPorts {
		t.Errorf("clone must publish no host ports, got bindings=%v publishAll=%v", hc.PortBindings, hc.PublishAllPorts)
	}
	if hc.Binds != nil || hc.Mounts != nil || hc.VolumesFrom != nil {
		t.Errorf("clone must drop all real volume/bind sources: binds=%v mounts=%v volumesFrom=%v", hc.Binds, hc.Mounts, hc.VolumesFrom)
	}
	if hc.Links != nil {
		t.Errorf("clone must drop stack links, got %v", hc.Links)
	}
	if hc.RestartPolicy.Name != "no" {
		t.Errorf("clone restart policy = %q, want \"no\"", hc.RestartPolicy.Name)
	}
	// Every original mount destination becomes a FRESH anonymous volume, so the
	// restore fills the clone's own volumes rather than the live container's.
	for _, dest := range []string{"/data", "/config", "/var/run/docker.sock"} {
		if _, ok := insp.Config.Volumes[dest]; !ok {
			t.Errorf("clone missing a fresh anonymous volume at %q: %v", dest, insp.Config.Volumes)
		}
	}
}

// F219: a clone must not inherit the labels that make it SOMEBODY ELSE. It used
// to keep the original's compose identity, so Docker — and DockBack reading
// Docker — saw a second member of the live stack: listed among its services,
// counted as a member with no backup, and a candidate for the next
// app-consistent snapshot. It kept the dockback.* policy labels too, which is
// how a throwaway copy acquires a backup schedule.
func TestStripForCloneDropsIdentityLabels(t *testing.T) {
	insp := &types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{HostConfig: &container.HostConfig{}},
		Config: &container.Config{Image: "app:1", Labels: map[string]string{
			"com.docker.compose.project":     "blog",
			"com.docker.compose.service":     "app",
			"dockback.enable":                "true",
			"dockback.schedule":              "nightly",
			"org.opencontainers.image.title": "App",
			"maintainer":                     "someone",
		}},
	}

	stripForClone(insp)

	for _, gone := range []string{"com.docker.compose.project", "com.docker.compose.service", "dockback.enable", "dockback.schedule"} {
		if _, still := insp.Config.Labels[gone]; still {
			t.Errorf("a clone must not inherit %q: %v", gone, insp.Config.Labels)
		}
	}
	// Everything else is the image's own metadata and says nothing about who this
	// container is to DockBack — dropping it would be vandalism, not isolation.
	for _, kept := range []string{"org.opencontainers.image.title", "maintainer"} {
		if _, ok := insp.Config.Labels[kept]; !ok {
			t.Errorf("label %q is not an identity claim and must survive: %v", kept, insp.Config.Labels)
		}
	}
}

// The test-clone marker is applied AFTER the strip, so the sweep that removes
// identity labels can never remove the one thing that makes the clone reapable.
func TestCloneLabelsSurviveTheStrip(t *testing.T) {
	insp := &types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{HostConfig: &container.HostConfig{}},
		Config:            &container.Config{Image: "app:1", Labels: map[string]string{"com.docker.compose.project": "blog"}},
	}
	stripForClone(insp)
	applyCloneLabels(insp, map[string]string{"com.dockback.test_clone": "1893456000"})

	if insp.Config.Labels["com.dockback.test_clone"] != "1893456000" {
		t.Errorf("the expiry marker must be on the created clone: %v", insp.Config.Labels)
	}
	// And a clone with no labels at all still gets a usable map rather than a
	// write to nil.
	bare := &types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{HostConfig: &container.HostConfig{}},
		Config:            &container.Config{Image: "app:1"},
	}
	applyCloneLabels(bare, map[string]string{"com.dockback.test_clone": "1"})
	if bare.Config.Labels["com.dockback.test_clone"] != "1" {
		t.Errorf("labels must be created when there were none: %v", bare.Config.Labels)
	}
	// No labels asked for: nothing invented.
	untouched := &types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{HostConfig: &container.HostConfig{}},
		Config:            &container.Config{Image: "app:1"},
	}
	applyCloneLabels(untouched, nil)
	if untouched.Config.Labels != nil {
		t.Errorf("an ordinary clone must gain no labels: %v", untouched.Config.Labels)
	}
}

// The comment said it dropped the auto-added short-id alias; the body did not.
//
// Every recreated container therefore carried the PREVIOUS container's short id
// as a working DNS alias, and they accumulate: the recorded inspect of a
// restored container includes the stale one, the next restore preserves it and
// adds its own. Confirmed in real recorded data during entry 3 — an
// app-consistent capture wrote `aliases: ["14eae861b80d", "nc-database"]`, where
// the first entry is the container's own short id.
func TestCleanAliasesDropsShortIDs(t *testing.T) {
	t.Run("the example from the plan", func(t *testing.T) {
		got := cleanAliases([]string{"wiki-js-db", "0123456789ab", "db"}, "wiki-js-db")
		want := []string{"wiki-js-db", "db"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("cleanAliases = %v, want %v", got, want)
		}
	})

	t.Run("the hostname is prepended and deduplicated", func(t *testing.T) {
		// The wikijs mechanism: Docker registers a container's hostname on every
		// user-defined network, which is why `DB_HOST=wiki-js-db` resolved at all.
		if got := cleanAliases(nil, "wiki-js-db"); !reflect.DeepEqual(got, []string{"wiki-js-db"}) {
			t.Fatalf("cleanAliases(nil, host) = %v", got)
		}
		if got := cleanAliases([]string{"wiki-js-db"}, "wiki-js-db"); len(got) != 1 {
			t.Fatalf("the hostname must not appear twice: %v", got)
		}
	})

	t.Run("names that only look like ids survive", func(t *testing.T) {
		for _, alias := range []string{
			"NotHex12Chars", // wrong shape and uppercase
			"abcdef",        // too short
			"0123456789abc", // 13 characters
			"0123456789a",   // 11 characters
			"0123456789AB",  // 12 chars, uppercase hex — not what Docker writes
			"g123456789ab",  // 12 chars, not hex
			"deadbeef-cafe", // 13 with a separator
			"web",           // an ordinary service alias
			"nc-database",   // the real alias from the entry-3 capture
		} {
			if got := cleanAliases([]string{alias}, ""); len(got) != 1 || got[0] != alias {
				t.Errorf("%q must survive, got %v", alias, got)
			}
		}
	})

	t.Run("every short-id shape is dropped", func(t *testing.T) {
		for _, alias := range []string{
			"0123456789ab",
			"14eae861b80d", // the one entry 3 found in a real manifest
			"bbe5dd40c7de",
			"000000000000",
			"ffffffffffff",
		} {
			if got := cleanAliases([]string{alias}, ""); len(got) != 0 {
				t.Errorf("%q is a short id and must be dropped, got %v", alias, got)
			}
		}
	})

	t.Run("stale ids accumulated across generations all go", func(t *testing.T) {
		// What a container restored three times looks like: its own id plus every
		// incarnation before it, with the one real name buried among them.
		got := cleanAliases([]string{"14eae861b80d", "bbe5dd40c7de", "nc-database", "0123456789ab"}, "wiki-js-db")
		want := []string{"wiki-js-db", "nc-database"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("cleanAliases = %v, want %v", got, want)
		}
	})

	t.Run("empty in, empty out", func(t *testing.T) {
		if got := cleanAliases(nil, ""); len(got) != 0 {
			t.Fatalf("cleanAliases(nil, \"\") = %v, want empty", got)
		}
		if got := cleanAliases([]string{"", "web"}, ""); !reflect.DeepEqual(got, []string{"web"}) {
			t.Fatalf("an empty alias must be skipped: %v", got)
		}
	})
}

func TestShortIDAlias(t *testing.T) {
	for _, yes := range []string{"0123456789ab", "abcdef012345", "000000000000", "ffffffffffff"} {
		if !ShortIDAlias(yes) {
			t.Errorf("%q is a 12-char lowercase hex short id", yes)
		}
	}
	for _, no := range []string{"", "abcdef", "0123456789abc", "0123456789AB", "g123456789ab", "web", "0123456789a-"} {
		if ShortIDAlias(no) {
			t.Errorf("%q is not a short id", no)
		}
	}
}

// fakeDockerForRecreate answers what RecreateContainer asks and records what it
// removes. createStatus decides the answer to the create itself.
func fakeDockerForRecreate(t *testing.T, createStatus int) (*client.Client, *[]string) {
	t.Helper()
	removed := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("API-Version", "1.44")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodDelete:
			*removed = append(*removed, path.Base(r.URL.Path))
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			w.WriteHeader(createStatus)
			if createStatus == http.StatusConflict {
				_, _ = w.Write([]byte(`{"message":"the container name is already in use"}`))
				return
			}
			_, _ = w.Write([]byte(`{"Id":"new-id"}`))
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			_, _ = w.Write([]byte(`[{"Id":"unrelated-id","Names":["/taken"]}]`))
		case strings.Contains(r.URL.Path, "/images/") && strings.HasSuffix(r.URL.Path, "/json"):
			_, _ = w.Write([]byte(`{"Id":"sha256:recorded","RepoDigests":[]}`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(srv.Close)
	c, err := client.NewClientWithOpts(client.WithHost("tcp://"+srv.Listener.Addr().String()), client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, removed
}

const recreateInspect = `{"Name":"/taken","Image":"sha256:recorded","Config":{"Image":"app:1"},"HostConfig":{}}`

// The create conflict is the atomic answer to "is this name free". Whatever holds
// the name when it comes, a copy gives up rather than clearing the way.
func TestCloneNeverRemovesAContainerHoldingItsName(t *testing.T) {
	c, removed := fakeDockerForRecreate(t, http.StatusConflict)

	_, _, _, err := RecreateContainer(context.Background(), c, []byte(recreateInspect), "", &CloneOptions{NewName: "taken"}, nil, false)

	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Errorf("the conflict must reach the caller, got %v", err)
	}
	if len(*removed) != 0 {
		t.Errorf("a clone removed %v", *removed)
	}
}

// An in-place recreate replaces its own container, which is what it was asked for.
func TestRecreateInPlaceStillReplacesItsContainer(t *testing.T) {
	c, removed := fakeDockerForRecreate(t, http.StatusCreated)

	id, _, _, err := RecreateContainer(context.Background(), c, []byte(recreateInspect), "", nil, nil, false)

	if err != nil || id != "new-id" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if !slices.Contains(*removed, "unrelated-id") {
		t.Errorf("the recreate must replace the container of that name, removed %v", *removed)
	}
}

func dangerousHostConfig() *container.HostConfig {
	return &container.HostConfig{
		Privileged:     true,
		CapAdd:         []string{"SYS_ADMIN", "CAP_NET_ADMIN", "ALL", "CHOWN"},
		CapDrop:        []string{"ALL"},
		NetworkMode:    "host",
		PidMode:        "host",
		IpcMode:        "host",
		UTSMode:        "host",
		UsernsMode:     "host",
		CgroupnsMode:   "host",
		Runtime:        "nvidia",
		SecurityOpt:    []string{"seccomp=unconfined", "apparmor=unconfined", "no-new-privileges:true"},
		Sysctls:        map[string]string{"net.ipv4.ip_forward": "1"},
		ExtraHosts:     []string{"db:10.0.0.5"},
		DNS:            []string{"10.0.0.53"},
		VolumeDriver:   "remote-storage",
		LogConfig:      container.LogConfig{Type: "syslog", Config: map[string]string{"syslog-address": "udp://10.0.0.9:514"}},
		ReadonlyRootfs: true,
		ShmSize:        1024,
		GroupAdd:       []string{"1000"},
		Tmpfs:          map[string]string{"/run": ""},
		Resources: container.Resources{
			Memory:            512,
			NanoCPUs:          1000,
			Devices:           []container.DeviceMapping{{PathOnHost: "/dev/dri", PathInContainer: "/dev/dri", CgroupPermissions: "rwm"}},
			DeviceCgroupRules: []string{"c 1:3 rmw"},
			DeviceRequests:    []container.DeviceRequest{{Driver: "nvidia"}},
			CgroupParent:      "/host.slice",
		},
	}
}

func dangerousClone() *types.ContainerJSON {
	return &types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{HostConfig: dangerousHostConfig()},
		Config:            &container.Config{Image: "app:1"},
	}
}

// A copy is built from what is known to be safe, not from the original minus a
// list: a host power this code has never heard of must not come through.
func TestStripForCloneKeepsNoHostPowers(t *testing.T) {
	insp := dangerousClone()
	stripForClone(insp)
	hc := insp.HostConfig

	if hc.Privileged {
		t.Error("a copy must not be privileged")
	}
	if !slices.Equal(hc.CapAdd, []string{"CHOWN"}) {
		t.Errorf("only capabilities Docker grants by default may be added back, got %v", hc.CapAdd)
	}
	if string(hc.NetworkMode) != "none" {
		t.Errorf("a copy has no network, got %q", hc.NetworkMode)
	}
	if hc.PidMode != "" || hc.IpcMode != "" || hc.UTSMode != "" || hc.UsernsMode != "" || hc.CgroupnsMode != "" {
		t.Errorf("host namespaces must go: %+v", hc)
	}
	if hc.Runtime != "" || len(hc.Sysctls) != 0 || len(hc.ExtraHosts) != 0 || len(hc.DNS) != 0 || hc.VolumeDriver != "" || hc.LogConfig.Type != "" {
		t.Errorf("runtime, sysctls, name resolution, volume driver and log shipping must go: %+v", hc)
	}
	if len(hc.Devices) != 0 || len(hc.DeviceCgroupRules) != 0 || len(hc.DeviceRequests) != 0 || hc.CgroupParent != "" {
		t.Errorf("devices must go: %+v", hc.Resources)
	}
	if !slices.Equal(hc.SecurityOpt, []string{"no-new-privileges:true"}) {
		t.Errorf("an option that loosens confinement must go and one that tightens it stay, got %v", hc.SecurityOpt)
	}
	// What only limits the copy, or that the application needs to start, stays.
	if !hc.ReadonlyRootfs || hc.Memory != 512 || hc.NanoCPUs != 1000 || hc.ShmSize != 1024 ||
		!slices.Equal(hc.CapDrop, []string{"ALL"}) || !slices.Equal(hc.GroupAdd, []string{"1000"}) || hc.Tmpfs["/run"] != "" || len(hc.Tmpfs) != 1 {
		t.Errorf("limits the original set must survive: %+v", hc)
	}
}

// What the sandbox takes away decides what a copy can prove, so it is said by name.
func TestCloneSandboxRemovalsNamesWhatWasTaken(t *testing.T) {
	raw, err := json.Marshal(dangerousClone())
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(CloneSandboxRemovals(raw), "; ")
	for _, want := range []string{"privileged", "SYS_ADMIN", "NET_ADMIN", "/dev/dri", "GPU", "host network", "PID", "nvidia"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is missing from %q", want, got)
		}
	}
	if strings.Contains(got, "CHOWN") {
		t.Errorf("a default capability was not taken away: %q", got)
	}

	plain, err := json.Marshal(&types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{HostConfig: &container.HostConfig{NetworkMode: "bridge"}},
		Config:            &container.Config{Image: "app:1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := CloneSandboxRemovals(plain); len(got) != 0 {
		t.Errorf("an ordinary container loses nothing worth naming: %v", got)
	}
}

// recreateCalls is a fake daemon's record of what a recreate did, in order.
func fakeDockerRecordingCalls(t *testing.T, createStatus int) (*client.Client, *[]string) {
	t.Helper()
	calls := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("API-Version", "1.44")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodDelete:
			*calls = append(*calls, "remove "+path.Base(r.URL.Path))
		case strings.HasSuffix(r.URL.Path, "/rename"):
			*calls = append(*calls, "rename "+r.URL.Query().Get("name"))
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			*calls = append(*calls, "create "+r.URL.Query().Get("name"))
			w.WriteHeader(createStatus)
			if createStatus != http.StatusCreated {
				_, _ = w.Write([]byte(`{"message":"invalid mount config"}`))
				return
			}
			_, _ = w.Write([]byte(`{"Id":"new-id"}`))
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			_, _ = w.Write([]byte(`[{"Id":"prior-container-id","Names":["/taken"]}]`))
		case strings.Contains(r.URL.Path, "/images/") && strings.HasSuffix(r.URL.Path, "/json"):
			_, _ = w.Write([]byte(`{"Id":"sha256:recorded","RepoDigests":[]}`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(srv.Close)
	c, err := client.NewClientWithOpts(client.WithHost("tcp://"+srv.Listener.Addr().String()), client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, calls
}

const priorHoldingName = "taken-dockback-prior-prior-contai"

// The old order removed the container and then failed the create, leaving nothing.
func TestAFailedCreatePutsTheOriginalBack(t *testing.T) {
	c, calls := fakeDockerRecordingCalls(t, http.StatusBadRequest)

	_, _, _, err := RecreateContainer(context.Background(), c, []byte(recreateInspect), "", nil, nil, false)

	if !errors.Is(err, ErrNothingReplaced) {
		t.Errorf("the caller must learn nothing was replaced, got %v", err)
	}
	want := []string{"rename " + priorHoldingName, "create taken", "rename taken"}
	if !slices.Equal(*calls, want) {
		t.Errorf("calls = %v, want %v", *calls, want)
	}
}

// putPriorBack takes no context on purpose: a cancel mid-recreate must still give
// the original its name back, because a holding name is not the stop asked for.
func TestPuttingTheOriginalBackIgnoresACanceledContext(t *testing.T) {
	c, calls := fakeDockerRecordingCalls(t, http.StatusBadRequest)
	if err := putPriorBack(c, "prior-container-id", "taken"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(*calls, []string{"rename taken"}) {
		t.Errorf("calls = %v", *calls)
	}
}

func TestTheOriginalGoesOnlyOnceItsReplacementExists(t *testing.T) {
	c, calls := fakeDockerRecordingCalls(t, http.StatusCreated)

	id, _, _, err := RecreateContainer(context.Background(), c, []byte(recreateInspect), "", nil, nil, false)

	if err != nil || id != "new-id" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	want := []string{"rename " + priorHoldingName, "create taken", "remove prior-container-id"}
	if !slices.Equal(*calls, want) {
		t.Errorf("calls = %v, want %v", *calls, want)
	}
}

// A refused image never touches the existing container, so that is not replaced either.
func TestABadSavedConfigReplacesNothing(t *testing.T) {
	c, calls := fakeDockerRecordingCalls(t, http.StatusCreated)

	_, _, _, err := RecreateContainer(context.Background(), c, []byte(`{not json`), "", nil, nil, false)

	if !errors.Is(err, ErrNothingReplaced) {
		t.Errorf("got %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("nothing should have happened: %v", *calls)
	}
}

// fakeDockerWithNames lists the given containers, and records renames.
func fakeDockerWithNames(t *testing.T, names ...string) (*client.Client, *[]string) {
	t.Helper()
	renames := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("API-Version", "1.44")
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/rename") {
			*renames = append(*renames, r.URL.Query().Get("name"))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		list := []map[string]any{}
		for _, n := range names {
			list = append(list, map[string]any{"Id": "id-" + n, "Names": []string{"/" + n}})
		}
		_ = json.NewEncoder(w).Encode(list)
	}))
	t.Cleanup(srv.Close)
	c, err := client.NewClientWithOpts(client.WithHost("tcp://"+srv.Listener.Addr().String()), client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, renames
}

// The process died between setting a container aside and replacing it.
func TestRestorePriorNameGivesTheNameBack(t *testing.T) {
	c, renames := fakeDockerWithNames(t, "app-dockback-prior-abc123")
	if !RestorePriorName(context.Background(), c, "app") {
		t.Error("the name is free and the original is there")
	}
	if !slices.Equal(*renames, []string{"app"}) {
		t.Errorf("renames = %v", *renames)
	}
}

// Something holds the name now, and it is not this function's to take back.
func TestRestorePriorNameLeavesATakenNameAlone(t *testing.T) {
	c, renames := fakeDockerWithNames(t, "app", "app-dockback-prior-abc123")
	if RestorePriorName(context.Background(), c, "app") || len(*renames) != 0 {
		t.Errorf("renames = %v", *renames)
	}
	other, otherRenames := fakeDockerWithNames(t, "application")
	if RestorePriorName(context.Background(), other, "app") || len(*otherRenames) != 0 {
		t.Errorf("an unrelated container was renamed: %v", *otherRenames)
	}
}
