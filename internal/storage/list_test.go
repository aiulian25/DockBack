package storage

import (
	"bytes"
	"context"
	"sort"
	"testing"
)

func TestLocalListAndHelper(t *testing.T) {
	b, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, k := range []string{"dockback-config/a.dback", "dockback-config/b.dback", "other/c.txt"} {
		if _, err := b.Put(ctx, k, bytes.NewReader([]byte("x"))); err != nil {
			t.Fatalf("put %s: %v", k, err)
		}
	}

	// List via the optional-interface helper.
	keys, err := ListKeys(ctx, b, "dockback-config")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	sort.Strings(keys)
	want := []string{"dockback-config/a.dback", "dockback-config/b.dback"}
	if len(keys) != 2 || keys[0] != want[0] || keys[1] != want[1] {
		t.Fatalf("got %v want %v", keys, want)
	}

	// Returned keys must round-trip through Get.
	rc, err := b.Get(ctx, keys[0])
	if err != nil {
		t.Fatalf("get listed key: %v", err)
	}
	rc.Close()

	// Missing prefix → empty, not error.
	empty, err := ListKeys(ctx, b, "nope")
	if err != nil || len(empty) != 0 {
		t.Fatalf("missing prefix: %v len=%d", err, len(empty))
	}
}

// TestLocalListRecursive locks in that ListKeys enumerates DEEPLY nested keys
// (the <node>/<stack>/<container>/… archive layout), which F20 adopt relies on.
func TestLocalListRecursive(t *testing.T) {
	b, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	nested := []string{
		"node1/web/nginx/2026-01-01_00-00-00_abc.dback",
		"node1/web/nginx/2026-01-01_00-00-00_abc.dback.manifest.json",
		"node1/db/pg/2026-01-02_00-00-00_xyz.dback",
		"top.dback",
	}
	for _, k := range nested {
		if _, err := b.Put(ctx, k, bytes.NewReader([]byte("x"))); err != nil {
			t.Fatalf("put %s: %v", k, err)
		}
	}
	keys, err := ListKeys(ctx, b, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	sort.Strings(keys)
	want := make([]string, len(nested))
	copy(want, nested)
	sort.Strings(want)
	if len(keys) != len(want) {
		t.Fatalf("got %v (%d), want all %d nested keys", keys, len(keys), len(want))
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("got %v, want %v", keys, want)
		}
	}
}
