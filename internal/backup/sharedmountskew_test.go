package backup

import (
	"strings"
	"testing"

	"dockback/internal/dockercli"
)

// R3 §Issue 23, reproduced as a fixture: the app on nextcloud:34.0.2 and the
// cron container on nextcloud:apache, both bind-mounting the same
// /var/www/html. "The cron container therefore ships a newer Nextcloud than the
// code it operates on, and the gap widens every time nextcloud:apache is
// rebuilt."
func TestSharedMountVersionSkew(t *testing.T) {
	const shared = "/volume1/docker/nextcloud/html"

	mounter := func(name, image, imageID, source string) *dockercli.Container {
		return &dockercli.Container{
			Name: name, Image: image, ImageID: imageID, Stack: "nextcloud",
			Mounts: []dockercli.Mount{{Type: "bind", Source: source, Destination: "/var/www/html", RW: true}},
		}
	}

	t.Run("differing image IDs on one source produce one finding", func(t *testing.T) {
		skews := SharedMountSkews([]*dockercli.Container{
			mounter("Nextcloud", "nextcloud:34.0.2", "sha256:aaa", shared),
			mounter("Nextcloud-CRON", "nextcloud:apache", "sha256:bbb", shared),
		})
		if len(skews) != 1 {
			t.Fatalf("got %d skews, want 1: %+v", len(skews), skews)
		}
		if skews[0].Source != shared || skews[0].Repository != "nextcloud" {
			t.Fatalf("skew describes the wrong thing: %+v", skews[0])
		}
		// Sorted by container name, so the message never reorders between runs.
		if got := skews[0].Containers(); got[0] != "Nextcloud" || got[1] != "Nextcloud-CRON" {
			t.Fatalf("mounters not in a stable order: %v", got)
		}
		// The message has to name the source, both containers, BOTH images, and
		// §4.6's fix — a finding that says what is wrong without saying what to
		// do about it is a puzzle.
		message := skews[0].Describe()
		for _, want := range []string{shared, "Nextcloud", "Nextcloud-CRON", "nextcloud:34.0.2", "nextcloud:apache", "ONE image digest"} {
			if !strings.Contains(message, want) {
				t.Errorf("message is missing %q:\n%s", want, message)
			}
		}
	})

	t.Run("the same image in two containers is normal and says nothing", func(t *testing.T) {
		// The step's own DO-NOT: a service and its worker off one image is the
		// ordinary case.
		skews := SharedMountSkews([]*dockercli.Container{
			mounter("app", "nextcloud:34.0.2", "sha256:aaa", shared),
			mounter("worker", "nextcloud:34.0.2", "sha256:aaa", shared),
		})
		if len(skews) != 0 {
			t.Fatalf("flagged identical images: %+v", skews)
		}
	})

	t.Run("different applications sharing a data directory are not skew", func(t *testing.T) {
		// The noise case that would otherwise fire on nearly every real stack:
		// immich's server and ML sidecar share the upload directory by design,
		// and they are different images because they are different programs.
		skews := SharedMountSkews([]*dockercli.Container{
			mounter("immich_server", "ghcr.io/immich-app/immich-server:v1.119", "sha256:aaa", "/mnt/pool/immich/upload"),
			mounter("immich_machine_learning", "ghcr.io/immich-app/immich-machine-learning:v1.119", "sha256:bbb", "/mnt/pool/immich/upload"),
		})
		if len(skews) != 0 {
			t.Fatalf("called two different applications a version skew: %+v", skews)
		}
	})

	t.Run("a source only one container mounts is not shared", func(t *testing.T) {
		skews := SharedMountSkews([]*dockercli.Container{
			mounter("app", "nextcloud:34.0.2", "sha256:aaa", shared),
			mounter("cron", "nextcloud:apache", "sha256:bbb", "/volume1/docker/nextcloud/elsewhere"),
		})
		if len(skews) != 0 {
			t.Fatalf("flagged containers that share nothing: %+v", skews)
		}
	})

	t.Run("system paths are not shared application directories", func(t *testing.T) {
		// /etc/localtime is bind-mounted into half the containers on a host; it is
		// not a shared application install.
		skews := SharedMountSkews([]*dockercli.Container{
			mounter("app", "nextcloud:34.0.2", "sha256:aaa", "/etc/localtime"),
			mounter("cron", "nextcloud:apache", "sha256:bbb", "/etc/localtime"),
		})
		if len(skews) != 0 {
			t.Fatalf("flagged a system bind: %+v", skews)
		}
	})

	t.Run("an unknown image id is not evidence on its own", func(t *testing.T) {
		if distinctImageIDs([]SkewMounter{{Container: "a"}, {Container: "b"}}) {
			t.Error("two containers with no recorded image id must not count as a disagreement")
		}
		if distinctImageIDs([]SkewMounter{{Container: "a", ImageID: "sha256:aaa"}, {Container: "b"}}) {
			t.Error("one known id against one unknown is not a measured disagreement")
		}
		if !distinctImageIDs([]SkewMounter{{ImageID: "sha256:aaa"}, {ImageID: "sha256:bbb"}}) {
			t.Error("two different ids IS the disagreement")
		}
	})

	t.Run("a container mounting one source twice is one sharer", func(t *testing.T) {
		solo := &dockercli.Container{
			Name: "app", Image: "nextcloud:34.0.2", ImageID: "sha256:aaa",
			Mounts: []dockercli.Mount{
				{Type: "bind", Source: shared, Destination: "/var/www/html"},
				{Type: "bind", Source: shared, Destination: "/mirror"},
			},
		}
		if skews := SharedMountSkews([]*dockercli.Container{solo}); len(skews) != 0 {
			t.Fatalf("a single container disagreed with itself: %+v", skews)
		}
	})

	t.Run("the repository is read past tags, digests and registry ports", func(t *testing.T) {
		cases := map[string]string{
			"nextcloud:apache":                    "nextcloud",
			"nextcloud":                           "nextcloud",
			"nextcloud@sha256:abc":                "nextcloud",
			"nextcloud:34.0.2@sha256:abc":         "nextcloud",
			"registry:5000/team/app:1.0":          "registry:5000/team/app",
			"ghcr.io/immich-app/immich-server:v1": "ghcr.io/immich-app/immich-server",
		}
		for image, want := range cases {
			if got := imageRepository(image); got != want {
				t.Errorf("imageRepository(%q) = %q, want %q", image, got, want)
			}
		}
	})

	t.Run("three sharers, one odd one out, still one finding", func(t *testing.T) {
		skews := SharedMountSkews([]*dockercli.Container{
			mounter("app", "nextcloud:34.0.2", "sha256:aaa", shared),
			mounter("cron", "nextcloud:apache", "sha256:bbb", shared),
			mounter("worker", "nextcloud:34.0.2", "sha256:aaa", shared),
		})
		if len(skews) != 1 || len(skews[0].Mounters) != 3 {
			t.Fatalf("want one finding naming all three sharers, got %+v", skews)
		}
		// A sentence, not "a and b and c".
		if message := skews[0].Describe(); !strings.Contains(message, "app (nextcloud:34.0.2), cron (nextcloud:apache) and worker") {
			t.Errorf("three sharers do not read as a list:\n%s", message)
		}
	})
}
