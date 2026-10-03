package backup

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// F184 — the operator says which ids restored data ends up owned by.
//
// Asked for after F182: detection covers images that ANNOUNCE the ids they drop
// to, which is a smaller set than the images people run. An image running as a
// baked-in user (www-data, or a numeric USER in its Dockerfile) announces
// nothing, so there is nothing to detect — and moving off a NAS is exactly when
// the numbers change.
func TestParseOwnership(t *testing.T) {
	ok := map[string][2]int{
		"1000:1000": {1000, 1000},
		"1026:100":  {1026, 100},
		" 33 : 33 ": {33, 33},
		"0:0":       {0, 0}, // root is a real answer for a container that runs as root
	}
	for in, want := range ok {
		uid, gid, good := ParseOwnership(in)
		if !good || uid != want[0] || gid != want[1] {
			t.Errorf("ParseOwnership(%q) = %d,%d,%v — want %d,%d,true", in, uid, gid, good, want[0], want[1])
		}
	}

	// Anything unclear is "not set", which falls back to detection — the
	// behaviour the operator had before they typed anything. A half-parsed pair
	// must never become an owner: chowning a restore to a wrong id is not
	// something anybody notices quickly.
	bad := []string{
		"", "1000", "1000:", ":1000", "abc:1000", "1000:abc",
		"-1:0", "0:-1", // negative ids are not ids
		"1000:1000:1000",
		"root:root",     // names resolve against a passwd file that differs per image
		"99999999:1000", // beyond the sane bound; a typo far more often than an account
		"1000 1000",     // space is not the separator
	}
	for _, in := range bad {
		if _, _, good := ParseOwnership(in); good {
			t.Errorf("ParseOwnership(%q) should not parse", in)
		}
	}
}

// The stored round trip, per container — which is what makes a stack restore
// work: each service goes through its own restore and reads its own pin. A
// database running as 999 and the application in front of it running as 1000
// need different answers, and one setting for a whole stack would be wrong for
// at least one of them.
func TestRestoreOwnershipSetting(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := &Engine{Store: st, Log: func(string, string, string) {}}

	if _, _, ok := e.RestoreOwnership("n1", "paperless"); ok {
		t.Fatal("nothing is pinned by default — detection decides")
	}
	if e.RestoreOwnershipSpec("n1", "paperless") != "" {
		t.Error("an unset pin must render as empty, not as 0:0")
	}

	if err := e.SetRestoreOwnership("n1", "paperless", "1000:1000"); err != nil {
		t.Fatal(err)
	}
	uid, gid, ok := e.RestoreOwnership("n1", "paperless")
	if !ok || uid != 1000 || gid != 1000 {
		t.Fatalf("got %d:%d ok=%v", uid, gid, ok)
	}
	if got := e.RestoreOwnershipSpec("n1", "paperless"); got != "1000:1000" {
		t.Errorf("spec = %q", got)
	}

	// Per container: setting one must not touch its neighbour in the same stack.
	if _, _, ok := e.RestoreOwnership("n1", "paperless-db"); ok {
		t.Error("a pin must not leak to another service")
	}
	// Per node, too.
	if _, _, ok := e.RestoreOwnership("n2", "paperless"); ok {
		t.Error("a pin must not leak to another node")
	}

	// Clearing goes back to detection rather than storing a zero.
	if err := e.SetRestoreOwnership("n1", "paperless", "  "); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := e.RestoreOwnership("n1", "paperless"); ok {
		t.Error("an empty value must clear the pin")
	}

	// A bad value is refused at the setter, so a typo cannot be stored and then
	// discovered at restore time.
	if err := e.SetRestoreOwnership("n1", "paperless", "root:root"); err == nil {
		t.Error("a non-numeric pair must be refused")
	}
	if _, _, ok := e.RestoreOwnership("n1", "paperless"); ok {
		t.Error("a refused value must not have been stored")
	}
}

// F189 — the pin has to change the CONTAINER as well as the files.
//
// Reported from a Synology-to-Linux move: the reconstructed compose on the new
// host still carried USERMAP_UID "1026" / USERMAP_GID "100" from the NAS.
//
// That exposes a hole in F184 as it first shipped. Chowning the restored data to
// the pinned ids while leaving the container declaring the source machine's is
// WORSE than doing nothing: the application drops to the old uid and then cannot
// write the files that were just handed to the new one. Both halves, or neither.
func TestApplyRunAsIDsRewritesTheDeclaredPair(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := &Engine{Store: st, Log: func(string, string, string) {}}
	b := &store.Backup{ID: "b1", TargetName: "PaperlessNGX"}
	opts := RestoreOptions{NodeID: "n1"}

	// The container as captured on the NAS.
	inspect := func(env ...string) []byte {
		insp := types.ContainerJSON{
			ContainerJSONBase: &types.ContainerJSONBase{Name: "/PaperlessNGX"},
			Config:            &container.Config{Env: env},
		}
		raw, _ := json.Marshal(insp)
		return raw
	}
	nas := inspect("USERMAP_UID=1026", "USERMAP_GID=100", "PAPERLESS_TIME_ZONE=Europe/London")

	// No pin: nothing is touched. A move that does not change the numbers must
	// not have its configuration rewritten underneath it.
	if got := string(e.applyRunAsIDs(b, nil, nas, opts)); !strings.Contains(got, "USERMAP_UID=1026") {
		t.Error("without a pin the captured ids must survive verbatim")
	}

	if err := e.SetRestoreOwnership("n1", "PaperlessNGX", "1000:1000"); err != nil {
		t.Fatal(err)
	}
	out := e.applyRunAsIDs(b, nil, nas, opts)
	env := dockercli.ContainerEnv(out)
	joined := strings.Join(env, " ")
	if !strings.Contains(joined, "USERMAP_UID=1000") || !strings.Contains(joined, "USERMAP_GID=1000") {
		t.Errorf("the declared pair must be rewritten to the pin: %v", env)
	}
	if strings.Contains(joined, "1026") || strings.Contains(joined, "USERMAP_GID=100 ") {
		t.Errorf("the source machine's ids must not survive: %v", env)
	}
	// Everything else is left exactly as it was.
	if !strings.Contains(joined, "PAPERLESS_TIME_ZONE=Europe/London") {
		t.Errorf("unrelated variables must not be disturbed: %v", env)
	}

	// An image that declares NO pair keeps its baked-in user: adding PUID to
	// something that never heard of it changes nothing and reads later like a
	// setting that should be doing something. Only the files are aligned there.
	plain := inspect("TZ=Europe/London")
	if got := dockercli.ContainerEnv(e.applyRunAsIDs(b, nil, plain, opts)); len(got) != 1 {
		t.Errorf("no pair declared means nothing to rewrite, got %v", got)
	}

	// The pair still comes from one convention.
	mixed := inspect("PUID=1026", "PGID=100")
	after := strings.Join(dockercli.ContainerEnv(e.applyRunAsIDs(b, nil, mixed, opts)), " ")
	if !strings.Contains(after, "PUID=1000") || !strings.Contains(after, "PGID=1000") {
		t.Errorf("PUID/PGID is a declared pair too: %s", after)
	}
}
