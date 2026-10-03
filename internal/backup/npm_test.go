package backup

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// testCertPEM builds a self-signed certificate valid for the given window.
// Generated rather than pinned so the fixture cannot rot into a test that fails
// on a date instead of on a defect.
func testCertPEM(t *testing.T, notBefore, notAfter time.Time, dnsNames ...string) []byte {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "dockback-test"},
		Issuer:       pkix.Name{CommonName: "dockback-test"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		DNSNames:     dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// Nginx Proxy Manager: atomic volume pair, factory-reset guard, certificate
// custody and the quiesce default (F141–F145).

func npmManifest(dests ...string) *Manifest {
	m := &Manifest{Image: "jc21/nginx-proxy-manager:latest"}
	for _, d := range dests {
		m.Volumes = append(m.Volumes, VolumeRef{Destination: d, Type: "bind"})
	}
	return m
}

// F141 — the whole point: half the pair is refused, before anything is touched.
func TestAtomicPairRestoreRefusesHalf(t *testing.T) {
	m := npmManifest("/data")
	m.AtomicVolumes = []string{"/data", "/etc/letsencrypt"}
	err := AtomicVolumeVerdict(m)
	if err == nil {
		t.Fatal("a backup missing /etc/letsencrypt must be refused")
	}
	for _, want := range []string{"/etc/letsencrypt", "nothing on the target has been stopped"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should mention %q; got %q", want, err)
		}
	}
}

func TestAtomicPairRestoreAllowsComplete(t *testing.T) {
	m := npmManifest("/data", "/etc/letsencrypt")
	m.AtomicVolumes = []string{"/data", "/etc/letsencrypt"}
	if err := AtomicVolumeVerdict(m); err != nil {
		t.Fatalf("a complete pair must restore: %v", err)
	}
}

// A backup taken before the set existed records nothing, so the profile decides
// — but only when the archive plainly holds part of the set.
func TestAtomicPairLegacyBackup(t *testing.T) {
	half := npmManifest("/data")
	if err := AtomicVolumeVerdict(half); err == nil {
		t.Error("a legacy archive holding half the pair must still be refused")
	}
	whole := npmManifest("/data", "/etc/letsencrypt")
	if err := AtomicVolumeVerdict(whole); err != nil {
		t.Errorf("a legacy archive holding both must restore: %v", err)
	}
	// Neither member captured: this backup is not of that data at all, and a rule
	// it predates must not block it.
	none := npmManifest("/config")
	if err := AtomicVolumeVerdict(none); err != nil {
		t.Errorf("a legacy archive holding neither member must not be blocked: %v", err)
	}
}

// An ordinary container is untouched by any of this.
func TestAtomicPairIgnoresOtherImages(t *testing.T) {
	m := &Manifest{Image: "nginx:1.27", Volumes: []VolumeRef{{Destination: "/data"}}}
	if err := AtomicVolumeVerdict(m); err != nil {
		t.Fatalf("an image with no atomic set must never be refused: %v", err)
	}
}

// F141 capture side: a deselected member is put back rather than producing an
// archive that can never be restored.
func TestAtomicSelectionForcesMemberBack(t *testing.T) {
	set := AtomicVolumesFor("jc21/nginx-proxy-manager:latest")
	if set == nil {
		t.Fatal("Nginx Proxy Manager must declare an atomic volume set")
	}
	mounted := []string{"/data", "/etc/letsencrypt"}
	chosen := map[string]bool{"/data": true}
	added := enforceAtomicSelection(set, mounted, chosen)
	if len(added) != 1 || added[0] != "/etc/letsencrypt" {
		t.Fatalf("expected /etc/letsencrypt to be added back, got %v", added)
	}
	if !chosen["/etc/letsencrypt"] {
		t.Error("the selection must actually contain the re-added member")
	}
}

// ...but a backup that captures NONE of the set is a backup of something else,
// and must not be quietly enlarged.
func TestAtomicSelectionLeavesUnrelatedSelectionAlone(t *testing.T) {
	set := AtomicVolumesFor("jc21/nginx-proxy-manager:latest")
	chosen := map[string]bool{"/somewhere/else": true}
	if added := enforceAtomicSelection(set, []string{"/data", "/etc/letsencrypt", "/somewhere/else"}, chosen); len(added) != 0 {
		t.Fatalf("a selection touching none of the set must be left alone, got %v", added)
	}
}

// A member the container never mounted cannot be required of the archive — that
// would make every backup of that container permanently unrestorable.
func TestAtomicSetRequiresOnlyMountedPaths(t *testing.T) {
	set := AtomicVolumesFor("jc21/nginx-proxy-manager:latest")
	required, absent := atomicSetRequired(set, []string{"/data"})
	if len(required) != 1 || required[0] != "/data" {
		t.Errorf("required should be just /data, got %v", required)
	}
	if len(absent) != 1 || absent[0] != "/etc/letsencrypt" {
		t.Errorf("absent should be /etc/letsencrypt, got %v", absent)
	}
}

// F142 — the factory-reset guard. Zero rows in a table that had rows is a
// failure, and the message says what an empty table MEANS.
func TestFactoryResetGuardFailsOnEmptyUserTable(t *testing.T) {
	critical := ProfileFor("jc21/nginx-proxy-manager").criticalTables()
	check := dockercli.SQLiteRestoreCheck{
		Path:      "/data/database.sqlite",
		Integrity: "ok",
		TableRows: map[string]int64{"user": 0, "proxy_host": 24},
	}
	ref := SQLiteRef{Source: "/data/database.sqlite", TableRows: map[string]int64{"user": 1, "proxy_host": 24}}
	fail, warn := criticalTableVerdict(critical, check, ref, true)
	if len(fail) != 1 {
		t.Fatalf("an emptied user table must fail the restore, got fail=%v warn=%v", fail, warn)
	}
	if !strings.Contains(fail[0], "setup wizard") {
		t.Errorf("the failure should name the factory-reset outcome; got %q", fail[0])
	}
	if !strings.Contains(fail[0], "before the container was started") {
		t.Errorf("the failure should say the container was not started; got %q", fail[0])
	}
}

// A faithful restore of an application that was already empty is not a defect.
func TestFactoryResetGuardWarnsWhenCaptureWasAlsoEmpty(t *testing.T) {
	critical := ProfileFor("jc21/nginx-proxy-manager").criticalTables()
	check := dockercli.SQLiteRestoreCheck{Path: "/data/database.sqlite", TableRows: map[string]int64{"user": 0}}
	ref := SQLiteRef{Source: "/data/database.sqlite", TableRows: map[string]int64{"user": 0}}
	fail, warn := criticalTableVerdict(critical, check, ref, true)
	if len(fail) != 0 {
		t.Fatalf("an honestly-empty backup must not fail: %v", fail)
	}
	if len(warn) != 1 {
		t.Fatalf("it should still be said out loud, got %v", warn)
	}
}

// No contract to compare against is not a contradiction — warn, never fail.
func TestFactoryResetGuardWarnsWithoutContract(t *testing.T) {
	critical := ProfileFor("jc21/nginx-proxy-manager").criticalTables()
	check := dockercli.SQLiteRestoreCheck{Path: "/data/database.sqlite", TableRows: map[string]int64{"user": 0}}
	fail, warn := criticalTableVerdict(critical, check, SQLiteRef{}, false)
	if len(fail) != 0 || len(warn) != 1 {
		t.Fatalf("a pre-contract backup must warn, not fail: fail=%v warn=%v", fail, warn)
	}
}

// A table that could not be counted yields NO verdict in either direction.
func TestFactoryResetGuardSilentWhenUncounted(t *testing.T) {
	critical := ProfileFor("jc21/nginx-proxy-manager").criticalTables()
	check := dockercli.SQLiteRestoreCheck{Path: "/data/database.sqlite"} // no TableRows at all
	ref := SQLiteRef{Source: "/data/database.sqlite", TableRows: map[string]int64{"user": 1}}
	fail, warn := criticalTableVerdict(critical, check, ref, true)
	if len(fail) != 0 || len(warn) != 0 {
		t.Fatalf("an uncounted table must produce no verdict: fail=%v warn=%v", fail, warn)
	}
}

// The guard is scoped to the database it names — another SQLite file in the same
// backup with a coincidentally-named table must not be judged by it.
func TestFactoryResetGuardScopedToItsDatabase(t *testing.T) {
	critical := ProfileFor("jc21/nginx-proxy-manager").criticalTables()
	check := dockercli.SQLiteRestoreCheck{Path: "/data/other.sqlite", TableRows: map[string]int64{"user": 0}}
	ref := SQLiteRef{Source: "/data/other.sqlite", TableRows: map[string]int64{"user": 5}}
	fail, warn := criticalTableVerdict(critical, check, ref, true)
	if len(fail) != 0 || len(warn) != 0 {
		t.Fatalf("a different database must not be judged: fail=%v warn=%v", fail, warn)
	}
}

// F145 — an ingress proxy is not paused for the copy, and every other app still
// gets the shipped default.
func TestNPMDefaultsToNoQuiesce(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := &Engine{Store: st}
	if got := e.resolvePauseMode(Options{NodeID: "n1"}, "jc21/nginx-proxy-manager:latest", "npm"); got != PauseNone {
		t.Errorf("Nginx Proxy Manager should default to %q, got %q", PauseNone, got)
	}
	if got := e.resolvePauseMode(Options{NodeID: "n1"}, "nginx:1.27", "web"); got != PausePause {
		t.Errorf("an ordinary app should keep the shipped default %q, got %q", PausePause, got)
	}
	// An explicit choice still wins — the app default is a default, not a policy.
	if got := e.resolvePauseMode(Options{NodeID: "n1", PauseMode: PauseStop}, "jc21/nginx-proxy-manager:latest", "npm"); got != PauseStop {
		t.Errorf("an explicit per-run choice must win, got %q", got)
	}
	mode, why := AppPauseDefault("jc21/nginx-proxy-manager:latest")
	if mode != PauseNone || why == "" {
		t.Errorf("the default must come with a stated reason, got %q / %q", mode, why)
	}
	if m, _ := AppPauseDefault("nginx:1.27"); m != "" {
		t.Errorf("an image declaring no default must report none, got %q", m)
	}
}

// F143 — a certificate's expiry is read from its own bytes; the private key's is
// never read at all.
func TestCertRefParsesLeafOnly(t *testing.T) {
	now := time.Now()
	pemBytes := testCertPEM(t, now.Add(-time.Hour), now.Add(60*24*time.Hour), "a.example.com", "b.example.com")
	ref, ok := certRefOf(dockercli.CertFile{Path: "/etc/letsencrypt/live/npm-4/fullchain.pem", PEM: pemBytes, HasKey: true})
	if !ok {
		t.Fatal("a valid certificate must parse")
	}
	if ref.NotAfter == "" || ref.NotBefore == "" {
		t.Error("validity bounds must be recorded")
	}
	if !ref.HasKey {
		t.Error("the private key's presence must be recorded")
	}
	if _, err := time.Parse(time.RFC3339, ref.NotAfter); err != nil {
		t.Errorf("NotAfter must be RFC3339: %v", err)
	}
	if ref.Names != 2 {
		t.Errorf("the NUMBER of covered names is recorded, got %d", ref.Names)
	}
	// The manifest travels in plaintext to every destination: it must count the
	// operator's hostnames, never publish them.
	if strings.Contains(ref.Issuer, "example.com") {
		t.Errorf("a covered hostname leaked into the manifest record: %q", ref.Issuer)
	}
	if certExpired(ref, now) {
		t.Error("a certificate valid for another two months is not expired")
	}
}

// A chain file holds the leaf first, then its intermediates. The leaf's expiry
// is the one that decides whether the deployment works, so that is the one
// recorded.
func TestCertRefReadsLeafOfChain(t *testing.T) {
	now := time.Now()
	leaf := testCertPEM(t, now.Add(-time.Hour), now.Add(30*24*time.Hour), "leaf.example.com")
	inter := testCertPEM(t, now.Add(-time.Hour), now.Add(3650*24*time.Hour), "inter.example.com")
	chain := append(append([]byte{}, leaf...), inter...)

	ref, ok := certRefOf(dockercli.CertFile{Path: "/etc/letsencrypt/live/npm-4/fullchain.pem", PEM: chain})
	if !ok {
		t.Fatal("a chain must parse")
	}
	leafRef, _ := certRefOf(dockercli.CertFile{Path: "x", PEM: leaf})
	if ref.NotAfter != leafRef.NotAfter {
		t.Errorf("the LEAF's expiry decides; got %q want %q", ref.NotAfter, leafRef.NotAfter)
	}
}

// A file matched by name that is not a certificate is dropped, not recorded as a
// certificate DockBack could not read.
func TestCertRefRejectsNonCertificate(t *testing.T) {
	if _, ok := certRefOf(dockercli.CertFile{Path: "/data/custom_ssl/cert.pem", PEM: []byte("not a certificate")}); ok {
		t.Error("a non-certificate must not be recorded")
	}
	if _, ok := certRefOf(dockercli.CertFile{Path: "/x/privkey.pem", PEM: []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n")}); ok {
		t.Error("a key file must never be recorded as a certificate")
	}
}

func TestCertExpiryVerdicts(t *testing.T) {
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	past := CertRef{NotAfter: now.Add(-24 * time.Hour).Format(time.RFC3339)}
	soon := CertRef{NotAfter: now.Add(10 * 24 * time.Hour).Format(time.RFC3339)}
	fine := CertRef{NotAfter: now.Add(60 * 24 * time.Hour).Format(time.RFC3339)}
	unknown := CertRef{}

	if !certExpired(past, now) {
		t.Error("an expired certificate must read as expired")
	}
	if certExpired(soon, now) || certExpired(fine, now) {
		t.Error("a valid certificate must not read as expired")
	}
	if !certExpiringWithin(soon, now, certExpiryWarnDays) {
		t.Error("a certificate inside the window must be flagged")
	}
	if certExpiringWithin(fine, now, certExpiryWarnDays) {
		t.Error("a certificate outside the window must not be flagged")
	}
	// The honesty rule: an unreadable date is not evidence of anything.
	if certExpired(unknown, now) || certExpiringWithin(unknown, now, certExpiryWarnDays) {
		t.Error("an unrecorded expiry must produce no verdict")
	}
}

// The profile must declare what makes this application dangerous to lose and
// impossible to half-restore. These are the claims the docs and the UI rest on.
func TestNPMProfile(t *testing.T) {
	p := ProfileFor("jc21/nginx-proxy-manager:2.15.1")
	if p == nil {
		t.Fatal("Nginx Proxy Manager must have a profile")
	}
	if p.AtomicVolumes == nil || len(p.AtomicVolumes.Paths) != 2 {
		t.Fatal("both volumes must be declared as one set")
	}
	if p.CredentialStore == "" {
		t.Error("an archive holding TLS private keys and DNS credentials is a credential store")
	}
	if p.OneWayMigration == "" {
		t.Error("the schema migrates forward only — a downgrade must be blocked")
	}
	if len(p.CertificateRoots) == 0 {
		t.Error("its certificate directories must be declared")
	}
	if len(p.NeverBackup) == 0 {
		t.Error("rotated nginx logs have no restore value and must be excluded")
	}
	// /data must be local disk: the database is written continuously.
	if !p.LocalOnlyPath("/data/database.sqlite") {
		t.Error("/data must be marked local-disk-only")
	}
	// A downgrade is blocked; an upgrade warns.
	if v := AppVersionCompatibility(p, "2.15.1", "2.11.0"); !v.Blocking {
		t.Error("restoring a newer backup into an older image must be blocked")
	}
	if v := AppVersionCompatibility(p, "2.11.0", "2.15.1"); v.Blocking || v.Warning == "" {
		t.Error("restoring into a newer image must warn, not block")
	}
}

// The pre-restore panel must say the pair is a pair, and what half of it does.
func TestNPMPreconditionsMentionTheAtomicPair(t *testing.T) {
	m := npmManifest("/data", "/etc/letsencrypt")
	pre := AppPreconditionsFor(m)
	if pre == nil {
		t.Fatal("Nginx Proxy Manager must have preconditions")
	}
	joined := strings.Join(pre.Notes, " ")
	for _, want := range []string{"/etc/letsencrypt", "factory-fresh", "refuses"} {
		if !strings.Contains(joined, want) {
			t.Errorf("preconditions should mention %q; got %q", want, joined)
		}
	}
}

// Certbot writes the same certificate twice — cert.pem is the leaf, fullchain.pem
// is the leaf plus intermediates — so a scan by filename finds one certificate
// and would report two. "2 certificates expiring" out of one is worse than
// useless.
func TestCertRefsDeduplicatesCertbotLayout(t *testing.T) {
	now := time.Now()
	leaf := testCertPEM(t, now.Add(-time.Hour), now.Add(60*24*time.Hour), "a.example.com")
	inter := testCertPEM(t, now.Add(-time.Hour), now.Add(3650*24*time.Hour), "ca.example.com")
	chain := append(append([]byte{}, leaf...), inter...)

	refs := certRefsOf([]dockercli.CertFile{
		{Path: "/etc/letsencrypt/live/npm-4/cert.pem", PEM: leaf, HasKey: true},
		{Path: "/etc/letsencrypt/live/npm-4/fullchain.pem", PEM: chain, HasKey: true},
		{Path: "/data/custom_ssl/1/certificate.pem", PEM: testCertPEM(t, now, now.Add(24*time.Hour), "b.example.com")},
	})
	if len(refs) != 2 {
		t.Fatalf("one certbot certificate plus one uploaded one is two records, got %d: %+v", len(refs), refs)
	}
	// fullchain.pem wins: it is the file a server is actually configured to load.
	found := false
	for _, r := range refs {
		if r.Path == "/etc/letsencrypt/live/npm-4/fullchain.pem" {
			found = true
		}
		if r.Path == "/etc/letsencrypt/live/npm-4/cert.pem" {
			t.Error("cert.pem should have been folded into fullchain.pem")
		}
	}
	if !found {
		t.Error("the fullchain record must be the one kept")
	}

	// Two genuinely different certificates in the same directory both survive.
	two := certRefsOf([]dockercli.CertFile{
		{Path: "/x/a.crt", PEM: testCertPEM(t, now, now.Add(10*24*time.Hour), "a")},
		{Path: "/x/b.crt", PEM: testCertPEM(t, now, now.Add(20*24*time.Hour), "b")},
	})
	if len(two) != 2 {
		t.Errorf("distinct certificates must not be merged, got %+v", two)
	}
}
