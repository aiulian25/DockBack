package backup

import (
	"strings"
	"testing"
)

// R4 §Issue 29, including the correction the author made to their own report:
// "A missing local tag says nothing about whether the reference is still valid
// upstream, and a present local tag says nothing about whether it still matches
// the registry. Here both are true at once."
func TestTagDriftVerdict(t *testing.T) {
	const (
		repo    = "ghcr.io/paperless-ngx/paperless-ngx"
		running = "sha256:80f96a38aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		moved   = "sha256:49eba766bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)

	cases := []struct {
		name        string
		configImage string
		repoTags    []string
		repoDigests []string
		registry    string
		want        tagDriftKind
	}{
		// The live hazard: `:latest` present locally at 3.0.0, registry moved to
		// 3.1.0. The next pull is a minor-version jump with schema migrations.
		{
			name: "the tag moved upstream", configImage: repo + ":latest",
			repoTags: []string{repo + ":latest"}, repoDigests: []string{repo + "@" + running},
			registry: moved, want: tagDrifted,
		},
		// Same tag, same image, both sides. Nothing to say.
		{
			name: "the tag still points where it did", configImage: repo + ":latest",
			repoTags: []string{repo + ":latest"}, repoDigests: []string{repo + "@" + running},
			registry: running, want: tagDriftNone,
		},
		// R4's own case: `:3.0.0` absent locally (the image now carries `:latest`),
		// but upstream `:3.0.0` still resolves to exactly the digest running here.
		{
			name: "the tag is missing only locally", configImage: repo + ":3.0.0",
			repoTags: []string{repo + ":latest"}, repoDigests: []string{repo + "@" + running},
			registry: running, want: tagRemovedLocally,
		},
		// Missing locally AND the registry has moved it too.
		{
			name: "missing locally and moved upstream", configImage: repo + ":3.0.0",
			repoTags: []string{repo + ":latest"}, repoDigests: []string{repo + "@" + running},
			registry: moved, want: tagRemovedLocally,
		},

		// Every case where the question could not be answered says nothing.
		{
			name: "no registry answer", configImage: repo + ":latest",
			repoTags: []string{repo + ":latest"}, repoDigests: []string{repo + "@" + running},
			registry: "", want: tagDriftNone,
		},
		{
			name: "a digest-pinned reference cannot move", configImage: repo + "@" + running,
			repoTags: []string{repo + ":latest"}, repoDigests: []string{repo + "@" + running},
			registry: moved, want: tagDriftNone,
		},
		{
			name: "an image built locally has no registry opinion", configImage: "my-app:dev",
			repoTags: []string{"my-app:dev"}, repoDigests: nil,
			registry: moved, want: tagDriftNone,
		},
		{
			name: "no reference at all", configImage: "",
			repoTags: nil, repoDigests: nil, registry: moved, want: tagDriftNone,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tagDriftVerdict(tc.configImage, tc.repoTags, tc.repoDigests, tc.registry)
			if got.Kind != tc.want {
				t.Fatalf("kind = %d, want %d (%+v)", got.Kind, tc.want, got)
			}
		})
	}

	t.Run("the digest for THIS repository is the one compared", func(t *testing.T) {
		// One image can carry digests from several repositories — a mirror, a
		// rename, a registry migration. Taking the first entry would compare this
		// registry's answer against another repository's digest, which mismatches
		// every time.
		got := tagDriftVerdict(repo+":latest",
			[]string{repo + ":latest"},
			[]string{"docker.io/mirror/paperless-ngx@" + moved, repo + "@" + running},
			running)
		if got.Kind != tagDriftNone {
			t.Fatalf("compared against the wrong repository's digest: %+v", got)
		}
		if got.LocalDigest != repo+"@"+running {
			t.Errorf("local digest = %q", got.LocalDigest)
		}
	})

	t.Run("RegistryAgrees is what stops a missing tag being alarming", func(t *testing.T) {
		agreeing := tagDriftVerdict(repo+":3.0.0", []string{repo + ":latest"}, []string{repo + "@" + running}, running)
		if !agreeing.RegistryAgrees {
			t.Fatal("the registry resolves the tag to the running digest — that must be recorded")
		}
		message := agreeing.describeRemoved("PaperlessNGX", repo+":3.0.0")
		if !strings.Contains(message, "not a problem") || !strings.Contains(message, "Nothing to do") {
			t.Errorf("R4's correction is that this is a non-event; the message must say so:\n%s", message)
		}

		disagreeing := tagDriftVerdict(repo+":3.0.0", []string{repo + ":latest"}, []string{repo + "@" + running}, moved)
		if disagreeing.RegistryAgrees {
			t.Fatal("a moved upstream tag does not agree")
		}
		if msg := disagreeing.describeRemoved("PaperlessNGX", repo+":3.0.0"); !strings.Contains(msg, "unaffected") {
			t.Errorf("even then the restore is unaffected and must say so:\n%s", msg)
		}
	})

	t.Run("the drift message names the running version and both digests", func(t *testing.T) {
		drift := tagDriftVerdict(repo+":latest", []string{repo + ":latest"}, []string{repo + "@" + running}, moved)
		man := &Manifest{ImageConfig: &ImageConfig{Version: "3.0.0"}}
		message := drift.describeDrift("PaperlessNGX", man)
		for _, want := range []string{"PaperlessNGX", "3.0.0", "sha256:80f96a38", "sha256:49eba766", "backup first"} {
			if !strings.Contains(message, want) {
				t.Errorf("message is missing %q:\n%s", want, message)
			}
		}
		// The registry's manifest carries a digest, not the image's labels, so the
		// NEW version is not knowable without the download this check avoids. It
		// must not be invented.
		if strings.Contains(message, "3.1.0") {
			t.Errorf("claimed a version it cannot read:\n%s", message)
		}
		// With no recorded version it still has to read as a sentence.
		if plain := drift.describeDrift("PaperlessNGX", nil); !strings.Contains(plain, "the version it runs today") {
			t.Errorf("unknown version must degrade gracefully:\n%s", plain)
		}
	})

	t.Run("digests are shortened for reading, never truncated to ambiguity", func(t *testing.T) {
		if got := shortDigest(repo + "@" + running); got != "sha256:80f96a38aaaa…" {
			t.Errorf("shortDigest = %q", got)
		}
		if got := shortDigest("sha256:abc"); got != "sha256:abc" {
			t.Errorf("a short digest must survive whole, got %q", got)
		}
	})
}
