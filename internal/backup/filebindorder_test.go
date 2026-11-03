package backup

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/docker/docker/client"
	"github.com/klauspost/compress/zstd"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

const (
	fileBindDestination = "/run/secrets/app.key"
	recordedKeyPath     = "/home/tester/old-stack/app.key"
	targetContainerID   = "target-container"
)

// fileBindBackup is a stored backup whose only content is one captured file bind,
// recorded at recordedKeyPath.
func fileBindBackup(t *testing.T) (*Engine, *store.Backup, *Manifest) {
	t.Helper()
	e := synthEngine(t)
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, bindFileWorkDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, bindFileWorkDir, "0"), []byte("key-from-backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	man := &Manifest{
		Version: ManifestVersion, BackupID: "filebind0001", TargetName: "app",
		KeyFingerprint: e.KeyFP, Format: Format{Algorithm: "zstd", Archive: "tar"},
		Volumes: []VolumeRef{{Type: "bind", Kind: dockercli.MountKindFile, Source: recordedKeyPath,
			Destination: fileBindDestination, Archive: bindFileArchivePrefix + "0"}},
	}
	storageKey := "node/app/filebind0001.dback"
	cipherSHA, _, err := e.packEncryptStore(context.Background(), work, man, storageKey, "zstd", zstd.SpeedDefault, 0)
	if err != nil {
		t.Fatal(err)
	}
	b := &store.Backup{ID: man.BackupID, NodeID: "n1", Status: "success", TargetName: "app", CreatedAt: 1_000_000}
	if err := e.Store.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	b.CipherSHA256, b.StorageKey = cipherSHA, storageKey
	mb, _ := json.Marshal(man)
	b.ManifestJSON = string(mb)
	b.LocationsJSON = mustJSON([]Location{{Kind: "local", Name: "local", Type: "local"}})
	if err := e.Store.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
	return e, b, man
}

// fakeDaemon answers as a node whose target container is running. It records
// every call, and the bodies of container creates, which is where a host-file
// write starts: the sidecar that binds the file's folder.
type fakeDaemon struct {
	stopStatus   int
	calls        []string
	createBodies []string
}

func (d *fakeDaemon) client(t *testing.T) *client.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("API-Version", "1.44")
		w.Header().Set("Content-Type", "application/json")
		d.calls = append(d.calls, r.Method+" "+r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/"+targetContainerID+"/json"):
			_, _ = w.Write([]byte(`{"Id":"` + targetContainerID + `","State":{"Running":true}}`))
		case strings.HasSuffix(r.URL.Path, "/containers/"+targetContainerID+"/stop"):
			w.WriteHeader(d.stopStatus)
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			body, _ := io.ReadAll(r.Body)
			d.createBodies = append(d.createBodies, string(body))
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"the fake daemon creates nothing"}`))
		case strings.Contains(r.URL.Path, "/images/"):
			_, _ = w.Write([]byte(`{"Id":"sha256:sidecar"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	c, err := client.NewClientWithOpts(client.WithHost("tcp://"+srv.Listener.Addr().String()), client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func liveConfigMounting(t *testing.T, source string) []byte {
	t.Helper()
	config, err := json.Marshal(map[string]any{
		"Id":         targetContainerID,
		"HostConfig": map[string]any{"Binds": []string{source + ":" + fileBindDestination + ":ro"}},
		"Mounts":     []map[string]string{{"Type": "bind", "Source": source, "Destination": fileBindDestination}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return config
}

// A stop the daemon refuses must leave every file as it was: the application is
// still running on them, and the restore is about to fail.
func TestARefusedStopWritesNothing(t *testing.T) {
	e, b, man := fileBindBackup(t)
	daemon := &fakeDaemon{stopStatus: http.StatusInternalServerError}
	opts := RestoreOptions{TargetID: targetContainerID}

	touched, err := e.putBackFileBinds(context.Background(), daemon.client(t), b, man, opts, liveConfigMounting(t, recordedKeyPath))

	if err == nil {
		t.Fatal("a refused stop must fail the restore")
	}
	if touched {
		t.Error("nothing was changed, so the failure needs no recovery")
	}
	if len(daemon.createBodies) != 0 {
		t.Errorf("a file bind was written although the stop was refused: %v", daemon.calls)
	}
}

// The file goes where the container reads it now. A deployment that moved the
// file must not have its old path written while the new one stays wrong, and a
// write that fails must fail the restore.
func TestAFileBindIsWrittenWhereTheContainerMountsIt(t *testing.T) {
	e, b, man := fileBindBackup(t)
	daemon := &fakeDaemon{stopStatus: http.StatusNoContent}
	opts := RestoreOptions{TargetID: targetContainerID}
	const movedKeyPath = "/home/tester/new-stack/secrets/app.key"

	touched, err := e.putBackFileBinds(context.Background(), daemon.client(t), b, man, opts, liveConfigMounting(t, movedKeyPath))

	stopAt := slices.IndexFunc(daemon.calls, func(call string) bool { return strings.HasSuffix(call, "/stop") })
	createAt := slices.IndexFunc(daemon.calls, func(call string) bool { return strings.HasSuffix(call, "/containers/create") })
	if stopAt < 0 || createAt < 0 || stopAt > createAt {
		t.Fatalf("the target must be stopped before its file is written: %v", daemon.calls)
	}
	if !strings.Contains(daemon.createBodies[0], filepath.Dir(movedKeyPath)+":") {
		t.Errorf("the file was not written to the path the container mounts: %s", daemon.createBodies[0])
	}
	if strings.Contains(strings.Join(daemon.createBodies, "\n"), filepath.Dir(recordedKeyPath)) {
		t.Error("the backup's old path was written")
	}
	if err == nil || !strings.Contains(err.Error(), movedKeyPath) {
		t.Errorf("a file bind that could not be written must fail the restore and be named, got %v", err)
	}
	if !touched {
		t.Error("the target was stopped, so the failure needs recovery")
	}
}

func TestAFileBindFollowsTheMountAtItsDestination(t *testing.T) {
	mounts := []dockercli.BindMount{
		{Source: "/home/tester/new-stack/secrets/app.key", Destination: fileBindDestination},
		{Source: "/home/tester/new-stack/data", Destination: "/data"},
	}
	if host, ok := bindFileTarget(mounts, fileBindDestination); !ok || host != mounts[0].Source {
		t.Errorf("want %s, got %q %v", mounts[0].Source, host, ok)
	}
	if _, ok := bindFileTarget(mounts, "/run/secrets/gone.key"); ok {
		t.Error("a destination nothing mounts any more has no target")
	}
}
