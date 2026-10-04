package backup

import (
	"fmt"
	"strings"
	"testing"

	"dockback/internal/dockercli"
)

// F231 — the stack's own .env, captured and brought back through the remaps.
//
// The bug this closes: a compose file that reads ${DOMAIN} has no literal for a
// domain remap to rewrite, because the value lives in the .env beside it. That
// file was never captured, so a cross-host restore recreated the containers with
// a correctly remapped environment — and then the operator ran
// `docker compose up`, compose re-interpolated the OLD .env, and the move was
// silently undone.

func TestRemapEnvFileRewritesEveryKind(t *testing.T) {
	env := []byte("DOMAIN=old.example.com\nAPI_URL=https://old.example.com/api\n" +
		"HOST_IP=10.168.1.80\nDATA=/mnt/old/stacks/app\nUNRELATED=keepme\n")

	out, ips, domains, paths := RemapEnvFile(env,
		"10.168.1.80", "10.168.1.42",
		"old.example.com", "new.example.com",
		"/mnt/old/stacks", "/srv/stacks")

	got := string(out)
	if domains != 2 {
		t.Errorf("both domain occurrences should move, got %d: %s", domains, got)
	}
	if ips != 1 || paths != 1 {
		t.Errorf("ip=%d path=%d, want 1 and 1: %s", ips, paths, got)
	}
	for _, want := range []string{"DOMAIN=new.example.com", "10.168.1.42", "/srv/stacks/app", "UNRELATED=keepme"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "old.example.com") || strings.Contains(got, "10.168.1.80") {
		t.Errorf("an old value survived:\n%s", got)
	}
}

// Which ARCHIVE MEMBERS are treated as captured host compose files (F57).
//
// This rule decides what may be written to a host filesystem during a restore,
// so it is worth pinning on its own: the prefix match keeps every other member
// of the archive out of that path, and the captured .env is excluded because it
// has its own slot and its own restore (stackEnvFromArchive) — routing it
// through here would write it a second time under a compose-shaped name.
func TestOriginalComposeEntry(t *testing.T) {
	accepted := map[string]string{
		"config/original-compose/docker-compose.yml":   "docker-compose.yml",
		"./config/original-compose/compose.yaml":       "compose.yaml",
		"config/original-compose/2-docker-compose.yml": "2-docker-compose.yml", // archiveComposeName's duplicate
	}
	for in, want := range accepted {
		got, ok := originalComposeEntry(in)
		if !ok || got != want {
			t.Errorf("originalComposeEntry(%q) = (%q,%v); want (%q,true)", in, got, ok, want)
		}
	}

	rejected := []string{
		"config/original-compose/" + stackEnvArchiveName, // the .env has its own restore path
		"config/docker-compose.yml",                      // the RECONSTRUCTION, not an original
		"config/inspect.json",
		"manifest.json",
		"volumes.tar",
		"db/app.dump",
		"config/original-compose/", // the directory entry itself
		"",
		// Capture never produces a nested path here, so a crafted archive
		// offering one is refused outright rather than flattened to a basename.
		"config/original-compose/sub/compose.yml",
		"config/original-compose/../../etc/passwd",
	}
	for _, in := range rejected {
		if got, ok := originalComposeEntry(in); ok {
			t.Errorf("originalComposeEntry(%q) = (%q,true); want rejected", in, got)
		}
	}
}

// What a captured compose file is CALLED inside original-compose/.
//
// Two collisions, both silent before: a second file with the same basename
// replaced the first (the archive claimed two originals and held one), and a
// config file literally named dockback-stack.env took the slot the captured
// .env is restored from — so a compose document could be written to the target
// host AS the stack's .env.
func TestArchiveComposeName(t *testing.T) {
	written := map[string]bool{}

	// First of a name: kept as-is.
	if name, skip := archiveComposeName("docker-compose.yml", written); skip || name != "docker-compose.yml" {
		t.Fatalf("first file should keep its name, got %q skip=%v", name, skip)
	}
	written["docker-compose.yml"] = true

	// Same basename from another directory: prefixed, never overwriting.
	name, skip := archiveComposeName("docker-compose.yml", written)
	if skip || name != "2-docker-compose.yml" {
		t.Fatalf("a colliding name must be prefixed, got %q skip=%v", name, skip)
	}
	written[name] = true

	// A third collision keeps counting rather than reusing the taken prefix.
	if name, skip := archiveComposeName("docker-compose.yml", written); skip || name != "3-docker-compose.yml" {
		t.Fatalf("third collision should be 3-..., got %q skip=%v", name, skip)
	}

	// A different name is unaffected by the collisions above.
	if name, skip := archiveComposeName("override.yml", written); skip || name != "override.yml" {
		t.Fatalf("an unrelated name must be untouched, got %q skip=%v", name, skip)
	}

	// The reserved .env slot is refused outright — never renamed into the archive,
	// because it must not be restorable as the stack's .env.
	if _, skip := archiveComposeName(stackEnvArchiveName, written); !skip {
		t.Errorf("a config file named %q must be skipped, not stored", stackEnvArchiveName)
	}
	// Refused even when nothing has been written yet (the squat is about the NAME,
	// not about the order files arrive in).
	if _, skip := archiveComposeName(stackEnvArchiveName, map[string]bool{}); !skip {
		t.Errorf("the reserved name must be refused regardless of what came before")
	}
}

// Where the .env is READ FROM at capture time.
//
// `docker compose` interpolates from the PROJECT directory, which equals the
// first config file's directory only by default. A stack run with
// --project-directory (or by a tool with its own layout) keeps its .env
// somewhere else entirely, and deriving from the config file captured the wrong
// file — or none — while the backup still looked complete.
func TestStackEnvSourcePath(t *testing.T) {
	cases := []struct {
		name        string
		configFiles string
		workingDir  string
		want        string
	}{
		{"working dir wins", "/elsewhere/compose.yml", "/srv/proj", "/srv/proj/.env"},
		{"falls back beside the first config file", "/srv/app/docker-compose.yml", "", "/srv/app/.env"},
		{"first of several config files", "/srv/app/base.yml,/other/override.yml", "", "/srv/app/.env"},
		{"whitespace is trimmed", "  /srv/app/compose.yml  ", "  ", "/srv/app/.env"},
		{"nothing recorded means nothing to fetch", "", "", ""},
		{"cleans a messy working dir", "/x/compose.yml", "/srv/proj/", "/srv/proj/.env"},
	}
	for _, c := range cases {
		if got := stackEnvSourcePath(c.configFiles, c.workingDir); got != c.want {
			t.Errorf("%s: stackEnvSourcePath(%q,%q) = %q, want %q", c.name, c.configFiles, c.workingDir, got, c.want)
		}
	}
}

// The compose file written on the host gets the SAME three remaps the .env does.
//
// reconstructHostStack's archived-document fallback used to apply the IP and
// path remaps by hand and silently omit the DOMAIN one, so a cross-host restore
// that landed on that branch wrote the old domain to the target — and the next
// `docker compose up` moved the app back to the address it had just left. It now
// routes through RemapEnvFile, which is generic text remap; this pins that all
// three kinds reach a compose-shaped document.
func TestRemapEnvFileRewritesAComposeDocument(t *testing.T) {
	compose := []byte("services:\n  app:\n    image: registry.old.example.com/app:1\n" +
		"    environment:\n      SITE_URL: https://old.example.com\n" +
		"    ports:\n      - \"10.168.1.80:8080:8080\"\n" +
		"    volumes:\n      - /mnt/old/stacks/app/data:/data\n")

	out, ips, domains, paths := RemapEnvFile(compose,
		"10.168.1.80", "10.168.1.42",
		"old.example.com", "new.example.com",
		"/mnt/old/stacks", "/srv/stacks")

	got := string(out)
	if ips == 0 || domains == 0 || paths == 0 {
		t.Fatalf("every remap kind must reach the compose file: ip=%d domain=%d path=%d\n%s", ips, domains, paths, got)
	}
	for _, want := range []string{"10.168.1.42:8080:8080", "https://new.example.com", "/srv/stacks/app/data:/data"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "https://old.example.com") || strings.Contains(got, "10.168.1.80") || strings.Contains(got, "/mnt/old") {
		t.Errorf("an old value survived in the compose file:\n%s", got)
	}
	// A SUBDOMAIN is a different host and deliberately does not move with its
	// parent (domainBoundaryRegexp) — a registry that happens to sit under the
	// old domain is not the site being migrated.
	if !strings.Contains(got, "registry.old.example.com/app:1") {
		t.Errorf("a subdomain must be left alone, not rewritten with its parent:\n%s", got)
	}
}

// Counts are what the log reports, so "nothing matched" must be distinguishable
// from "it was rewritten" — the whole point of F232's warning.
func TestRemapEnvFileReportsNothingWhenNothingMatches(t *testing.T) {
	env := []byte("DOMAIN=${SOMETHING_ELSE}\nPUID=1000\n")
	out, ips, domains, paths := RemapEnvFile(env, "10.0.0.1", "10.0.0.2", "a.example.com", "b.example.com", "/x", "/y")
	if ips != 0 || domains != 0 || paths != 0 {
		t.Errorf("nothing should have matched, got %d/%d/%d", ips, domains, paths)
	}
	if string(out) != string(env) {
		t.Errorf("content must be untouched when nothing matches:\n%s", out)
	}
	// No remap configured at all is a no-op, not a crash.
	if _, i, d, p := RemapEnvFile(env, "", "", "", "", "", ""); i+d+p != 0 {
		t.Error("an unconfigured remap changes nothing")
	}
}

// The operator's own file wins. DockBack's generated entries exist only to
// satisfy the ${VAR} references in the compose file IT wrote, so they fill gaps
// and never overwrite a value the stack is actually configured around.
func TestMergeEnvFilesPrefersTheOperatorsOwnValues(t *testing.T) {
	original := []byte("# my stack\nDOMAIN=new.example.com\nPUID=1000\nDB_PASSWORD=theirs\n")
	generated := []byte("DB_PASSWORD=extracted\nSECRET_KEY=abc123\n")

	got := string(MergeEnvFiles(original, generated))

	if !strings.Contains(got, "DB_PASSWORD=theirs") || strings.Contains(got, "DB_PASSWORD=extracted") {
		t.Errorf("a key defined in both must keep the operator's value:\n%s", got)
	}
	if !strings.Contains(got, "SECRET_KEY=abc123") {
		t.Errorf("a generated key the operator does not define must be added:\n%s", got)
	}
	for _, keep := range []string{"# my stack", "DOMAIN=new.example.com", "PUID=1000"} {
		if !strings.Contains(got, keep) {
			t.Errorf("the operator's file must survive intact, missing %q:\n%s", keep, got)
		}
	}
	// The addition is labelled, so nobody wonders where those lines came from.
	if !strings.Contains(got, "Added by DockBack") {
		t.Errorf("appended entries must say who added them:\n%s", got)
	}
}

func TestMergeEnvFilesEdges(t *testing.T) {
	gen := []byte("A=1\n")
	orig := []byte("B=2\n")
	if string(MergeEnvFiles(nil, gen)) != string(gen) {
		t.Error("no captured file means the generated one stands alone")
	}
	if string(MergeEnvFiles(orig, nil)) != string(orig) {
		t.Error("nothing generated means the operator's file stands alone")
	}
	if string(MergeEnvFiles([]byte("   \n"), gen)) != string(gen) {
		t.Error("a blank captured file is no file")
	}
	// Nothing to add: the original comes back byte-for-byte, not reformatted with
	// an empty "Added by DockBack" section.
	if got := string(MergeEnvFiles(orig, []byte("B=other\n"))); got != string(orig) {
		t.Errorf("with nothing new to add the file must be untouched, got:\n%s", got)
	}
}

func TestEnvKeysIgnoresCommentsAndBlanks(t *testing.T) {
	got := envKeys([]byte("# a comment\n\n  \nFOO=1\n  BAR = 2\n=novalue\nBAZ=\n"))
	want := map[string]bool{"FOO": true, "BAR": true, "BAZ": true}
	if len(got) != 3 {
		t.Fatalf("want 3 keys, got %v", got)
	}
	for _, k := range got {
		if !want[k] {
			t.Errorf("unexpected key %q in %v", k, got)
		}
	}
}

// Compose strips `export`, and ends a name at `=` or `:`. Read differently, a key
// the operator already defines looks missing, and the merge appends it again.
func TestDotenvKeyReadsLikeCompose(t *testing.T) {
	for line, want := range map[string]string{
		"TUNNEL_TOKEN=abc":          "TUNNEL_TOKEN",
		"export TUNNEL_TOKEN=abc":   "TUNNEL_TOKEN",
		"export\tTUNNEL_TOKEN=abc":  "TUNNEL_TOKEN",
		"  BAR = 2":                 "BAR",
		"TZ: Europe/London":         "TZ",
		"URL=http://x:8080/a=b":     "URL",
		"exportFOO=1":               "exportFOO",
		"app.name-1[0]=x":           "app.name-1[0]",
		"# COMMENTED=1":             "",
		"=novalue":                  "",
		"no separator here":         "",
		"not a key=1":               "",
		"MIIBIjANBgkq+hkiG9w0B/A==": "",
		"":                          "",
	} {
		if got := dotenvKey(line); got != want {
			t.Errorf("dotenvKey(%q) = %q, want %q", line, got, want)
		}
	}
}

// The 2026-10-04 recovery: cloudflare's .env came back with TUNNEL_TOKEN twice. An
// operator's `export TUNNEL_TOKEN=` was not recognised, so the merge added the
// generated one as a second definition — and Compose used DockBack's copy.
func TestMergeEnvFilesRecognisesExportedKeys(t *testing.T) {
	original := []byte("export TUNNEL_TOKEN=theirs\nTZ: Europe/London\n")
	generated := []byte("TUNNEL_TOKEN=extracted\nTZ=UTC\n")
	got := MergeEnvFiles(original, generated)
	if string(got) != string(original) {
		t.Errorf("every generated key is already defined, so the file must be untouched, got:\n%s", got)
	}
	if dups := duplicateEnvKeys(got); len(dups) != 0 {
		t.Errorf("the merge must never create a duplicate, got %v", dups)
	}
}

func TestDuplicateEnvKeysNamesEveryLineInOrder(t *testing.T) {
	env := []byte("# tunnel\nTUNNEL_TOKEN=one\nTZ=UTC\nexport TUNNEL_TOKEN=two\nA=1\nTZ: Europe/London\nTUNNEL_TOKEN=three\n")
	got := duplicateEnvKeys(env)
	if len(got) != 2 {
		t.Fatalf("want 2 duplicated keys, got %v", got)
	}
	if got[0].Key != "TUNNEL_TOKEN" || fmt.Sprint(got[0].Lines) != "[2 4 7]" {
		t.Errorf("first duplicate = %+v, want TUNNEL_TOKEN on lines [2 4 7]", got[0])
	}
	if got[1].Key != "TZ" || fmt.Sprint(got[1].Lines) != "[3 6]" {
		t.Errorf("second duplicate = %+v, want TZ on lines [3 6]", got[1])
	}
	if dups := duplicateEnvKeys([]byte("A=1\nB=2\n")); len(dups) != 0 {
		t.Errorf("no key repeats, so nothing to report, got %v", dups)
	}
}

// F230 — a folder DockBack just created must not be root's.
func TestHostFileOwnerPrecedence(t *testing.T) {
	env := []string{"PUID=1000", "PGID=1000"}

	// The operator's pinned choice wins over everything.
	if got := HostFileOwner(1500, 1600, true, env); got != "1500:1600" {
		t.Errorf("a pinned ownership must win, got %q", got)
	}
	// Otherwise the container's own declared ids.
	if got := HostFileOwner(0, 0, false, env); got != "1000:1000" {
		t.Errorf("the container's declared ids are the next best answer, got %q", got)
	}
	// Nothing declared: no answer, so the writer keeps matching the parent rather
	// than inventing an owner.
	if got := HostFileOwner(0, 0, false, []string{"TZ=UTC"}); got != "" {
		t.Errorf("with nothing to go on it must not guess, got %q", got)
	}
	// A container that genuinely declares root is not a better answer than the
	// parent, so it does not override it either.
	if got := HostFileOwner(0, 0, false, []string{"PUID=0", "PGID=0"}); got != "" {
		t.Errorf("declared root is not an improvement on the parent, got %q", got)
	}
}

// The preflight decides "DockBack has this data" by CONTAINER path, because the
// host path remap deliberately makes the manifest's source and the restore's
// source different strings. Keying on source would report every remapped bind as
// uncaptured and tell the operator to go fetch data the backup already holds.
func TestCapturedBindDestinationsIgnoresRemappedSources(t *testing.T) {
	man := &Manifest{Volumes: []VolumeRef{
		{Type: "bind", Source: "/volume1/docker/app/data", Destination: "/app/data"},
		{Type: "volume", Name: "appvol", Destination: "/var/lib/app"},
		{Type: "bind", Source: "/volume1/docker/app/logs", Destination: "/app/logs"},
	}}
	captured := capturedBindDestinations(man)

	// Destinations survive a restore onto a host with a different layout.
	for _, dest := range []string{"/app/data", "/app/logs"} {
		if !captured[dest] {
			t.Errorf("%s is in the backup and must read as captured", dest)
		}
	}
	// A named volume is not a host bind — F90 creates it, the preflight ignores it.
	if captured["/var/lib/app"] {
		t.Error("a named volume must not count as a captured bind source")
	}
	// A read-only mount was never a backup candidate, so it is absent entirely.
	if captured["/run/secrets/key"] {
		t.Error("a mount the backup never captured must not read as captured")
	}
	if len(captured) != 2 {
		t.Errorf("got %d captured destinations, want 2: %v", len(captured), captured)
	}
}

func TestCapturedBindDestinationsHandlesNilManifest(t *testing.T) {
	if got := capturedBindDestinations(nil); len(got) != 0 {
		t.Errorf("a nil manifest must yield no captured destinations, got %v", got)
	}
}

// The 2026-10-04 recovery: six reconstructions had to be swapped for the originals by
// hand. When the backup holds the stack's own compose file, that is the file
// Compose reads, and the reconstruction sits beside it under a name Compose
// ignores.
func TestPlanStackFolderPrefersTheOriginal(t *testing.T) {
	reconstruction := []byte("services: {rebuilt: {}}\n")
	originals := []dockercli.NamedFile{
		{Name: "compose.yaml", Content: []byte("services: {other-dir: {}}\n")},
		{Name: "docker-compose.override.yml", Content: []byte("services: {override: {}}\n")},
		{Name: "docker-compose.yml", Content: []byte("services: {mine: {}}\n")},
	}
	layout := planStackFolder("docker-compose.yml", reconstruction, originals)
	if !layout.PrimaryOriginal || string(layout.Primary) != "services: {mine: {}}\n" {
		t.Fatalf("the original named like the stack's compose file must be primary, got %+v", layout)
	}
	want := map[string]string{
		reconstructionComposeName:     "services: {rebuilt: {}}\n",
		"docker-compose.override.yml": "services: {override: {}}\n",
		// A default name beside the primary would be read instead of it.
		"compose.yaml.original-from-backup": "services: {other-dir: {}}\n",
	}
	if len(layout.Beside) != len(want) {
		t.Fatalf("beside = %+v, want %v", layout.Beside, want)
	}
	for _, f := range layout.Beside {
		if want[f.Name] != string(f.Content) {
			t.Errorf("beside %s = %q, want %q", f.Name, f.Content, want[f.Name])
		}
		if dockercli.IsDefaultComposeName(f.Name) {
			t.Errorf("%s would be read by Compose instead of the primary", f.Name)
		}
	}
}

func TestPlanStackFolderFallsBackToTheReconstruction(t *testing.T) {
	reconstruction := []byte("services: {rebuilt: {}}\n")
	// The captured file is not under the stack's compose name (its first file
	// could not be read at capture), so it cannot stand in for the project.
	layout := planStackFolder("docker-compose.yml", reconstruction,
		[]dockercli.NamedFile{{Name: "docker-compose.prod.yml", Content: []byte("x: 1\n")}})
	if layout.PrimaryOriginal || string(layout.Primary) != string(reconstruction) {
		t.Fatalf("with no original under the compose name the reconstruction is primary, got %+v", layout)
	}
	if len(layout.Beside) != 1 || layout.Beside[0].Name != "docker-compose.prod.yml.original-from-backup" {
		t.Errorf("captured files must go aside, got %+v", layout.Beside)
	}
	if empty := planStackFolder("docker-compose.yml", reconstruction, nil); empty.PrimaryOriginal || len(empty.Beside) != 0 {
		t.Errorf("no originals: reconstruction alone, got %+v", empty)
	}
}

// commafeed came back without a compose file because its database had no
// backup. A restore of part of a project writes the operator's own file, which
// describes all of it, and never a reconstruction of the part.
func TestPartialStackFolderWritesOnlyTheOriginal(t *testing.T) {
	originals := []dockercli.NamedFile{{Name: "docker-compose.yml", Content: []byte("services: {app: {}, postgresql: {}}\n")}}
	layout, ok := partialStackFolder(planStackFolder("docker-compose.yml", []byte("services: {app: {}}\n"), originals))
	if !ok || !layout.PrimaryOriginal || string(layout.Primary) != "services: {app: {}, postgresql: {}}\n" {
		t.Fatalf("the original describes the whole project and must be written, got %+v ok=%v", layout, ok)
	}
	for _, f := range layout.Beside {
		if f.Name == reconstructionComposeName {
			t.Errorf("a reconstruction of part of the project must not be written: %+v", layout.Beside)
		}
	}
	if _, ok := partialStackFolder(planStackFolder("docker-compose.yml", []byte("services: {app: {}}\n"), nil)); ok {
		t.Error("with no original, nothing describes the whole project, so nothing may be written")
	}
}
