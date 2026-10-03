package backup

import (
	"strings"
	"testing"
)

// TestProfileFor locks in that only a recognised image gets a profile — an
// ordinary container must keep the unchanged restore path.
func TestProfileFor(t *testing.T) {
	if p := ProfileFor("ghcr.io/advplyr/audiobookshelf:latest"); p == nil || p.Name != "Audiobookshelf" {
		t.Fatalf("audiobookshelf image should match a profile, got %+v", p)
	}
	for _, img := range []string{"nginx:alpine", "postgres:16", "", "mariadb:11.4-noble", "redis:7"} {
		if p := ProfileFor(img); p != nil {
			t.Fatalf("%q should have no profile, got %+v", img, p)
		}
	}
}

// TestLocalOnlyPath covers the nested case: /config/metadata inherits /config's
// local-disk requirement, but an unrelated path that merely shares a prefix
// ("/configuration") must not.
func TestLocalOnlyPath(t *testing.T) {
	p := ProfileFor("audiobookshelf")
	cases := map[string]bool{
		"/config":          true,
		"/config/metadata": true,
		"/configuration":   false,
		"/audiobooks":      false,
		"":                 false,
	}
	for dest, want := range cases {
		if got := p.LocalOnlyPath(dest); got != want {
			t.Errorf("LocalOnlyPath(%q) = %v, want %v", dest, got, want)
		}
	}
	var nilp *AppProfile
	if nilp.LocalOnlyPath("/config") {
		t.Error("a nil profile must claim nothing")
	}
}

// TestBookStackProfile locks in what BookStack does and does NOT declare. The
// negatives matter as much as the positives: its uploads travel inside the
// archive and its database is a separate container, so claiming a path or
// local-disk requirement would put a warning in front of the operator that is
// simply untrue.
func TestBookStackProfile(t *testing.T) {
	for _, img := range []string{"solidnerd/bookstack:latest", "lscr.io/linuxserver/bookstack:24"} {
		p := ProfileFor(img)
		if p == nil || p.Name != "BookStack" {
			t.Fatalf("%q should match the BookStack profile, got %+v", img, p)
		}
		if p.OneWayMigration == "" {
			t.Error("BookStack runs migrate --force on every start — that must be declared")
		}
		if !p.NoHealthcheck {
			t.Error("the BookStack images ship no HEALTHCHECK — that must be declared, it is what makes the settle window apply")
		}
		if p.currentAddressEnvKey() != "APP_URL" {
			t.Errorf("BookStack's single current address is APP_URL, got %q", p.currentAddressEnvKey())
		}
		var sawEnv, sawContent bool
		for _, bnd := range p.Address {
			switch bnd.Kind {
			case BindEnv:
				sawEnv = true
			case BindContentRewrite:
				sawContent = true
			}
		}
		if !sawEnv || !sawContent {
			t.Errorf("BookStack needs a reversible env binding AND an irreversible content rewrite, got %+v", p.Address)
		}
		// BookStack is reachable at a new address without any of this — only its
		// content links go stale. Marking it blocking would cry wolf.
		if len(p.BlockingAddressSymptoms()) != 0 {
			t.Errorf("BookStack stays reachable at a new address — nothing here should be blocking, got %v", p.BlockingAddressSymptoms())
		}
		if len(p.LocalOnly) != 0 {
			t.Errorf("BookStack's database is a separate container — it has no local-disk path requirement, got %v", p.LocalOnly)
		}
		if p.PathEmbedding != "" {
			t.Error("BookStack's uploads travel inside the archive — it must not claim a path-reproduction requirement")
		}
	}
}

// TestWikiJSProfile covers both image names Wiki.js ships under. Neither is a
// substring of the other, so matching only one would silently skip the guard for
// half the deployments in the wild.
func TestWikiJSProfile(t *testing.T) {
	for _, img := range []string{"ghcr.io/linuxserver/wikijs:latest", "requarks/wiki:2", "ghcr.io/requarks/wiki:2.5"} {
		p := ProfileFor(img)
		if p == nil || p.Name != "Wiki.js" {
			t.Fatalf("%q should match the Wiki.js profile, got %+v", img, p)
		}
		if p.OneWayMigration == "" {
			t.Errorf("%q: Wiki.js migrates on start — that must be declared", img)
		}
	}
}

// TestAddressCommandSteps: the rendered commands must carry the REAL current
// address. A half-filled command is worse than none — it invites running a
// rewrite with a wrong "from" value, which changes nothing while appearing to
// work.
func TestAddressCommandSteps(t *testing.T) {
	p := ProfileFor("solidnerd/bookstack")

	// No new address yet: show what a future change would take.
	steps := p.AddressCommandSteps("BookStack", "https://books.example.com", "")
	if len(steps) != 2 {
		t.Fatalf("expected the update-url + cache:clear pair, got %v", steps)
	}
	if !strings.Contains(steps[0], "docker exec BookStack php artisan bookstack:update-url https://books.example.com ") {
		t.Errorf("first step should be update-url with the current address filled in, got %q", steps[0])
	}
	if !strings.Contains(steps[0], "NEW-ADDRESS-HERE") {
		t.Errorf("with no new address supplied the target must be an obvious placeholder, got %q", steps[0])
	}
	if !strings.Contains(steps[1], "cache:clear") {
		t.Errorf("second step should clear the cache, got %q", steps[1])
	}

	// New address supplied: both ends real, ready to paste.
	steps = p.AddressCommandSteps("BookStack", "https://books.example.com", "https://wiki.new.example")
	if !strings.Contains(steps[0], "https://books.example.com https://wiki.new.example") {
		t.Errorf("expected both addresses filled in, got %q", steps[0])
	}

	if got := p.AddressCommandSteps("BookStack", "   ", "https://x"); got != nil {
		t.Errorf("an unreadable current address must render nothing, got %v", got)
	}
	if got := p.AddressCommandSteps("", "https://x", ""); got != nil {
		t.Errorf("no container name must render nothing, got %v", got)
	}
	// Nextcloud's binding is applied through occ, not printed as a command.
	if got := ProfileFor("nextcloud").AddressCommandSteps("nc", "https://x", ""); got != nil {
		t.Errorf("an app whose binding DockBack applies must render no manual command, got %v", got)
	}
	var nilp *AppProfile
	if got := nilp.AddressCommandSteps("x", "https://y", ""); got != nil {
		t.Errorf("a nil profile must render nothing, got %v", got)
	}
}

// TestBlockingApps pins the distinction the whole feature turns on: which apps
// become UNREACHABLE at a new address, versus merely imperfect. Getting this
// backwards either cries wolf or hides a silent outage.
func TestBlockingApps(t *testing.T) {
	blocking := map[string]bool{
		"nextcloud":                      true, // "untrusted domain" — refuses every request
		"ghcr.io/gethomepage/homepage":   true, // HTTP 400 — dashboard gone
		"solidnerd/bookstack":            false,
		"ghcr.io/linuxserver/wikijs":     false, // relative links; only emails go stale
		"ghcr.io/advplyr/audiobookshelf": false,
	}
	for img, want := range blocking {
		p := ProfileFor(img)
		if p == nil {
			t.Fatalf("%q should have a profile", img)
		}
		got := len(p.BlockingAddressSymptoms()) > 0
		if got != want {
			t.Errorf("%s: blocking = %v, want %v (symptoms %v)", p.Name, got, want, p.BlockingAddressSymptoms())
		}
		if want {
			// A blocking symptom must describe what the operator SEES, so they
			// can match it against their browser.
			for _, s := range p.BlockingAddressSymptoms() {
				if len(s) < 20 {
					t.Errorf("%s: symptom %q is too vague to match against a real error page", p.Name, s)
				}
			}
		}
	}
}

// TestHomepageProfile: the accepted-hosts list is a SET, and the new address is
// added to it. Replacing it would lock the dashboard out at every address it
// currently answers on — turning a fix for one address into an outage at all.
func TestHomepageProfile(t *testing.T) {
	p := ProfileFor("ghcr.io/gethomepage/homepage:latest")
	if p == nil || p.Name != "Homepage" {
		t.Fatalf("expected the Homepage profile, got %+v", p)
	}
	if len(p.Address) != 1 || p.Address[0].Kind != BindEnv {
		t.Fatalf("Homepage's address is an env var, got %+v", p.Address)
	}
	b := p.Address[0]
	if !b.List {
		t.Error("HOMEPAGE_ALLOWED_HOSTS is a set of accepted hosts — it must be marked as a list so entries are added, not replaced")
	}
	if !b.Blocking {
		t.Error("an unlisted host gets HTTP 400 — this is blocking")
	}
	if len(b.Keys) != 1 || b.Keys[0] != "HOMEPAGE_ALLOWED_HOSTS" {
		t.Errorf("unexpected keys %v", b.Keys)
	}
}

// TestNextcloudProfile: Nextcloud is the one app DockBack actively reconfigures,
// and only through occ.
func TestNextcloudProfile(t *testing.T) {
	p := ProfileFor("nextcloud:31-apache")
	if p == nil || p.Name != "Nextcloud" {
		t.Fatalf("expected the Nextcloud profile, got %+v", p)
	}
	// One occ binding, plus the environment variables the official image reads
	// its address from (F175). The occ half alone was not enough: the image's own
	// config/reverse-proxy.config.php reads OVERWRITEHOST from the environment on
	// every request and is loaded AFTER config.php, so the variable carried over
	// from the previous machine silently overrode everything occ had just set.
	var b AddressBinding
	env := map[string]AddressBinding{}
	for _, a := range p.Address {
		switch a.Kind {
		case BindAppCommand:
			b = a
		case BindEnv:
			for _, k := range a.Keys {
				env[k] = a
			}
		}
	}
	if b.Kind != BindAppCommand || b.Apply != applyNextcloudTrustedDomain {
		t.Errorf("Nextcloud's address must be applied by the occ procedure, got kind=%q apply=%q", b.Kind, b.Apply)
	}
	if !b.Blocking {
		t.Error("a stale trusted_domains makes Nextcloud refuse every request — this is blocking")
	}
	// The three parts of one address, each shaped for the variable that holds it
	// — writing the same string into all three would give a host with a scheme in
	// it and a protocol that is a URL.
	for key, want := range map[string]EnvShape{
		"OVERWRITEHOST":     EnvHost,
		"OVERWRITECLIURL":   EnvURL,
		"OVERWRITEPROTOCOL": EnvScheme,
	} {
		got, ok := env[key]
		if !ok {
			t.Errorf("%s is read from the environment on every request and must be bound", key)
			continue
		}
		if got.Shape != want {
			t.Errorf("%s should be written as %q, got %q", key, want, got.Shape)
		}
	}
	// Space-separated, because the entrypoint word-splits it. A comma would make
	// two domains into one entry that matches nothing.
	if td, ok := env["NEXTCLOUD_TRUSTED_DOMAINS"]; !ok {
		t.Error("the trusted-domains variable must be bound")
	} else if !td.List || td.ListSep != " " {
		t.Errorf("NEXTCLOUD_TRUSTED_DOMAINS is a space-separated list, got list=%v sep=%q", td.List, td.ListSep)
	}
	// The note must state the trusted_proxies carve-out: it is a security
	// decision, not an omission.
	if !strings.Contains(b.Note, "trusted_proxies") {
		t.Errorf("the note must explain why trusted_proxies is left alone, got %q", b.Note)
	}
}

// TestValidSiteAddress is a security boundary as much as a usability one: this
// value lands in a trust list and in commands run inside the container.
func TestValidSiteAddress(t *testing.T) {
	ok := []string{
		"cloud.example.com", "https://cloud.example.com", "http://10.168.1.50:8080",
		"10.168.1.50", "nas", "[2001:db8::1]:8443", "https://cloud.example.com/",
	}
	for _, s := range ok {
		if err := ValidSiteAddress(s); err != nil {
			t.Errorf("ValidSiteAddress(%q) should pass, got %v", s, err)
		}
	}
	// The wildcard rejection is the important one: "*" would make the restore
	// succeed by disabling the protection the value exists to provide.
	bad := []string{
		"", "   ", "*", "*.example.com", "cloud.example.com *",
		"a;rm -rf /", "a && b", "a|b", "$(whoami)", "`id`", "a'b", `a"b`,
		"host\nother", strings.Repeat("a", 300),
	}
	for _, s := range bad {
		if err := ValidSiteAddress(s); err == nil {
			t.Errorf("ValidSiteAddress(%q) must be rejected", s)
		}
	}
	if err := ValidSiteAddress("*"); err == nil || !strings.Contains(err.Error(), "trust list") {
		t.Errorf("the wildcard rejection should explain WHY, got %v", err)
	}
}

// TestAddressHost strips a scheme and path, because a trust list holds HOSTS —
// an entry carrying "https://" silently never matches, which presents to the
// operator as "the fix did nothing" with no clue why.
func TestAddressHost(t *testing.T) {
	cases := map[string]string{
		"https://cloud.example.com":       "cloud.example.com",
		"http://10.168.1.50:8080":         "10.168.1.50:8080",
		"cloud.example.com":               "cloud.example.com",
		"https://cloud.example.com/path/": "cloud.example.com",
		"  https://x.example  ":           "x.example",
		"":                                "",
	}
	for in, want := range cases {
		if got := AddressHost(in); got != want {
			t.Errorf("AddressHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBookStackPreconditions: the panel must tell the operator that the COMMON
// move needs no rewrite. Running the rewrite "to be safe" on a same-domain move
// irreversibly edits content for no reason, and that is the mistake worth
// pre-empting.
func TestBookStackPreconditions(t *testing.T) {
	pre := AppPreconditionsFor(&Manifest{
		Image:   "solidnerd/bookstack:latest",
		Volumes: []VolumeRef{{Destination: "/var/www/bookstack/public/uploads", Type: "bind"}},
	})
	if pre == nil || pre.App != "BookStack" {
		t.Fatalf("expected BookStack preconditions, got %+v", pre)
	}
	if len(pre.DataPaths) != 0 {
		t.Errorf("BookStack declares no path-reproduction requirement, so no paths should be listed, got %v", pre.DataPaths)
	}
	var sawMigration, sawURL bool
	for _, n := range pre.Notes {
		if strings.Contains(n, "migrations") {
			sawMigration = true
		}
		if strings.Contains(n, "content rewrite") {
			sawURL = true
		}
	}
	if !sawMigration {
		t.Errorf("expected the one-way migration note, got %v", pre.Notes)
	}
	if !sawURL {
		t.Errorf("expected the content-rewrite note, got %v", pre.Notes)
	}
	// BookStack is reachable at a new address regardless, so its panel must not
	// be styled as a blocking warning.
	if pre.Blocking {
		t.Error("BookStack stays reachable at a new address — its panel must not claim otherwise")
	}
	// It DOES record its address, so the field is offered.
	if pre.AddressPrompt == "" {
		t.Error("BookStack records its address in APP_URL, so the new-address field should be offered")
	}
}

// TestNextcloudPreconditionsBlocking is the case the whole feature exists for: a
// faithful restore puts the OLD address back into config.php, and the site then
// refuses every request. The panel has to say so before the operator commits.
func TestNextcloudPreconditionsBlocking(t *testing.T) {
	pre := AppPreconditionsFor(&Manifest{
		Image:   "nextcloud:31-apache",
		Volumes: []VolumeRef{{Destination: "/var/www/html", Type: "bind"}},
	})
	if pre == nil {
		t.Fatal("expected Nextcloud preconditions")
	}
	if !pre.Blocking {
		t.Error("a stale trusted_domains makes Nextcloud unreachable — the panel must be marked blocking")
	}
	if pre.AddressPrompt == "" {
		t.Error("the new-address field must be offered for Nextcloud")
	}
	joined := strings.Join(pre.Notes, " ")
	if !strings.Contains(joined, "untrusted domain") {
		t.Errorf("the note must quote the error the operator will actually see, got %v", pre.Notes)
	}
	// Equally important: say that the common move needs nothing, so nobody
	// changes an address that was not changing.
	if !strings.Contains(joined, "Keeping the same address") {
		t.Errorf("the note must say a same-address move needs nothing here, got %v", pre.Notes)
	}
}

// TestHomepagePreconditionsBlocking: an unlisted Host header is a bare HTTP 400
// with no explanation anywhere — the operator needs the symptom named up front.
func TestHomepagePreconditionsBlocking(t *testing.T) {
	pre := AppPreconditionsFor(&Manifest{Image: "ghcr.io/gethomepage/homepage:latest"})
	if pre == nil || !pre.Blocking {
		t.Fatalf("Homepage returns HTTP 400 at an unlisted address — must be blocking, got %+v", pre)
	}
	if !strings.Contains(strings.Join(pre.Notes, " "), "400") {
		t.Errorf("the note must name the HTTP 400 the operator will see, got %v", pre.Notes)
	}
}

// TestUnprofiledAppOffersNoAddressField — the overwhelming majority of
// containers. The restore dialog must be exactly what it was.
func TestUnprofiledAppOffersNoAddressField(t *testing.T) {
	if pre := AppPreconditionsFor(&Manifest{Image: "nginx:alpine"}); pre != nil {
		t.Errorf("an unprofiled image must render no panel at all, got %+v", pre)
	}
	// An app with preconditions but no address binding gets the panel, without
	// the address field.
	pre := AppPreconditionsFor(&Manifest{
		Image:         "ghcr.io/advplyr/audiobookshelf:latest",
		SkippedMounts: []SkippedMount{{Destination: "/audiobooks", Type: "bind"}},
	})
	if pre == nil {
		t.Fatal("expected Audiobookshelf preconditions")
	}
	if pre.AddressPrompt != "" {
		t.Errorf("Audiobookshelf records no address — the field must not be offered, got %q", pre.AddressPrompt)
	}
}

// TestAppVersionCompatibilityBookStack: the ratchet the report flags — a
// :latest auto-update advances the schema, after which older images cannot run
// the database. Restoring into one must be refused, not warned about.
func TestAppVersionCompatibilityBookStack(t *testing.T) {
	p := ProfileFor("solidnerd/bookstack")
	if v := AppVersionCompatibility(p, "26.5.3", "25.9.1"); !v.Blocking {
		t.Fatalf("restoring a v26 backup into a v25 image must BLOCK, got %+v", v)
	}
	if v := AppVersionCompatibility(p, "25.9.1", "26.5.3"); v.Blocking || v.Warning == "" {
		t.Fatalf("restoring into a newer image must warn, not block, got %+v", v)
	}
	if v := AppVersionCompatibility(p, "26.5.3", "26.5.3"); v.Warning != "" {
		t.Fatalf("the same version must be silent, got %+v", v)
	}
}

// TestCalibreProfileOrdering is the ordering trap: every calibre-web image name
// also contains "calibre", so a registry that checked the shorter match first
// would give calibre-web the desktop app's profile and describe the wrong
// database as the one that migrates.
func TestCalibreProfileOrdering(t *testing.T) {
	for _, img := range []string{
		"crocodilestick/calibre-web-automated:latest",
		"lscr.io/linuxserver/calibre-web:latest",
	} {
		p := ProfileFor(img)
		if p == nil || p.Name != "calibre-web" {
			t.Fatalf("%q must match calibre-web, not the desktop app — got %+v", img, p)
		}
	}
	for _, img := range []string{"ghcr.io/linuxserver/calibre", "linuxserver/calibre:latest"} {
		p := ProfileFor(img)
		if p == nil || p.Name != "Calibre" {
			t.Fatalf("%q must match the desktop Calibre profile, got %+v", img, p)
		}
	}
}

// TestCalibreProfileContent: both variants must refuse network storage and block
// a downgrade. Calibre's own documentation is explicit that a library on a
// network filesystem corrupts, and the failure is silent for days.
func TestCalibreProfileContent(t *testing.T) {
	for _, img := range []string{"linuxserver/calibre", "crocodilestick/calibre-web-automated"} {
		p := ProfileFor(img)
		if p.OneWayMigration == "" {
			t.Errorf("%s: the database migrates forward on start — that must be declared", p.Name)
		}
		if p.PathEmbedding == "" {
			t.Errorf("%s: the library location is recorded in its data — that must be declared", p.Name)
		}
		for _, want := range []string{"/config", "/calibre-library"} {
			if !p.LocalOnlyPath(want) {
				t.Errorf("%s: %s must be marked local-disk-only", p.Name, want)
			}
		}
		// A media/ingest mount is NOT covered by the local-only rule: those live
		// on a NAS perfectly happily, and refusing them would block valid setups.
		if p.LocalOnlyPath("/Carti") || p.LocalOnlyPath("/cwa-book-ingest") {
			t.Errorf("%s: an ingest/media path must not be forced onto local disk", p.Name)
		}
	}
	// A newer library restored into an older Calibre cannot be opened.
	if v := AppVersionCompatibility(ProfileFor("linuxserver/calibre"), "7.16.0", "6.29.0"); !v.Blocking {
		t.Errorf("a Calibre downgrade must BLOCK, got %+v", v)
	}
}

// TestCommaFeedVariants is the tag-ordering trap: the SAME application ships
// against two completely different storage backends and only the image tag
// distinguishes them. Matching the shorter name first would put an
// embedded-database warning in front of a Postgres deployment that has no
// embedded database — a warning that is wrong is worse than none.
func TestCommaFeedVariants(t *testing.T) {
	pg := ProfileFor("athou/commafeed:latest-postgresql")
	if pg == nil || pg.Name != "CommaFeed" {
		t.Fatalf("the Postgres variant must have a profile, got %+v", pg)
	}
	if pg.EmbeddedDBWarning != "" {
		t.Error("the Postgres build keeps nothing in /commafeed/data — it must NOT warn about an embedded database")
	}
	if len(pg.LocalOnly) != 0 {
		t.Errorf("the Postgres build has no embedded database file, so no local-disk requirement: %v", pg.LocalOnly)
	}

	h2 := ProfileFor("athou/commafeed:latest")
	if h2 == nil || h2.EmbeddedDBWarning == "" {
		t.Fatalf("the default build keeps an embedded H2 database — that must be declared, got %+v", h2)
	}
	if !h2.LocalOnlyPath("/commafeed/data") {
		t.Error("the H2 file must be marked local-disk-only")
	}
	// The warning has to name the quiesce setting, since that is the thing
	// actually protecting the copy.
	for _, want := range []string{"H2", "pause"} {
		if !strings.Contains(h2.EmbeddedDBWarning, want) {
			t.Errorf("the embedded-database warning should mention %q, got %q", want, h2.EmbeddedDBWarning)
		}
	}

	// Both migrate forward and neither can be downgraded.
	for _, p := range []*AppProfile{pg, h2} {
		if v := AppVersionCompatibility(p, "5.6.0", "5.2.0"); !v.Blocking {
			t.Errorf("%s: a downgrade must BLOCK, got %+v", p.Name, v)
		}
	}
}

// TestEmbeddedDBSurfacedAtBackupTime: the warning is worth far more before a
// backup than before a restore, because it is the pause setting on that same
// page that it is telling the operator not to switch off.
func TestEmbeddedDBSurfacedAtBackupTime(t *testing.T) {
	labels := AutoHookLabels("athou/commafeed:latest", false)
	if len(labels) == 0 || !strings.Contains(labels[0], "H2") {
		t.Fatalf("the embedded-database warning must lead the backup-time presets, got %v", labels)
	}
	if got := AutoHookLabels("athou/commafeed:latest-postgresql", false); len(got) != 0 {
		t.Errorf("the Postgres build needs no such warning, got %v", got)
	}
	// An ordinary container's preset list is unchanged.
	if got := AutoHookLabels("nginx:alpine", false); len(got) != 0 {
		t.Errorf("an unprofiled image must add nothing, got %v", got)
	}
	// SQLite apps must NOT pick this up — they have a real snapshot path and a
	// warning here would contradict it.
	if got := AutoHookLabels("ghcr.io/advplyr/audiobookshelf:latest", false); len(got) != 0 {
		t.Errorf("a SQLite app has a consistent-snapshot path and must not be warned, got %v", got)
	}
}

// TestCredentialStoreClass pins which apps are declared as credential stores —
// apps whose archive is a higher-value target than anything it protects, because
// it holds the keys to OTHER systems.
//
// Both directions matter. Missing one leaves a fleet-root archive graded A with
// no reasons; adding an ordinary app cries wolf on a warning that should always
// mean something.
func TestCredentialStoreClass(t *testing.T) {
	for _, img := range []string{"fnsys/dockhand:latest", "ghcr.io/lukegus/termix:latest", "jwetzell/guacamole"} {
		p := ProfileFor(img)
		if p == nil || p.CredentialStore == "" {
			t.Fatalf("%q holds credentials for other systems — that must be declared, got %+v", img, p)
		}
		// The text has to say what a leak would GRANT, not merely that secrets
		// exist. "Contains secrets" is true of nearly every backup.
		if len(p.CredentialStore) < 60 {
			t.Errorf("%s: the declaration should describe what a decrypted archive grants, got %q", p.Name, p.CredentialStore)
		}
	}
	// Ordinary apps must not be in the class, however sensitive their own data.
	for _, img := range []string{"nginx:alpine", "ghcr.io/advplyr/audiobookshelf", "solidnerd/bookstack", "nextcloud"} {
		if p := ProfileFor(img); p != nil && p.CredentialStore != "" {
			t.Errorf("%q is not a credential store for other systems — declaring it would cry wolf", img)
		}
	}
}

// TestDockhandProfile covers the control-plane facts a move depends on.
func TestDockhandProfile(t *testing.T) {
	p := ProfileFor("fnsys/dockhand:latest")
	if p == nil || p.Name != "Dockhand" {
		t.Fatalf("expected the Dockhand profile, got %+v", p)
	}
	if p.OneWayMigration == "" {
		t.Error("Dockhand migrates its schema on start — a downgrade must be blocked")
	}
	// The three things that actually decide whether a moved Dockhand works.
	for _, want := range []string{"dials OUT", "reach every agent address", "socket"} {
		if !strings.Contains(p.ControlPlane, want) {
			t.Errorf("the control-plane note must cover %q, got %q", want, p.ControlPlane)
		}
	}
	// And the reassurance that stops a restore being deferred out of fear.
	if !strings.Contains(p.ControlPlane, "keep running") {
		t.Error("the note must say managed containers keep running during a restore")
	}

	pre := AppPreconditionsFor(&Manifest{Image: "fnsys/dockhand:latest"})
	if pre == nil {
		t.Fatal("Dockhand must produce restore preconditions")
	}
	if !strings.Contains(strings.Join(pre.Notes, " "), "dials OUT") {
		t.Errorf("the control-plane note must reach the restore dialog, got %v", pre.Notes)
	}
}

// TestCredentialStoreWarnedAtBackupTime: the warning belongs where write-only
// mode is CHOSEN. Afterwards it is only a regret.
func TestCredentialStoreWarnedAtBackupTime(t *testing.T) {
	labels := AutoHookLabels("fnsys/dockhand:latest", false)
	joined := strings.Join(labels, " ")
	if !strings.Contains(joined, "write-only") {
		t.Errorf("the backup-time presets must recommend write-only encryption, got %v", labels)
	}
	if !strings.Contains(joined, "High-value archive") {
		t.Errorf("the warning should lead with what it is, got %v", labels)
	}
	if got := AutoHookLabels("nginx:alpine", false); len(got) != 0 {
		t.Errorf("an ordinary image must add nothing, got %v", got)
	}
}

func TestIsNetworkFilesystem(t *testing.T) {
	for _, fs := range []string{"cifs", "nfs4", "NFS", "fuse.sshfs", " smbfs "} {
		if !IsNetworkFilesystem(fs) {
			t.Errorf("%q should be recognised as a network filesystem", fs)
		}
	}
	// Unknown must read as local: a guard that blocked on anything unfamiliar
	// would refuse valid restores on ordinary filesystems.
	for _, fs := range []string{"ext4", "btrfs", "zfs", "xfs", "overlay", "", "wormhole"} {
		if IsNetworkFilesystem(fs) {
			t.Errorf("%q must not be treated as networked", fs)
		}
	}
}

// TestCompareVersions is the reason parseMajor could not be reused: 2.36.0 and
// 2.26.0 share a major, and telling them apart is the entire point of the gate.
func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		ok   bool
	}{
		{"2.36.0", "2.26.0", 1, true},
		{"2.26.0", "2.36.0", -1, true},
		{"2.36.0", "2.36.0", 0, true},
		{"2.36", "2.36.0", 0, true}, // shorter compares zero-padded
		{"v2.37.1", "2.36.0", 1, true},
		{"2.36.0-beta.1", "2.36.0", 0, true}, // pre-release suffix ignored
		{"2.36.0+build5", "2.36.0", 0, true},
		{"10.0.0", "9.9.9", 1, true},
		{"latest", "2.36.0", 0, false},
		{"", "2.36.0", 0, false},
		{"2.36.0", "", 0, false},
	}
	for _, c := range cases {
		got, ok := compareVersions(c.a, c.b)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("compareVersions(%q,%q) = (%d,%v), want (%d,%v)", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
}

// TestAppVersionCompatibility locks the asymmetry: forward migration warns,
// a downgrade blocks, and an unreadable version on either side stays silent.
func TestAppVersionCompatibility(t *testing.T) {
	p := ProfileFor("audiobookshelf")

	down := AppVersionCompatibility(p, "2.36.0", "2.26.0")
	if !down.Blocking || down.Warning == "" {
		t.Fatalf("newer backup into older image must BLOCK, got %+v", down)
	}

	up := AppVersionCompatibility(p, "2.26.0", "2.36.0")
	if up.Blocking || up.Warning == "" {
		t.Fatalf("older backup into newer image must WARN only, got %+v", up)
	}

	if v := AppVersionCompatibility(p, "2.36.0", "2.36.0"); v.Blocking || v.Warning != "" {
		t.Fatalf("equal versions must be silent, got %+v", v)
	}
	// Fail-open: an image publishing no version label must never block.
	for _, tv := range []string{"", "latest"} {
		if v := AppVersionCompatibility(p, "2.36.0", tv); v.Blocking || v.Warning != "" {
			t.Fatalf("unreadable target version %q must be silent, got %+v", tv, v)
		}
	}
	// An app with no profile is never gated.
	if v := AppVersionCompatibility(nil, "2.36.0", "1.0.0"); v.Blocking {
		t.Fatal("a nil profile must never block")
	}
}

// TestAppPreconditionsFor proves the media path a backup deliberately SKIPPED is
// still surfaced — that mount is the one a new host is most likely to place
// differently, and it is absent from the archive precisely because it is large.
func TestAppPreconditionsFor(t *testing.T) {
	man := &Manifest{
		Image:   "ghcr.io/advplyr/audiobookshelf:latest",
		Volumes: []VolumeRef{{Destination: "/config", Type: "bind"}, {Destination: "/metadata", Type: "bind"}},
		SkippedMounts: []SkippedMount{
			{Destination: "/audiobooks", Type: "bind", Reason: "large bind"},
			{Destination: "/Carti", Type: "bind", Reason: "large bind"},
		},
	}
	pre := AppPreconditionsFor(man)
	if pre == nil {
		t.Fatal("audiobookshelf manifest must produce preconditions")
	}
	want := map[string]bool{"/metadata": true, "/audiobooks": true, "/Carti": true}
	for _, d := range pre.DataPaths {
		delete(want, d)
	}
	if len(want) != 0 {
		t.Errorf("missing data paths %v (got %v)", want, pre.DataPaths)
	}
	// /config is a local-only path, not a media path — listing it under
	// "reproduce these destinations" would muddle two different requirements.
	for _, d := range pre.DataPaths {
		if d == "/config" {
			t.Error("/config must not be listed as a media path")
		}
	}
	if len(pre.Notes) < 2 {
		t.Errorf("expected a path note and a local-disk note, got %v", pre.Notes)
	}

	if AppPreconditionsFor(&Manifest{Image: "nginx:alpine"}) != nil {
		t.Error("an unprofiled image must produce no preconditions")
	}
	if AppPreconditionsFor(nil) != nil {
		t.Error("a nil manifest must produce no preconditions")
	}
}

// TestGotifyProfile (F125). The interesting part is what Gotify is NOT: its
// tokens grant access to Gotify itself, not to other systems, so it must stay
// out of the credential-store class that Dockhand and Termix are in. Diluting
// that warning is how it stops meaning anything.
func TestGotifyProfile(t *testing.T) {
	p := ProfileFor("ghcr.io/gotify/server:latest")
	if p == nil || p.Name != "Gotify" {
		t.Fatalf("expected the Gotify profile, got %+v", p)
	}
	if p.OneWayMigration == "" {
		t.Error("an older Gotify cannot open a newer schema — a downgrade must be blocked")
	}
	if p.CredentialStore != "" {
		t.Error("Gotify's tokens reach Gotify, not other systems — it must not be in the credential-store class")
	}
	if len(p.Address) != 1 || p.Address[0].Kind != BindManual {
		t.Fatalf("the address lives in the client apps, not in Gotify — expected one manual binding, got %+v", p.Address)
	}
	b := p.Address[0]
	// Non-blocking: Gotify itself is perfectly reachable after a move; it is the
	// clients that are pointed elsewhere.
	if b.Blocking {
		t.Error("Gotify comes up fine at a new address — this is not a blocking precondition")
	}
	// The note must defuse the misdiagnosis, which is the whole reason it exists.
	for _, want := range []string{"survive a move", "re-issuing", "SERVER URL"} {
		if !strings.Contains(b.Note, want) {
			t.Errorf("the note should cover %q, got %q", want, b.Note)
		}
	}
	if !strings.Contains(b.Symptom, "looks like a broken token") {
		t.Errorf("the symptom must name the misdiagnosis, got %q", b.Symptom)
	}
}

// A newer-schema backup restored into an older Gotify image must be refused.
func TestGotifyDowngradeBlocked(t *testing.T) {
	p := ProfileFor("ghcr.io/gotify/server")
	if v := AppVersionCompatibility(p, "3.0.0", "2.6.3"); !v.Blocking {
		t.Fatalf("a Gotify downgrade must BLOCK, got %+v", v)
	}
	if v := AppVersionCompatibility(p, "2.6.3", "3.0.0"); v.Blocking || v.Warning == "" {
		t.Fatalf("restoring into a newer Gotify must warn, not block, got %+v", v)
	}
}

// TestHomepageCredentialStore (F129). Homepage is the case that shows the
// credential-store class is about REACH, not size: the app is trivial and has no
// database, but its services.yaml embeds working API keys for ~20 OTHER
// services. Contrast Gotify, deliberately excluded, whose tokens reach only
// Gotify.
func TestHomepageCredentialStore(t *testing.T) {
	p := ProfileFor("ghcr.io/gethomepage/homepage:latest")
	if p == nil || p.CredentialStore == "" {
		t.Fatalf("Homepage's config is a combined credential dump for other services — it must be in the class, got %+v", p)
	}
	// The two must stay on opposite sides of the line, or the class means nothing.
	if g := ProfileFor("ghcr.io/gotify/server"); g == nil || g.CredentialStore != "" {
		t.Error("Gotify's tokens reach only Gotify — it must stay OUT of the class")
	}
	// Its existing F114 behaviour must survive: the HTTP 400 is still blocking.
	if len(p.BlockingAddressSymptoms()) == 0 {
		t.Error("an unlisted host still returns HTTP 400 — that must remain a blocking precondition")
	}
}

// TestNoConfigMigrationWarnsBothWays (F129) is the mirror image of a one-way
// migration and needs the opposite handling: an app that migrates NOTHING is a
// risk in either direction, and neither direction fails loudly.
func TestNoConfigMigrationWarnsBothWays(t *testing.T) {
	p := ProfileFor("ghcr.io/gethomepage/homepage")
	if p.NoConfigMigration == "" {
		t.Fatal("Homepage does not migrate its config — that must be declared")
	}
	up := AppVersionCompatibility(p, "1.13.2", "1.20.0")
	if up.Blocking || up.Warning == "" {
		t.Fatalf("a newer target must WARN and never block, got %+v", up)
	}
	down := AppVersionCompatibility(p, "1.20.0", "1.13.2")
	if down.Blocking || down.Warning == "" {
		t.Fatalf("an older target must ALSO warn — the risk runs both ways — got %+v", down)
	}
	// The warning has to say what the failure looks like, because it is silent.
	if !strings.Contains(up.Warning, "quietly") && !strings.Contains(up.Warning, "stop working") {
		t.Errorf("the warning must describe the silent failure mode, got %q", up.Warning)
	}
	// Same version: nothing to say.
	if v := AppVersionCompatibility(p, "1.13.2", "1.13.2"); v.Warning != "" {
		t.Errorf("an identical version must be silent, got %+v", v)
	}
	// An app that DOES migrate keeps the one-way behaviour: downgrade blocks.
	if v := AppVersionCompatibility(ProfileFor("solidnerd/bookstack"), "26.5.3", "25.9.1"); !v.Blocking {
		t.Error("a one-way-migrating app must still BLOCK a downgrade")
	}
	// And an app with none of the three declarations stays silent. Radicale is
	// the case: its format is stable across a whole major version, so there is
	// nothing to gate in either direction. (This used to use Termix, which now
	// declares a version LOCK — see TestTermixVersionLock.)
	if v := AppVersionCompatibility(ProfileFor("tomsquest/docker-radicale"), "3.7.1", "3.7.6"); v.Warning != "" || v.Blocking {
		t.Errorf("an app declaring no migration property must say nothing, got %+v", v)
	}
}

// TestHomepagePreconditionsMentionPinning: the fix is to pin the image, which has
// to happen BEFORE the restore rather than after someone notices a blank panel.
func TestHomepagePreconditionsMentionPinning(t *testing.T) {
	pre := AppPreconditionsFor(&Manifest{Image: "ghcr.io/gethomepage/homepage:latest"})
	if pre == nil {
		t.Fatal("expected Homepage preconditions")
	}
	joined := strings.Join(pre.Notes, " ")
	if !strings.Contains(joined, "pin the target image") {
		t.Errorf("the note must tell the operator to pin, got %v", pre.Notes)
	}
	if !strings.Contains(joined, "400") {
		t.Errorf("the existing HTTP 400 precondition must survive, got %v", pre.Notes)
	}
}

// The socket directory must reach the client. Without it psql looks only in
// /var/run/postgresql, the preflight fails, and that is exit 3 — NOT the
// graceful "tools missing" fallback, so it fails the whole backup.
func TestPgSocketDirExport(t *testing.T) {
	if got := pgSocketDirExport(&EmbeddedDump{Socket: "/tmp"}); got != `export PGHOST='/tmp'; ` {
		t.Errorf("directory form: got %q", got)
	}
	// A profile naming the socket FILE (the shape mysql uses) is reduced to its
	// directory, because libpq wants the directory.
	if got := pgSocketDirExport(&EmbeddedDump{Socket: "/tmp/.s.PGSQL.5432"}); got != `export PGHOST='/tmp'; ` {
		t.Errorf("socket-file form: got %q", got)
	}
	// No socket declared = no export, so every existing profile is unchanged.
	for _, d := range []*EmbeddedDump{nil, {}, {Socket: "   "}} {
		if got := pgSocketDirExport(d); got != "" {
			t.Errorf("undeclared socket must emit nothing, got %q", got)
		}
	}
	// And it actually lands in the command the backup runs.
	cmd := embeddedDumpCommand(&EmbeddedDump{Engine: "postgres", User: "webapp", DBName: "webapp", Socket: "/tmp"}, nil)
	if len(cmd) != 3 || !strings.Contains(cmd[2], `export PGHOST='/tmp'; `) {
		t.Errorf("PGHOST missing from the postgres dump command: %v", cmd)
	}
}

// The IMPORT has to reach the same server the dump came from. The default shape
// is the official postgres image's — a `postgres` superuser over TCP — and a
// bundled server often has neither, so a profile that names its own role and
// socket must override both or the import fails on a healthy application.
func TestPgClientOptsForEmbeddedServer(t *testing.T) {
	// No profile: unchanged from the official-image behaviour.
	if got := pgClientOpts(nil); got != `-h 127.0.0.1 -U "${POSTGRES_USER:-postgres}"` {
		t.Errorf("default must stay TCP + POSTGRES_USER, got %q", got)
	}
	// WebApp's shape: its own role over its own trusted socket.
	got := pgClientOpts(&EmbeddedDump{Engine: "postgres", User: "webapp", Socket: "/tmp"})
	if got != `-h '/tmp' -U 'webapp'` {
		t.Errorf("embedded profile must use its socket and role, got %q", got)
	}
	// A declared user with no socket keeps TCP.
	if got := pgClientOpts(&EmbeddedDump{User: "guacamole"}); got != `-h 127.0.0.1 -U 'guacamole'` {
		t.Errorf("user without socket keeps TCP, got %q", got)
	}
	// And it lands in both commands that talk to the server.
	cmd, err := importCommand("postgres", &EmbeddedDump{User: "webapp", Socket: "/tmp"})
	if err != nil || len(cmd) != 3 || !strings.Contains(cmd[2], `-h '/tmp' -U 'webapp' -d postgres`) {
		t.Errorf("import command: %v (%v)", cmd, err)
	}
	term := pgTerminateConnectionsCmd([]string{"webapp"}, &EmbeddedDump{User: "webapp", Socket: "/tmp"})
	if len(term) != 3 || !strings.Contains(term[2], `-h '/tmp' -U 'webapp'`) {
		t.Errorf("terminate command must reach the same server: %v", term)
	}
}
