package backup

import (
	"slices"
	"strings"
	"testing"

	"dockback/internal/dockercli"
)

// index builds a VolIndex from relative paths, the shape ParseIndexOutput
// produces (leading slash stripped, same convention as tar members).
func index(paths ...string) VolIndex {
	idx := VolIndex{}
	for _, p := range paths {
		idx.Entries = append(idx.Entries, FileEntry{Path: p, Size: 1})
	}
	return idx
}

func TestExpectedNonEmptyDestinations(t *testing.T) {
	for _, tc := range []struct {
		name string
		man  *Manifest
		idx  VolIndex
		want []string
	}{
		{
			name: "a bind the backup recorded ten files in is expected to hold data",
			man: &Manifest{Volumes: []VolumeRef{
				{Destination: "/data", Type: "bind", Source: "/srv/app/data"},
			}},
			idx:  index("data/f1", "data/f2", "data/sub/f3"),
			want: []string{"/data"},
		},
		{
			// The case that must never block: an application's geoip directory is
			// empty on the source until first run. Reproducing empty is correct.
			name: "a bind that was empty at capture is not expected to hold data",
			man: &Manifest{Volumes: []VolumeRef{
				{Destination: "/data", Type: "bind", Source: "/srv/app/data"},
				{Destination: "/geoip", Type: "bind", Source: "/srv/app/geoip"},
			}},
			idx:  index("data/f1"),
			want: []string{"/data"},
		},
		{
			// The untar and F226 creation govern named volumes, and one that was
			// empty at capture is legitimately empty now.
			name: "named volumes are not checked here",
			man: &Manifest{Volumes: []VolumeRef{
				{Destination: "/var/lib/mysql", Type: "volume", Name: "dbdata"},
			}},
			idx:  index("var/lib/mysql/ibdata1"),
			want: nil,
		},
		{
			// A file bind's destination IS a file; asking whether it contains
			// anything would report every one of them empty and refuse every
			// restore that carries a mounted secret.
			name: "file-rooted binds are excluded",
			man: &Manifest{Volumes: []VolumeRef{
				{Destination: "/run/secrets/key", Type: "bind", Source: "/srv/secrets/key",
					Kind: dockercli.MountKindFile, Archive: "bind-files/0.bin"},
			}},
			idx:  index("run/secrets/key"),
			want: nil,
		},
		{
			// A plain string prefix would let /data2's files vouch for /data, and
			// an empty /data would then start the application.
			name: "a sibling sharing a name prefix does not vouch for it",
			man: &Manifest{Volumes: []VolumeRef{
				{Destination: "/data", Type: "bind", Source: "/srv/data"},
				{Destination: "/data2", Type: "bind", Source: "/srv/data2"},
			}},
			idx:  index("data2/f1"),
			want: []string{"/data2"},
		},
		{
			// Step 04 folds a nested child into its parent's archive member, but
			// its files still sit under its own path in the index, so both mounts
			// are still expected to hold data.
			name: "nested mounts are each expected to hold their own data",
			man: &Manifest{Volumes: []VolumeRef{
				{Destination: "/var/www/html", Type: "bind", Source: "/srv/html"},
				{Destination: "/var/www/html/data", Type: "bind", Source: "/srv/data", NestedIn: "/var/www/html"},
			}},
			idx:  index("var/www/html/index.php", "var/www/html/data/user/photo.jpg"),
			want: []string{"/var/www/html", "/var/www/html/data"},
		},
		{
			// No index means no record of what was there. Inventing an expectation
			// would refuse restores on a guess.
			name: "an archive with no index yields no expectation",
			man:  &Manifest{Volumes: []VolumeRef{{Destination: "/data", Type: "bind", Source: "/srv/data"}}},
			idx:  VolIndex{},
			want: nil,
		},
		{
			name: "a mount at the root is not reasoned about this way",
			man:  &Manifest{Volumes: []VolumeRef{{Destination: "/", Type: "bind", Source: "/srv"}}},
			idx:  index("etc/passwd"),
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := expectedNonEmptyDestinations(tc.man, tc.idx)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for _, want := range tc.want {
				if !slices.Contains(got, want) {
					t.Errorf("%s must be expected to hold data, got %v", want, got)
				}
			}
		})
	}
}

func TestEmptyProbeRoundTrip(t *testing.T) {
	script := emptyProbeScript([]string{"/data", "/var/www/html"})
	for _, want := range []string{"'/data'", "'/var/www/html'", "-mindepth 1 -maxdepth 1"} {
		if !strings.Contains(script, want) {
			t.Errorf("script must contain %q: %s", want, script)
		}
	}

	states := parseEmptyProbe("FILLED|/var/www/html\nEMPTY|/data\nMISSING|/geoip\n")
	for dest, want := range map[string]string{"/var/www/html": bindFilled, "/data": bindEmpty, "/geoip": bindMissing} {
		if states[dest] != want {
			t.Errorf("%s = %q, want %q", dest, states[dest], want)
		}
	}
	// A destination the probe said nothing about is unknown, not empty — a
	// truncated report must not refuse a restore that was fine.
	if _, reported := states["/never-mentioned"]; reported {
		t.Error("an unreported destination must be absent from the map")
	}
	if got := parseEmptyProbe("garbage\n\nWAT|/x\n"); len(got) != 0 {
		t.Errorf("unparseable output must yield nothing, got %v", got)
	}
}
