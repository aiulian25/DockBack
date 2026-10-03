package backup

import (
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

// F190 — one compose file for a stack, not one per service into the same path.
//
// Found by reading the file a restored 5-service Paperless stack left on the new
// host: it described ONE service. Every member of a compose project records the
// same working directory and the same config filename, so a per-service write
// puts five files through one path in turn and the last one wins. Since F176
// made the reconstruction take the canonical name, each also displaced the
// previous — so the folder ended up with a one-service docker-compose.yml and a
// pile of timestamped backups.
func TestMergeComposeDocs(t *testing.T) {
	paperless := []byte(`
services:
  paperless:
    image: ghcr.io/paperless-ngx/paperless-ngx:3.0.0
    container_name: PaperlessNGX
    ports: ["8777:8000"]
    networks: [paperlessngx_default]
networks:
  paperlessngx_default:
    external: true
`)
	db := []byte(`
services:
  db:
    image: postgres:16
    container_name: paper-db
    networks: [paperlessngx_default]
networks:
  paperlessngx_default:
    driver: bridge
    ipam:
      config:
        - subnet: 172.30.0.0/16
`)
	redis := []byte(`
services:
  redis:
    image: redis:8
    container_name: paper-redis
    networks: [paperlessngx_default]
networks:
  paperlessngx_default:
    external: true
`)

	out, _, err := mergeComposeDocs([][]byte{paperless, db, redis})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("the merged file must be valid YAML: %v", err)
	}
	svcs, _ := doc["services"].(map[string]any)
	if len(svcs) != 3 {
		t.Fatalf("every service must survive the merge, got %d: %v", len(svcs), svcs)
	}
	for _, want := range []string{"paperless", "db", "redis"} {
		if _, ok := svcs[want]; !ok {
			t.Errorf("%s is missing from the merged file", want)
		}
	}

	// One network entry, and the REAL definition wins over a bare external — only
	// some members may have recorded one, and `external: true` is the fallback
	// used when nothing is known.
	nets, _ := doc["networks"].(map[string]any)
	if len(nets) != 1 {
		t.Fatalf("the shared network must appear once, got %v", nets)
	}
	def, _ := nets["paperlessngx_default"].(map[string]any)
	if _, bare := def["external"]; bare {
		t.Errorf("a recorded definition must beat `external: true`: %v", def)
	}
	if def["driver"] != "bridge" {
		t.Errorf("the recorded driver must survive: %v", def)
	}

	// The header explaining what this file is stays on top.
	if !strings.HasPrefix(string(out), "#") {
		t.Error("the merged file must keep the explanatory header")
	}
}

// One unreadable member costs its own service, not the other four.
func TestMergeComposeDocsSkipsUnreadable(t *testing.T) {
	good := []byte("services:\n  a:\n    image: alpine\n")
	out, _, err := mergeComposeDocs([][]byte{[]byte("{{{ not yaml"), good})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "a:") {
		t.Errorf("the readable service must still be written: %s", out)
	}

	// Nothing readable at all is an error rather than an empty compose file: a
	// file with no services would `docker compose up` into nothing and look like
	// a stack that had been emptied.
	if _, _, err := mergeComposeDocs([][]byte{[]byte("{{{"), []byte("")}); err == nil {
		t.Error("a merge with no services must fail rather than write an empty file")
	}
}

// A single-container "stack" must come out exactly as it did before — this
// changed the multi-service case, not the ordinary one.
func TestMergeComposeDocsSingleService(t *testing.T) {
	one := []byte("services:\n  app:\n    image: alpine\nnetworks:\n  n:\n    external: true\n")
	out, _, err := mergeComposeDocs([][]byte{one})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	svcs, _ := doc["services"].(map[string]any)
	if len(svcs) != 1 || svcs["app"] == nil {
		t.Errorf("a single service must pass through unchanged: %v", svcs)
	}
	nets, _ := doc["networks"].(map[string]any)
	if !isBareExternal(nets["n"]) {
		t.Errorf("with nothing recorded, external: true remains the only safe assumption: %v", nets)
	}
}

// F192 — the pieces that turned a BookStack restore into a folder named "26"
// and a /volume1 tree on a machine that never had one.
//
// The stack on the source was deployed through a management tool, so its
// recorded compose working directory is the TOOL'S layout — /data/compose/26 —
// while the data the operator cares about lives under /volume1/docker/bookstack.
// Deriving the "auto" remap base from the working dir therefore matched no bind
// at all: nothing was remapped, Docker recreated /volume1 on the new host the
// moment the container started, and the reconstruction faithfully produced a
// folder named 26.
func TestBindBasesForProject(t *testing.T) {
	vols := []VolumeRef{
		{Type: "bind", Source: "/volume1/docker/bookstack/storage-uploads", Destination: "/var/www/bookstack/storage/uploads"},
		{Type: "bind", Source: "/volume1/docker/bookstack/uploads", Destination: "/var/www/bookstack/public/uploads"},
		{Type: "bind", Source: "/volume1/docker/bookstack/db", Destination: "/var/lib/mysql"},
		// Says nothing about where the stack lives; must not vote.
		{Type: "bind", Source: "/etc/localtime", Destination: "/etc/localtime"},
		// A named volume is not a host path at all.
		{Type: "volume", Source: "/var/lib/docker/volumes/x/_data", Destination: "/data"},
	}
	got := BindBasesForProject(vols, "bookstack")
	if len(got) != 3 {
		t.Fatalf("three binds carry the project segment, got %v", got)
	}
	for _, b := range got {
		if b != "/volume1/docker" {
			t.Errorf("base = %q, want /volume1/docker", b)
		}
	}
	// A source whose leaf IS the project votes too — the db bind often is.
	if got := BindBasesForProject([]VolumeRef{{Type: "bind", Source: "/opt/stacks/wiki"}}, "wiki"); len(got) != 1 || got[0] != "/opt/stacks" {
		t.Errorf("a bind ending at the project dir must vote: %v", got)
	}
	if got := BindBasesForProject(vols, ""); got != nil {
		t.Errorf("no project, no votes: %v", got)
	}
}

func TestPreferProjectLeaf(t *testing.T) {
	// The reported case: Portainer's internal id, remapped under the target base.
	dir, changed := PreferProjectLeaf("/home/user/docker/26", "bookstack", "/home/user/docker")
	if !changed || dir != "/home/user/docker/bookstack" {
		t.Errorf("got %q changed=%v", dir, changed)
	}
	// A folder already named for the project is left exactly alone.
	if dir, changed := PreferProjectLeaf("/home/user/docker/bookstack", "bookstack", "/home/user/docker"); changed || dir != "/home/user/docker/bookstack" {
		t.Errorf("a matching leaf must not change: %q %v", dir, changed)
	}
	// No target base means nothing safer to prefer: the recorded path stands,
	// weird leaf and all — inventing a location is worse than an ugly name.
	if dir, changed := PreferProjectLeaf("/data/compose/26", "bookstack", ""); changed || dir != "/data/compose/26" {
		t.Errorf("without a base the recorded dir stands: %q %v", dir, changed)
	}
}

// F194 — the compose file carries no secrets; the .env beside it does.
//
// The contract this serves, in the operator's words: "compose file and .env
// need to live at directory root with all other data." The reconstruction is
// built from docker inspect, so its environment is fully resolved — written
// as-is it leaks on sight, and it diverges from the shape people keep in
// version control: their compose says ${DB_PASSWORD}, ours said the password.
func TestSplitComposeSecrets(t *testing.T) {
	doc := []byte(`# header — reconstructed by DockBack
services:
  bookstack:
    image: solidnerd/bookstack:latest
    environment:
      APP_KEY: base64:abc123==
      APP_URL: https://bookstack.example.uk
      DB_PASSWORD: bookpass
      MAIL_PASSWORD: mailpass
      MAIL_HOST: smtp.gmail.com
      PHP_VERSION: 8.5.4
  db:
    image: mariadb:11.4
    environment:
      MYSQL_ROOT_PASSWORD: rootpass
      MYSQL_PASSWORD: bookpass
      TZ: Europe/London
`)
	compose, envFile, moved := splitComposeSecrets(doc)
	if moved == 0 || len(envFile) == 0 {
		t.Fatal("the secrets must move")
	}
	cs, es := string(compose), string(envFile)

	// No secret value survives in the compose file, and every moved key
	// references a variable instead.
	for _, secret := range []string{"base64:abc123==", "bookpass", "mailpass", "rootpass"} {
		if strings.Contains(cs, secret) {
			t.Errorf("a secret is still in the compose file: %q", secret)
		}
		if !strings.Contains(es, secret) {
			t.Errorf("a secret is missing from the .env: %q", secret)
		}
	}
	for _, ref := range []string{"APP_KEY: ${APP_KEY}", "DB_PASSWORD: ${DB_PASSWORD}", "MYSQL_ROOT_PASSWORD: ${MYSQL_ROOT_PASSWORD}"} {
		if !strings.Contains(cs, ref) {
			t.Errorf("expected reference %q in:\n%s", ref, cs)
		}
	}

	// The values that are NOT secrets stay exactly where they were: an address,
	// a hostname, a version and a timezone belong in the shareable file.
	for _, keep := range []string{"APP_URL: https://bookstack.example.uk", "MAIL_HOST: smtp.gmail.com", "PHP_VERSION: 8.5.4", "TZ: Europe/London"} {
		if !strings.Contains(cs, keep) {
			t.Errorf("a non-secret moved or was lost: %q", keep)
		}
	}
	// The header explaining what the file is survives the round trip.
	if !strings.HasPrefix(cs, "#") {
		t.Error("the compose header must survive")
	}

	// A document with nothing secret in it is returned untouched — no .env, no
	// rewrite, byte-identical, so re-running converges.
	plain := []byte("services:\n  a:\n    image: alpine\n    environment:\n      TZ: UTC\n")
	same, env2, n := splitComposeSecrets(plain)
	if n != 0 || env2 != nil || string(same) != string(plain) {
		t.Errorf("nothing to split must change nothing: n=%d env=%q", n, env2)
	}
}

// The two edges that decide whether the split is safe: values dotenv would
// misread stay inline, and one key with two values gets two variables.
func TestSplitComposeSecretsEdges(t *testing.T) {
	doc := []byte(`services:
  a:
    environment:
      API_TOKEN: "has space value"
      APP_SECRET: has#hash
      DB_PASSWORD: dollar$inside
      GOOD_KEY: cleanvalue
  b:
    environment:
      GOOD_KEY: differentvalue
`)
	compose, envFile, _ := splitComposeSecrets(doc)
	cs, es := string(compose), string(envFile)

	// Unsafe values remain exactly where they were — moved wrongly, a password
	// round-trips broken, which is worse than staying inline.
	for _, stay := range []string{"has space value", "has#hash", "dollar$inside"} {
		if !strings.Contains(cs, stay) {
			t.Errorf("an unsafe value must stay inline: %q\n%s", stay, cs)
		}
		if strings.Contains(es, stay) {
			t.Errorf("an unsafe value must not reach the .env: %q", stay)
		}
	}

	// Same key, different values: the second service gets a scoped variable, and
	// both values survive.
	if !strings.Contains(es, "GOOD_KEY=cleanvalue") || !strings.Contains(es, "B_GOOD_KEY=differentvalue") {
		t.Errorf("conflicting keys must both survive under distinct names:\n%s", es)
	}
	if !strings.Contains(cs, "GOOD_KEY: ${GOOD_KEY}") || !strings.Contains(cs, "GOOD_KEY: ${B_GOOD_KEY}") {
		t.Errorf("each service must reference its own variable:\n%s", cs)
	}
}
