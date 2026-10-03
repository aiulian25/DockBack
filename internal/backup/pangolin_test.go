package backup

import (
	"strings"
	"testing"

	"dockback/internal/dockercli"
)

// Pangolin: stack-atomic sets, secret-file permissions, and the preconditions a
// move actually depends on (F146–F149).

func pangolinStack() []*dockercli.Container {
	return []*dockercli.Container{
		{Name: "pangolin", Service: "pangolin", Image: "fosrl/pangolin:1.21.0", Stack: "pangolin"},
		{Name: "gerbil", Service: "gerbil", Image: "fosrl/gerbil:1.4.3", Stack: "pangolin"},
		{Name: "traefik", Service: "traefik", Image: "traefik:v3.7.9", Stack: "pangolin"},
		{Name: "crowdsec", Service: "crowdsec", Image: "crowdsecurity/crowdsec:v1.7.8", Stack: "pangolin"},
	}
}

// A manifest as one member of a captured set would carry it.
func memberManifest(service, groupID string, cs []*dockercli.Container) *Manifest {
	set := StackAtomicFor("fosrl/pangolin:1.21.0")
	return &Manifest{
		Image: "fosrl/" + service, Service: service, TargetName: service, Stack: "pangolin",
		ConsistencyGroup: groupID,
		StackAtomic: &StackAtomicRef{
			Why: set.Why, Symptom: set.Symptom, SoloRestore: set.SoloRestore,
			GroupID: groupID, Members: stackAtomicMembers(cs),
		},
	}
}

// F146 — the anchor declares the set; membership comes from the deployment.
func TestStackAtomicAnchorAndMembers(t *testing.T) {
	set, image := StackAtomicAnchor(pangolinStack())
	if set == nil {
		t.Fatal("the Pangolin stack must anchor an atomic set")
	}
	if !strings.Contains(image, "fosrl/pangolin") {
		t.Errorf("the anchor should be the application image, got %q", image)
	}
	members := stackAtomicMembers(pangolinStack())
	if len(members) != 4 {
		t.Fatalf("every container in the project is a member, got %d", len(members))
	}
	// Deterministic order — a manifest that differs run to run over nothing reads
	// as config drift.
	if members[0].Service != "crowdsec" || members[3].Service != "traefik" {
		t.Errorf("members must be sorted by service, got %v", memberServices(members))
	}
	// The set is discovered, not listed: a stack with a service removed and one
	// added is handled without a code change.
	trimmed := []*dockercli.Container{
		{Name: "pangolin", Service: "pangolin", Image: "fosrl/pangolin:2.0.0", Stack: "p"},
		{Name: "gerbil", Service: "gerbil", Image: "fosrl/gerbil:2.0.0", Stack: "p"},
		{Name: "extra", Service: "extra", Image: "someone/else:1", Stack: "p"},
	}
	if got := memberServices(stackAtomicMembers(trimmed)); len(got) != 3 {
		t.Errorf("a differently-shaped deployment must still resolve, got %v", got)
	}
	// An ordinary stack anchors nothing at all.
	if s, _ := StackAtomicAnchor([]*dockercli.Container{{Name: "web", Image: "nginx:1.27"}}); s != nil {
		t.Error("an ordinary stack must anchor no atomic set")
	}
}

// F146 — restoring one service of the set in place is refused.
func TestStackAtomicSoloRestoreRefused(t *testing.T) {
	man := memberManifest("gerbil", "cg-1", pangolinStack())
	err := StackAtomicSoloRestoreVerdict(man, false)
	if err == nil {
		t.Fatal("restoring one member alone must be refused")
	}
	for _, want := range []string{"pangolin", "traefik", "Nothing on the target has been stopped"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q; got %q", want, err)
		}
	}

	// An isolated clone overwrites nothing, and inspecting a backup must not
	// require permission.
	if err := StackAtomicSoloRestoreVerdict(man, true); err != nil {
		t.Errorf("an isolated clone must always be allowed: %v", err)
	}

	// A backup taken before this existed carries no marker, and a rule it
	// predates must not block it.
	if err := StackAtomicSoloRestoreVerdict(&Manifest{Image: "fosrl/gerbil:1.4.3"}, false); err != nil {
		t.Errorf("a legacy backup must not be refused: %v", err)
	}
	// Neither must an ordinary container.
	if err := StackAtomicSoloRestoreVerdict(&Manifest{Image: "nginx:1.27"}, false); err != nil {
		t.Errorf("an ordinary backup must never be refused: %v", err)
	}
	// A "set" of one is not a set.
	solo := memberManifest("pangolin", "cg-1", pangolinStack()[:1])
	if err := StackAtomicSoloRestoreVerdict(solo, false); err != nil {
		t.Errorf("a single-service stack has nothing to hold together: %v", err)
	}
}

// F146 — a stack restore must cover every member, from ONE snapshot.
func TestStackAtomicGroupVerdict(t *testing.T) {
	cs := pangolinStack()
	complete := map[string]*Manifest{
		"pangolin": memberManifest("pangolin", "cg-1", cs),
		"gerbil":   memberManifest("gerbil", "cg-1", cs),
		"traefik":  memberManifest("traefik", "cg-1", cs),
		"crowdsec": memberManifest("crowdsec", "cg-1", cs),
	}
	if err := StackAtomicGroupVerdict(complete); err != nil {
		t.Fatalf("a complete snapshot must restore: %v", err)
	}

	// A member missing entirely.
	missing := map[string]*Manifest{}
	for k, v := range complete {
		missing[k] = v
	}
	delete(missing, "gerbil")
	err := StackAtomicGroupVerdict(missing)
	if err == nil || !strings.Contains(err.Error(), "gerbil") {
		t.Fatalf("a missing member must be refused by name, got %v", err)
	}

	// Every member present, but one captured on its own — the mixed-point-in-time
	// case, which is the one that looks fine.
	mixed := map[string]*Manifest{}
	for k, v := range complete {
		mixed[k] = v
	}
	mixed["gerbil"] = memberManifest("gerbil", "", cs)
	err = StackAtomicGroupVerdict(mixed)
	if err == nil || !strings.Contains(err.Error(), "gerbil") {
		t.Fatalf("a member captured outside the snapshot must be refused, got %v", err)
	}

	// Members from two different snapshots.
	twoGroups := map[string]*Manifest{}
	for k, v := range complete {
		twoGroups[k] = v
	}
	twoGroups["traefik"] = memberManifest("traefik", "cg-2", cs)
	err = StackAtomicGroupVerdict(twoGroups)
	if err == nil || !strings.Contains(err.Error(), "different snapshots") {
		t.Fatalf("mixing snapshots must be refused, got %v", err)
	}

	// An ordinary stack is judged by none of this.
	ordinary := map[string]*Manifest{"web": {Image: "nginx:1.27"}, "db": {Image: "postgres:16"}}
	if err := StackAtomicGroupVerdict(ordinary); err != nil {
		t.Errorf("an ordinary stack must never be refused: %v", err)
	}
}

// F147 — an over-permissive private key is reported, with the command, and never
// silently changed.
func TestSecretFileFindings(t *testing.T) {
	want := map[string]SecretFile{
		"/var/config/key":        {Path: "/var/config/key", MaxMode: 0o600, What: "the WireGuard private key"},
		"/letsencrypt/acme.json": {Path: "/letsencrypt/acme.json", MaxMode: 0o600, What: "the certificate store"},
		"/app/config/key":        {Path: "/app/config/key", MaxMode: 0o600, What: "the WireGuard private key"},
	}
	modes := []dockercli.FileMode{
		{Path: "/var/config/key", Mode: "644"},
		{Path: "/letsencrypt/acme.json", Mode: "600"},
		{Path: "/app/config/key", Missing: true}, // this image variant does not have it
	}
	got := secretModesOf(want, modes)
	if len(got) != 2 {
		t.Fatalf("a path that does not exist must not be recorded, got %+v", got)
	}
	findings := findingsFor(got)
	if len(findings) != 1 {
		t.Fatalf("only the world-readable key is a finding, got %v", findings)
	}
	for _, s := range []string{"644", "chmod 600 /var/config/key", "never changes them"} {
		if !strings.Contains(findings[0], s) {
			t.Errorf("the finding should contain %q; got %q", s, findings[0])
		}
	}
}

func TestSecretFileModeVerdicts(t *testing.T) {
	cases := []struct {
		mode, max string
		open      bool
	}{
		{"600", "600", false},
		{"400", "600", false}, // tighter than required is fine
		{"644", "600", true},
		{"640", "600", true},
		{"666", "600", true},
		{"", "600", false},    // unread: no verdict
		{"600", "", false},    // no expectation: no verdict
		{"abc", "600", false}, // unparseable: no verdict, never a guess
	}
	for _, c := range cases {
		got := SecretFileMode{Mode: c.mode, Max: c.max}.TooOpen()
		if got != c.open {
			t.Errorf("mode %q vs max %q: got TooOpen=%v want %v", c.mode, c.max, got, c.open)
		}
	}
}

func TestParseFileModes(t *testing.T) {
	got := dockercli.ParseFileModes("MODE\t/a/key\t600\nMODE\t/b/key\t-\nnoise\nMODE\t/c/key\t644\n")
	if len(got) != 3 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Mode != "600" || got[1].Missing != true || got[2].Mode != "644" {
		t.Errorf("unexpected parse: %+v", got)
	}
}

// The profile must carry the claims the docs and the refusals rest on.
func TestPangolinProfile(t *testing.T) {
	p := ProfileFor("fosrl/pangolin:1.21.0")
	if p == nil {
		t.Fatal("Pangolin must have a profile")
	}
	if p.StackAtomic == nil || p.StackAtomic.SoloRestore == "" {
		t.Error("its services must be declared as one atomic set, with guidance for the solo case")
	}
	if p.CredentialStore == "" {
		t.Error("an archive holding a tunnel identity and certificate keys is a credential store")
	}
	if len(p.SecretFiles) == 0 {
		t.Error("the WireGuard key's permissions must be audited")
	}
	if p.OneWayMigration == "" {
		t.Error("the schema migrates forward only — a downgrade must be blocked")
	}
	if !p.LocalOnlyPath("/app/config/db/db.sqlite") {
		t.Error("the database directory must be marked local-disk-only")
	}
	if v := AppVersionCompatibility(p, "1.21.0", "1.19.0"); !v.Blocking {
		t.Error("restoring a newer backup into an older image must be blocked")
	}
	// The stack's own default: a clean stop for the busy database.
	if mode, why := AppPauseDefault("fosrl/pangolin:1.21.0"); mode != PauseStop || why == "" {
		t.Errorf("Pangolin should default to a full stop with a stated reason, got %q / %q", mode, why)
	}
}

// The move guidance is the part that decides whether a restored stack works, so
// it has to say the things that are outside DockBack's reach.
func TestPangolinPreconditions(t *testing.T) {
	pre := AppPreconditionsFor(&Manifest{Image: "fosrl/pangolin:1.21.0"})
	if pre == nil {
		t.Fatal("Pangolin must have preconditions")
	}
	joined := strings.ToLower(strings.Join(pre.Notes, " "))
	for _, want := range []string{"dns", "domain", "renewal", "80", "443", "udp"} {
		if !strings.Contains(joined, want) {
			t.Errorf("preconditions should mention %q; got %q", want, joined)
		}
	}
	// And it must NOT publish the operator's own hostnames: the manifest travels
	// in plaintext to every destination, so guidance names the KIND of record to
	// change, never the records themselves.
	if strings.Contains(joined, "ascunse") {
		t.Error("a specific deployment's domain must never be baked into guidance")
	}
}

// The backup page must say, before the choice is made, that per-service backups
// are the wrong shape for this application.
func TestPangolinAdvertisesTheAtomicSet(t *testing.T) {
	labels := AutoHookLabels("fosrl/pangolin:1.21.0", false)
	joined := strings.Join(labels, " | ")
	if !strings.Contains(joined, "app-consistent snapshot") {
		t.Errorf("the stack-atomic advice must be advertised; got %q", joined)
	}
	// The credential-store escalation still applies and is what drives write-only.
	if !strings.Contains(joined, "write-only") {
		t.Errorf("write-only must be recommended for this archive; got %q", joined)
	}
}
