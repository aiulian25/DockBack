package backup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dockback/internal/dockercli"
	"dockback/internal/storage"
	"dockback/internal/store"
)

// A copy creates a container; it never takes over one. Whatever already holds the
// name belongs to somebody else, so neither a copy nor a standby rehearsal may
// stop, write to or remove it, however the request reached the engine.

const unrelatedContainerID = "unrelated-container-id"

// recordingDocker is the part of a Docker daemon these refusals can reach: one
// unrelated container, and a record of everything that was not a read.
type recordingDocker struct {
	mu        sync.Mutex
	mutations []string
}

func (d *recordingDocker) recorded() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.mutations...)
}

func serveRecordingDocker(t *testing.T, containerName string) (*dockercli.Registry, *recordingDocker) {
	t.Helper()
	docker := &recordingDocker{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("API-Version", "1.44")
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			docker.mu.Lock()
			docker.mutations = append(docker.mutations, r.Method+" "+r.URL.Path)
			docker.mu.Unlock()
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{"Id": unrelatedContainerID, "Names": []string{"/" + containerName}}})
		case strings.Contains(r.URL.Path, "/containers/"+containerName+"/json"), strings.Contains(r.URL.Path, "/containers/"+unrelatedContainerID+"/json"):
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": unrelatedContainerID, "Name": "/" + containerName, "State": map[string]any{"Running": true}})
		case r.URL.Path == "/_ping":
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	reg := dockercli.NewRegistry()
	reg.Set(dockercli.NodeConn{ID: "n1", Transport: dockercli.TransportTCPProxy, Address: "tcp://" + srv.Listener.Addr().String()})
	return reg, docker
}

func cloneEngine(t *testing.T, reg *dockercli.Registry) (*Engine, *store.Backup) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	man, err := json.Marshal(Manifest{BackupID: "b1", TargetName: "app", ContainerID: "cid-app", VolIndex: "index"})
	if err != nil {
		t.Fatal(err)
	}
	b := &store.Backup{ID: "b1", NodeID: "n1", TargetName: "app", Status: "success", Verified: "verified",
		CreatedAt: time.Now().Unix(), ManifestJSON: string(man), StorageKey: "k/b1"}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
	empty, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{Store: st, Reg: reg, Storage: empty, Log: func(string, string, string) {}}, b
}

func TestRestoreAsACopyRefusesANameInUse(t *testing.T) {
	for _, recreate := range []bool{false, true} {
		reg, docker := serveRecordingDocker(t, "taken")
		e, b := cloneEngine(t, reg)

		err := e.Restore(context.Background(), RestoreOptions{
			BackupID: b.ID, NodeID: "n1", AsName: "taken", Isolated: true,
			Volumes: true, Database: true, Recreate: recreate, Snapshot: true,
		})

		if err == nil || !strings.Contains(err.Error(), "taken") {
			t.Errorf("recreate=%v: a name in use must be refused and named, got %v", recreate, err)
		}
		if got := docker.recorded(); len(got) != 0 {
			t.Errorf("recreate=%v: the container holding that name must not be touched: %v", recreate, got)
		}
	}
}

// With isolation off the copy would keep the original's mounts and write the
// backup straight into its live data, under a different container name.
func TestRestoreAsACopyMustBeIsolated(t *testing.T) {
	reg, docker := serveRecordingDocker(t, "unrelated")
	e, b := cloneEngine(t, reg)

	err := e.Restore(context.Background(), RestoreOptions{BackupID: b.ID, NodeID: "n1", AsName: "free-name", Volumes: true})

	if err == nil || !strings.Contains(err.Error(), "isolated") {
		t.Errorf("a copy that is not isolated must be refused, got %v", err)
	}
	if got := docker.recorded(); len(got) != 0 {
		t.Errorf("nothing may happen on the daemon: %v", got)
	}
}

// The rehearsal used a fixed name and removed whatever held it afterwards, even
// when its own create never happened. An operator's container called
// <target>-standby was deleted by a rehearsal that failed to restore anything.
func TestStandbyRehearsalRemovesOnlyWhatItCreated(t *testing.T) {
	reg, docker := serveRecordingDocker(t, StandbyCloneName("app"))
	e, b := cloneEngine(t, reg)

	ok, _, detail := e.RehearseStandby(context.Background(), b, "n1")

	if ok {
		t.Fatalf("this restore cannot succeed: there is no archive to read (%s)", detail)
	}
	if got := docker.recorded(); len(got) != 0 {
		t.Errorf("the rehearsal created nothing, so it must remove nothing: %v", got)
	}
}

func TestEachStandbyRehearsalGetsItsOwnName(t *testing.T) {
	first, second := standbyRunName("app"), standbyRunName("app")
	if first == second {
		t.Errorf("two rehearsals must not share a name: %q", first)
	}
	if !strings.HasPrefix(first, StandbyCloneName("app")+"-") {
		t.Errorf("the name should still say what it is: %q", first)
	}
}
