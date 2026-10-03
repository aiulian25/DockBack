package dockercli

import (
	"context"
	"strings"
	"testing"
)

// The rescue builds a shell command around two caller-supplied strings. Both are
// checked before anything runs, so the guards are what this asserts — the sidecar
// itself needs a live daemon and is covered by the end-to-end restore.
func TestStripAutoConfSettingRefusesUnsafeInput(t *testing.T) {
	for _, tc := range []struct{ dir, name, why string }{
		{"", "shared_preload_libraries", "empty data directory"},
		{"relative/path", "shared_preload_libraries", "not absolute"},
		{"/data; rm -rf /", "shared_preload_libraries", "shell syntax in the path"},
		{"/data$(id)", "shared_preload_libraries", "command substitution in the path"},
		{"/var/lib/postgresql/data", "", "empty setting name"},
		{"/var/lib/postgresql/data", "shared_preload_libraries; rm -rf /", "shell syntax in the name"},
		{"/var/lib/postgresql/data", ".*", "a regular expression that would strip every setting"},
	} {
		err := StripAutoConfSetting(context.Background(), nil, "target", tc.dir, tc.name)
		if err == nil {
			t.Errorf("accepted %s (dir=%q name=%q) — it must be refused before any sidecar runs", tc.why, tc.dir, tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "refusing") {
			t.Errorf("%s: refused with the wrong error: %v", tc.why, err)
		}
	}
}
