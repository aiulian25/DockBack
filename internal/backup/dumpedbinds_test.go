package backup

import (
	"slices"
	"testing"
)

// A failure from the 2026-10-04 recovery: an app that bundles PostgreSQL,
// with its data directory bind-mounted on its own. The backup dumped the
// database and left the raw directory out of the archive — but the file index
// still listed it. After the files were restored the directory was empty by
// design, the check read that as "data that did not arrive", and refused to
// start the container. The dump was never imported, and the live database had
// already been cleared.
func TestADumpedDataDirectoryIsMeantToBeEmpty(t *testing.T) {
	dbFiles := index(
		"config/postgres/PG_VERSION",
		"config/postgres/base/1/1259",
		"config/postgres/global/pg_control",
	)
	bundled := func(databases []DBDump) *Manifest {
		return &Manifest{
			Image: "flcontainers/guacamole:latest", // a public profile with a bundled postgres at /config/postgres
			Volumes: []VolumeRef{
				{Type: "bind", Destination: "/config/postgres", Source: "/srv/guac/pg"},
			},
			Databases: databases,
		}
	}

	t.Run("older backup: the profile and the recorded dump say it was excluded", func(t *testing.T) {
		got := expectedNonEmptyDestinations(bundled([]DBDump{{Engine: "postgres", Path: "db/guacamole.sql"}}), dbFiles)
		if len(got) != 0 {
			t.Errorf("a dumped data directory must not be expected to hold files after the file restore, got %v", got)
		}
	})

	t.Run("a failed dump means the directory WAS archived, so it must arrive", func(t *testing.T) {
		got := expectedNonEmptyDestinations(bundled(nil), dbFiles)
		if !slices.Equal(got, []string{"/config/postgres"}) {
			t.Errorf("with no dump the raw copy is the only copy and must be checked, got %v", got)
		}
	})

	t.Run("newer backup: the manifest records the exclusion itself", func(t *testing.T) {
		man := &Manifest{
			Image:           "some/app-with-its-own-postgres:1",
			Volumes:         []VolumeRef{{Type: "bind", Destination: "/var/lib/postgresql/data"}},
			ArchiveExcluded: []string{"/var/lib/postgresql/data"},
		}
		got := expectedNonEmptyDestinations(man, index("var/lib/postgresql/data/PG_VERSION"))
		if len(got) != 0 {
			t.Errorf("a recorded exclusion must be honoured whatever the image, got %v", got)
		}
	})

	// The check still has to protect real data: every other bind is unchanged.
	t.Run("other binds are still checked", func(t *testing.T) {
		man := &Manifest{
			Image: "some/app:1",
			Volumes: []VolumeRef{
				{Type: "bind", Destination: "/var/lib/postgresql/data"},
				{Type: "bind", Destination: "/app/uploads"},
			},
			ArchiveExcluded: []string{"/var/lib/postgresql/data"},
		}
		got := expectedNonEmptyDestinations(man, index("var/lib/postgresql/data/PG_VERSION", "app/uploads/photo.jpg"))
		if !slices.Equal(got, []string{"/app/uploads"}) {
			t.Errorf("only the excluded directory may be skipped, got %v", got)
		}
	})
}

func TestArchiveExclusionsMatchTheWayTheCaptureAppliedThem(t *testing.T) {
	excluded := []string{"/config/postgres", "/app/data/mealie.log*", "/cache/*/thumbs"}
	for path, want := range map[string]bool{
		"config/postgres":                true,  // the directory itself
		"config/postgres/base/1/1259":    true,  // everything beneath it
		"config/postgresql.conf":         false, // a sibling with a shared prefix is NOT beneath it
		"app/data/mealie.log":            true,  // a file glob
		"app/data/mealie.log.3":          true,
		"app/data/mealie.db":             false,
		"cache/user1/thumbs/a.jpg":       true, // a glob matching a directory covers what is under it
		"cache/user1/originals/a.jpg":    false,
		"elsewhere/config/postgres/x.db": false,
	} {
		if got := excludedByAny(path, excluded); got != want {
			t.Errorf("%s: excluded = %v, want %v", path, got, want)
		}
	}
}
