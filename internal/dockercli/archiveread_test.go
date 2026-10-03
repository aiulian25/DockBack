package dockercli

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
)

// R1 §3.1 measured a daemon that creates a sidecar and refuses to start it, so
// file capture died where `GET /containers/{id}/archive` would have worked.
// PLAYBOOK §4.3: the archive API "needs nothing inside the container, works on
// scratch images, reads through bind mounts, and preserves ownership/mode in tar
// headers."
//
// The layouts differ, and that difference is the entire risk. Verified against a
// live daemon before this was written:
//
//	sidecar (tar -cf - -C / var/lib/foo):  var/lib/foo/  var/lib/foo/top.txt
//	archive API (path=/var/lib/foo):       foo/          foo/top.txt
//
// Restores extract with `tar -xf - -C /`, so shipping the daemon's layout
// unchanged writes /foo instead of /var/lib/foo: every file present, every one
// in the wrong place.
func TestArchiveTarRewrite(t *testing.T) {
	// member is one entry as the daemon would emit it, with the ownership and
	// mode that must survive.
	type member struct {
		name     string
		body     string
		mode     int64
		uid, gid int
		typ      byte
		link     string
	}

	build := func(members []member) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, m := range members {
			typ := m.typ
			if typ == 0 {
				typ = tar.TypeReg
			}
			hdr := &tar.Header{
				Name: m.name, Mode: m.mode, Uid: m.uid, Gid: m.gid,
				Size: int64(len(m.body)), Typeflag: typ, Linkname: m.link,
			}
			if typ != tar.TypeReg {
				hdr.Size = 0
			}
			if err := tw.WriteHeader(hdr); err != nil {
				t.Fatal(err)
			}
			if typ == tar.TypeReg {
				if _, err := tw.Write([]byte(m.body)); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	rewrite := func(t *testing.T, in []byte, root string, excludes []string) []*tar.Header {
		t.Helper()
		var out bytes.Buffer
		tw := tar.NewWriter(&out)
		if err := RewriteArchiveMembers(bytes.NewReader(in), tw, root, excludes); err != nil {
			t.Fatalf("rewrite: %v", err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		var got []*tar.Header
		tr := tar.NewReader(&out)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			got = append(got, hdr)
		}
		return got
	}

	t.Run("the daemon's basename root becomes the sidecar's full path", func(t *testing.T) {
		// The exact fixture measured live, headers and all.
		in := build([]member{
			{name: "foo/", typ: tar.TypeDir, mode: 0o775, uid: 1000, gid: 1000},
			{name: "foo/top.txt", body: "hello\n", mode: 0o640, uid: 101, gid: 104},
			{name: "foo/sub/", typ: tar.TypeDir, mode: 0o775, uid: 1000, gid: 1000},
			{name: "foo/sub/deep.txt", body: "nested\n", mode: 0o664, uid: 1000, gid: 1000},
		})
		got := rewrite(t, in, "var/lib/foo", nil)
		want := []string{"var/lib/foo/", "var/lib/foo/top.txt", "var/lib/foo/sub/", "var/lib/foo/sub/deep.txt"}
		if len(got) != len(want) {
			t.Fatalf("got %d members, want %d", len(got), len(want))
		}
		for i, w := range want {
			if got[i].Name != w {
				t.Errorf("member %d = %q, want %q", i, got[i].Name, w)
			}
		}
		// PLAYBOOK §4.3's claim, and the reason the fallback is usable at all:
		// ownership and mode ride in the headers and must survive untouched.
		if got[1].Uid != 101 || got[1].Gid != 104 || got[1].Mode != 0o640 {
			t.Errorf("ownership/mode did not survive: uid=%d gid=%d mode=%o", got[1].Uid, got[1].Gid, got[1].Mode)
		}
		if got[1].Size != int64(len("hello\n")) {
			t.Errorf("size = %d", got[1].Size)
		}
	})

	t.Run("a mount at the root level is already in the sidecar's shape", func(t *testing.T) {
		// path=/data gives `data/…`, which is what -C / would have produced. The
		// rewrite has to be an identity here, not a double-prefix.
		in := build([]member{
			{name: "data/", typ: tar.TypeDir, mode: 0o755},
			{name: "data/x", body: "x", mode: 0o644},
		})
		got := rewrite(t, in, "data", nil)
		if got[0].Name != "data/" || got[1].Name != "data/x" {
			t.Fatalf("root-level mount was rewritten: %q %q", got[0].Name, got[1].Name)
		}
	})

	t.Run("a single file path re-roots to the file", func(t *testing.T) {
		// Measured: path=/var/lib/foo/top.txt yields one member named `top.txt`.
		in := build([]member{{name: "top.txt", body: "hello\n", mode: 0o640, uid: 101, gid: 104}})
		got := rewrite(t, in, "var/lib/foo/top.txt", nil)
		if len(got) != 1 || got[0].Name != "var/lib/foo/top.txt" {
			t.Fatalf("file member = %+v", got)
		}
		if got[0].Uid != 101 || got[0].Gid != 104 {
			t.Errorf("ownership lost on a file-rooted bind")
		}
	})

	t.Run("a path containing a space survives", func(t *testing.T) {
		// Measured: the daemon emits `a dir/` verbatim.
		in := build([]member{
			{name: "a dir/", typ: tar.TypeDir, mode: 0o755},
			{name: "a dir/f", body: "x\n", mode: 0o644},
		})
		got := rewrite(t, in, "srv/a dir", nil)
		if got[0].Name != "srv/a dir/" || got[1].Name != "srv/a dir/f" {
			t.Fatalf("spaced path mangled: %q %q", got[0].Name, got[1].Name)
		}
	})

	t.Run("two mounts sharing a basename do not collide", func(t *testing.T) {
		// /var/lib/foo and /foo both arrive as `foo/…`. Replacing the component
		// wholesale — rather than trimming a prefix — is what keeps them apart.
		in := build([]member{{name: "foo/x", body: "1", mode: 0o644}})
		deep := rewrite(t, in, "var/lib/foo", nil)
		shallow := rewrite(t, in, "foo", nil)
		if deep[0].Name != "var/lib/foo/x" || shallow[0].Name != "foo/x" {
			t.Fatalf("collided: %q vs %q", deep[0].Name, shallow[0].Name)
		}
	})

	t.Run("exclusions are applied, both forms", func(t *testing.T) {
		// The API has no --exclude of its own. Dropping these is not cosmetic: an
		// embedded database's live data directory that ships anyway lands beside
		// the dump that supersedes it, and the restore starts the app on the torn
		// copy.
		in := build([]member{
			{name: "foo/", typ: tar.TypeDir, mode: 0o755},
			{name: "foo/keep.txt", body: "k", mode: 0o644},
			{name: "foo/postgres/", typ: tar.TypeDir, mode: 0o700},
			{name: "foo/postgres/base/1", body: "pg", mode: 0o600},
		})
		got := rewrite(t, in, "config", []string{"/config/postgres"})
		var names []string
		for _, h := range got {
			names = append(names, h.Name)
		}
		if len(names) != 2 || names[0] != "config/" || names[1] != "config/keep.txt" {
			t.Fatalf("exclusion did not take: %v", names)
		}
	})

	t.Run("a hard link is re-rooted, a symlink's target is not", func(t *testing.T) {
		// A hard link names another member of THIS archive, so it moves with it.
		// A symlink resolves inside the extracted tree at read time; rewriting it
		// would repoint the link at something else.
		in := build([]member{
			{name: "foo/real", body: "r", mode: 0o644},
			{name: "foo/hard", typ: tar.TypeLink, link: "foo/real", mode: 0o644},
			{name: "foo/soft", typ: tar.TypeSymlink, link: "../elsewhere", mode: 0o777},
		})
		got := rewrite(t, in, "var/lib/foo", nil)
		if got[1].Linkname != "var/lib/foo/real" {
			t.Errorf("hard link target = %q, want the re-rooted member", got[1].Linkname)
		}
		if got[2].Linkname != "../elsewhere" {
			t.Errorf("symlink target was rewritten to %q", got[2].Linkname)
		}
	})

	t.Run("ArchiveMemberRoot refuses what cannot be re-rooted", func(t *testing.T) {
		for in, want := range map[string]string{
			"/var/lib/foo":  "var/lib/foo",
			"/data":         "data",
			"/data/":        "data",
			"//data//sub//": "data/sub",
			" /data ":       "data",
			"/":             "", // no first component to replace
			"":              "",
			"relative/path": "", // not a container path
		} {
			if got := ArchiveMemberRoot(in); got != want {
				t.Errorf("ArchiveMemberRoot(%q) = %q, want %q", in, got, want)
			}
		}
	})

	t.Run("the fallback triggers on a refused START and nothing else", func(t *testing.T) {
		// PLAYBOOK §3: branch on the attempt's error. The report's author "took
		// ALLOW_STOP=0 at face value and wrote in two reports that a cold backup
		// was structurally impossible. That was wrong."
		if !SidecarStartRefused(fmt.Errorf("sidecar start: %w", errors.New("Error response from daemon: 403 Forbidden"))) {
			t.Error("a refused start must engage the fallback")
		}
		for _, err := range []error{
			nil,
			errors.New("sidecar create: 403 Forbidden"), // a different problem
			errors.New("sidecar attach: broken pipe"),
			errors.New("tar exited 2"),
		} {
			if SidecarStartRefused(err) {
				t.Errorf("%v must not engage the archive-API fallback", err)
			}
		}
	})
}
