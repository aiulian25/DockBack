package backup

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

func TestDrillsSkipWhatTheyCannotTest(t *testing.T) {
	e := writeOnlyEngine(t)
	data := []VolumeRef{{Destination: "/data"}}
	cases := []struct {
		name string
		man  Manifest
		want string
	}{
		{"a native export only", Manifest{AppExport: &AppExportRef{Tool: "paperless"}}, "app-native paperless export"},
		{"configuration only", Manifest{}, "nothing to restore"},
		{"volumes", Manifest{Volumes: data}, ""},
		{"a database", Manifest{Databases: []DBDump{{Service: "db"}}}, ""},
	}
	for _, c := range cases {
		mj, _ := json.Marshal(&c.man)
		got := e.DrillSkipReason(&store.Backup{ID: "b", ManifestJSON: string(mj)})
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: %q", c.name, got)
		}
	}
}

func TestADrillBootsTheRecordedImage(t *testing.T) {
	if got := drillImageRef(&Manifest{Image: "app:latest", ImageDigest: "app@sha256:abc"}); got != "app@sha256:abc" {
		t.Errorf("by digest when recorded: %q", got)
	}
	if got := drillImageRef(&Manifest{Image: "app:latest"}); got != "app:latest" {
		t.Errorf("by tag otherwise: %q", got)
	}
}

// deltaChain is a full and a delta over it: b changed, a deleted, c added.
func deltaChain(t *testing.T) (*Engine, *store.Backup, *store.Backup) {
	t.Helper()
	e := synthEngine(t)
	full := mkChainGen(t, e, "fullgen00001", "", "", 0,
		map[string][]byte{"a.txt": []byte("A1"), "b.txt": []byte("B1")}, nil, idxOf(fe("a.txt", 2, 100), fe("b.txt", 2, 100)), nil)
	delta := mkChainGen(t, e, "deltagen0001", full.ID, full.CipherSHA256, 1,
		map[string][]byte{"b.txt": []byte("B2"), "c.txt": []byte("C1")}, nil, idxOf(fe("b.txt", 2, 200), fe("c.txt", 2, 200)), []string{"a.txt"})
	return e, full, delta
}

func TestAChainIsMaterialisedToTheDeltasState(t *testing.T) {
	e, _, delta := deltaChain(t)
	man := &Manifest{}
	if err := unmarshal(delta.ManifestJSON, man); err != nil {
		t.Fatal(err)
	}
	merged, err := e.materializeChainVolumes(context.Background(), delta, man)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(merged)
	files, _ := readTar(t, merged)
	if fmt.Sprint(files) != fmt.Sprint(map[string]string{"b.txt": "B2", "c.txt": "C1"}) {
		t.Fatalf("materialised %v", files)
	}
}

func TestAMissingAncestorIsNamed(t *testing.T) {
	e, full, delta := deltaChain(t)
	if err := e.Store.DeleteBackup(full.ID); err != nil {
		t.Fatal(err)
	}
	man := &Manifest{}
	if err := unmarshal(delta.ManifestJSON, man); err != nil {
		t.Fatal(err)
	}
	if _, err := e.materializeChainVolumes(context.Background(), delta, man); err == nil || !strings.Contains(err.Error(), short(full.ID)) {
		t.Fatalf("the missing parent must be named: %v", err)
	}
}

// payloadSHA is what the capture records for a backup's volume payload member.
func payloadSHA(t *testing.T, e *Engine, b *store.Backup, member string) string {
	t.Helper()
	h := sha256.New()
	err := e.streamArchive(context.Background(), b, "", func(tr *tar.Reader, hdr *tar.Header) (bool, error) {
		if hdr.Name != member {
			return true, nil
		}
		_, cerr := io.Copy(h, tr)
		return false, cerr
	})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// The finding: a healthy delta read as corrupt, or as passed with nothing tested.
// A registry with no node stops the drill at the point it would start a container,
// so reaching it means the archive checks and the chain were sound.
func TestAHealthyDeltaReachesTheTestRestore(t *testing.T) {
	e, _, delta := deltaChain(t)
	e.Reg = dockercli.NewRegistry()
	man := &Manifest{}
	if err := unmarshal(delta.ManifestJSON, man); err != nil {
		t.Fatal(err)
	}
	man.VolumesSHA256 = payloadSHA(t, e, delta, volumeDeltaMember)
	mb, _ := json.Marshal(man)
	delta.ManifestJSON = string(mb)

	ok, summary := e.DrillBackup(context.Background(), delta)
	if ok || !strings.Contains(summary, "node unavailable for test-restore") {
		t.Fatalf("a healthy delta must get as far as restoring: %v %q", ok, summary)
	}
}
