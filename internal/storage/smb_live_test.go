package storage

import (
	"context"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// smbLiveEnv opts into a round-trip against a real SMB server, for checking an
// SMB library change. Format: "host:port|share|username|password".
const smbLiveEnv = "DOCKBACK_SMB_LIVE"

// Every call DockBack makes over SMB, against a real server: authenticate, mount,
// write into a new folder, list, stat, read back and delete.
func TestSMBRoundTripAgainstARealServer(t *testing.T) {
	spec := os.Getenv(smbLiveEnv)
	if spec == "" {
		t.Skipf("set %s=host:port|share|username|password to run against a real SMB server", smbLiveEnv)
	}
	parts := strings.Split(spec, "|")
	if len(parts) != 4 {
		t.Fatalf("%s must be host:port|share|username|password", smbLiveEnv)
	}
	be, err := NewSMB(map[string]string{"host": parts[0], "share": parts[1], "username": parts[2], "password": parts[3], "path": "dockback-live"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	const key, body = "node1/web/archive.dback", "archive-bytes"

	if n, err := be.Put(ctx, key, strings.NewReader(body)); err != nil || n != int64(len(body)) {
		t.Fatalf("put: %d %v", n, err)
	}
	keys, err := be.List(ctx, "node1")
	if err != nil || !slices.Contains(keys, key) {
		t.Fatalf("list: %v %v", keys, err)
	}
	if size, ok, err := be.Stat(ctx, key); err != nil || !ok || size != int64(len(body)) {
		t.Fatalf("stat: %d %v %v", size, ok, err)
	}
	rc, err := be.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(got) != body {
		t.Fatalf("get: %q %v", got, err)
	}
	if err := be.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := be.Stat(ctx, key); err != nil || ok {
		t.Fatalf("after delete: %v %v", ok, err)
	}
}
