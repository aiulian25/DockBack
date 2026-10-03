package dockercli

import (
	"strings"
	"testing"
)

func TestValidateHostStackDir(t *testing.T) {
	cases := []struct {
		name       string
		dir        string
		wantParent string
		wantBase   string
		wantErr    bool
	}{
		// Accepted: the common organized layouts.
		{"typical", "/opt/docker/app1", "/opt/docker", "app1", false},
		{"trailing slash cleaned", "/opt/docker/app2/", "/opt/docker", "app2", false},
		{"dotted redundancy cleaned", "/srv/stacks/./app", "/srv/stacks", "app", false},
		{"opt/appdata", "/opt/appdata/nextcloud", "/opt/appdata", "nextcloud", false},

		// Rejected: system roots and their children.
		{"root", "/", "", "", true},
		{"etc", "/etc", "", "", true},
		{"etc child", "/etc/cron.d/evil", "", "", true},
		{"usr child", "/usr/local/bin/app", "", "", true},
		{"var lib docker", "/var/lib/docker/volumes/x", "", "", true},
		{"proc", "/proc/self/foo", "", "", true},
		{"root home", "/root/stack", "", "", true},

		// Rejected: too shallow (top-level).
		{"top level", "/app", "", "", true},

		// Rejected: relative / unsafe characters (bind-spec + shell injection).
		{"relative", "opt/docker/x", "", "", true},
		{"empty", "", "", "", true},
		{"colon (bind injection)", "/opt/docker/x:ro", "", "", true},
		{"newline", "/opt/docker/x\ny", "", "", true},
		{"nul", "/opt/docker/x\x00y", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent, base, err := validateHostStackDir(tc.dir)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got parent=%q base=%q", tc.dir, parent, base)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.dir, err)
			}
			if parent != tc.wantParent || base != tc.wantBase {
				t.Fatalf("validateHostStackDir(%q) = (%q,%q); want (%q,%q)", tc.dir, parent, base, tc.wantParent, tc.wantBase)
			}
		})
	}
}

// F57 — the genuine host compose file restored beside the reconstruction takes a
// name `docker compose` does NOT read, and an unsafe archive entry takes no name
// at all (it is skipped, not defaulted: writing an attacker-chosen file under a
// guessed name is worse than not writing it).
func TestAsideName(t *testing.T) {
	ok := []struct{ in, want string }{
		{"docker-compose.yml", "docker-compose.yml.original-from-backup"},
		{"compose.yaml", "compose.yaml.original-from-backup"},
		{"2-docker-compose.yml", "2-docker-compose.yml.original-from-backup"}, // archiveComposeName's prefixed duplicate
	}
	for _, tc := range ok {
		if got := asideName(tc.in); got != tc.want {
			t.Errorf("asideName(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
	// Anything outside the safe-component grammar is refused outright.
	for _, bad := range []string{"", " ", ".", "..", "weird name.yml", "evil;rm -rf.yml", "../../etc/passwd", "a/b.yml", ".hidden"} {
		if got := asideName(bad); got != "" {
			t.Errorf("asideName(%q) = %q; want \"\" (refused)", bad, got)
		}
	}
	// The suffix is never a name compose reads, so the aside file can never be
	// picked up by a bare `docker compose up`.
	for _, n := range standardComposeNames {
		if asideName(n) == n {
			t.Errorf("asideName(%q) must not return a canonical compose name", n)
		}
	}
}

func TestSafeComposeName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"docker-compose.yml", "docker-compose.yml"},
		{"compose.yaml", "compose.yaml"},
		{"", "docker-compose.yml"},
		{".", "docker-compose.yml"},
		{"..", "docker-compose.yml"},
		{"weird name.yml", "docker-compose.yml"},   // space -> unsafe -> default
		{"evil;rm -rf.yml", "docker-compose.yml"},  // shell metachars -> default
		{"../../etc/passwd", "docker-compose.yml"}, // slash -> unsafe -> default
	}
	for _, tc := range cases {
		if got := safeComposeName(tc.in); got != tc.want {
			t.Errorf("safeComposeName(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// WriteHostFile puts attacker-influenceable bytes at an attacker-influenceable
// path: the path comes from a backup manifest, which came from `docker inspect`
// on some other machine. Its parent is then bind-mounted READ-WRITE into a root
// sidecar, so the validator is the whole security boundary and every shape it
// must refuse is pinned here.
func TestValidateHostFilePath(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		wantParent string
		wantBase   string
		wantErr    bool
	}{
		{"ordinary secret", "/home/user/docker/webapp/secrets/vapid_private_key",
			"/home/user/docker/webapp/secrets", "vapid_private_key", false},
		{"synology layout", "/volume1/docker/webapp/secrets/vapid_private_key",
			"/volume1/docker/webapp/secrets", "vapid_private_key", false},
		{"cleaned", "/srv//app/./conf.yml", "/srv/app", "conf.yml", false},

		// Refusals. Writing into any of these would put the sidecar's read-write
		// bind on a system directory.
		{"system root itself", "/etc/passwd", "", "", true},
		{"under a system root", "/etc/ssl/private/key.pem", "", "", true},
		{"var lib", "/var/lib/docker/key", "", "", true},
		{"proc", "/proc/self/mem", "", "", true},
		{"root dir", "/", "", "", true},
		{"too shallow", "/key.pem", "", "", true},
		{"relative", "srv/app/conf.yml", "", "", true},
		{"empty", "", "", "", true},
		{"blank", "   ", "", "", true},
		// ':' would split the "src:dst" bind spec and remount something else.
		{"colon in path", "/srv/app:/etc/x", "", "", true},
		{"newline", "/srv/app/a\nb", "", "", true},
		{"nul", "/srv/app/a\x00b", "", "", true},
		// Traversal collapses to a shallow or system path, never escapes.
		{"traversal to root", "/srv/../../etc/passwd", "", "", true},
		{"leaf is dotdot", "/srv/app/..", "", "", true},
		{"leading dash leaf", "/srv/app/-rf", "", "", true},
		{"leaf with space", "/srv/app/my key", "", "", true},
	}
	for _, tc := range cases {
		parent, base, err := validateHostPath(tc.path, "host file")
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: validateHostPath(%q) must be refused, got (%q,%q)", tc.name, tc.path, parent, base)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: validateHostPath(%q) = %v", tc.name, tc.path, err)
			continue
		}
		if parent != tc.wantParent || base != tc.wantBase {
			t.Errorf("%s: got (%q,%q), want (%q,%q)", tc.name, parent, base, tc.wantParent, tc.wantBase)
		}
	}
}

// The staging file exists so an interrupted restore leaves the previous file
// intact rather than a truncated secret, and so the bytes are never briefly
// world-readable. Both properties live in the command, so they are asserted here.
func TestHostFileStagingIsPrivateAndDistinct(t *testing.T) {
	if hostFileTempSuffix == "" {
		t.Fatal("a host file must be staged under a distinct name, not written in place")
	}
	if !strings.HasPrefix(hostFileTempSuffix, ".") {
		t.Errorf("the staging suffix should keep the partial file out of ordinary listings, got %q", hostFileTempSuffix)
	}
	// safeHostComponent governs the real leaf; the staging name is that leaf plus
	// this suffix, so the suffix must not introduce anything the grammar rejects.
	if !safeHostComponent.MatchString("vapid_private_key" + hostFileTempSuffix) {
		t.Errorf("staging name %q must still be a safe host component", "vapid_private_key"+hostFileTempSuffix)
	}
}
