package backup

import (
	"encoding/json"
	"strings"
	"testing"

	"dockback/internal/crypto"
	"dockback/internal/store"
)

// F209 — a stack containing a write-only member is restorable, and a wrong key
// costs zero services.
//
// Before this, StackRestoreOptions had no field for the offline private key at
// all. So a stack whose database was write-only encrypted restored every app
// service, then stopped dead at the database with "supply the offline private
// key to restore" — a half-restored stack, and nowhere to put the key. The check
// now happens before service one is touched.

// woMember builds a stack member whose manifest is sealed to a real keypair,
// returning the private half that opens it.
func woMember(t *testing.T, service string) (*stackService, string) {
	t.Helper()
	pub, priv, err := crypto.NewBackupKeypair()
	if err != nil {
		t.Fatal(err)
	}
	man := &Manifest{
		Service:       service,
		TargetName:    service,
		WrappedKeyPub: "sealed-dek",
		BackupPubFP:   crypto.BackupPubFP(pub),
	}
	return &stackService{
		service: service,
		man:     man,
		backup:  &store.Backup{ID: "b-" + service, TargetName: service, Status: "success"},
	}, priv
}

func plainMember(service string) *stackService {
	man := &Manifest{Service: service, TargetName: service}
	return &stackService{
		service: service,
		man:     man,
		backup:  &store.Backup{ID: "b-" + service, TargetName: service, Status: "success"},
	}
}

// AC1 — no key supplied: refused, and the refusal names the member that needs
// one so the operator knows which recovery sheet to find.
func TestStackWriteOnlyRefusedWithoutAKey(t *testing.T) {
	db, _ := woMember(t, "db")
	set := map[string]*stackService{"app": plainMember("app"), "db": db}

	err := checkStackPrivateKey(set, "")
	if err == nil {
		t.Fatal("a stack with a write-only member must be refused when no key is supplied")
	}
	if !strings.Contains(err.Error(), `"db"`) {
		t.Errorf("the refusal must name the service that needs the key: %v", err)
	}
	if !strings.Contains(err.Error(), "write-only") {
		t.Errorf("and say why: %v", err)
	}
}

// AC1 — a key from a DIFFERENT keypair is refused the same way, and is reported
// as a wrong key rather than as corruption.
func TestStackWriteOnlyRefusedWithTheWrongKey(t *testing.T) {
	db, _ := woMember(t, "db")
	_, otherPriv, err := crypto.NewBackupKeypair()
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]*stackService{"app": plainMember("app"), "db": db}

	verr := checkStackPrivateKey(set, otherPriv)
	if verr == nil {
		t.Fatal("a key from another keypair must be refused")
	}
	if !strings.Contains(verr.Error(), "keypair") {
		t.Errorf("it must read as a wrong key, not a decryption failure: %v", verr)
	}
	// Junk in the field is a refusal too, never a panic or a pass.
	for _, junk := range []string{"not-base64!!", "c2hvcnQ=", "   "} {
		if e := checkStackPrivateKey(set, junk); e == nil {
			t.Errorf("junk key %q must not pass", junk)
		}
	}
}

// The correct key passes the pre-flight, for every write-only member at once.
func TestStackWriteOnlyAcceptsTheRightKey(t *testing.T) {
	db, priv := woMember(t, "db")
	// A second member sealed to the SAME keypair — which is what write-only mode
	// actually produces, since one instance keypair seals every backup.
	cache := &stackService{
		service: "cache",
		man:     &Manifest{Service: "cache", TargetName: "cache", WrappedKeyPub: "sealed-dek", BackupPubFP: db.man.BackupPubFP},
		backup:  &store.Backup{ID: "b-cache", TargetName: "cache", Status: "success"},
	}
	set := map[string]*stackService{"app": plainMember("app"), "db": db, "cache": cache}

	if err := checkStackPrivateKey(set, priv); err != nil {
		t.Fatalf("the right key must open every write-only member: %v", err)
	}
}

// A stack with no write-only member is unaffected, key or no key — the ordinary
// case must not gain a new way to fail.
func TestStackWithoutWriteOnlyMembersIsUnaffected(t *testing.T) {
	set := map[string]*stackService{"app": plainMember("app"), "db": plainMember("db")}
	if err := checkStackPrivateKey(set, ""); err != nil {
		t.Errorf("no write-only member means nothing to check: %v", err)
	}
	if err := checkStackPrivateKey(set, "an-irrelevant-key"); err != nil {
		t.Errorf("a key nobody needs is inert: %v", err)
	}
	if err := checkStackPrivateKey(map[string]*stackService{}, ""); err != nil {
		t.Errorf("an empty set checks nothing: %v", err)
	}
}

// The same wrong key must always name the same service — a map iteration order
// would otherwise report a different member each run.
func TestStackWriteOnlyRefusalIsDeterministic(t *testing.T) {
	a, _ := woMember(t, "aaa")
	z, _ := woMember(t, "zzz")
	set := map[string]*stackService{"zzz": z, "aaa": a}

	first := checkStackPrivateKey(set, "")
	if first == nil {
		t.Fatal("expected a refusal")
	}
	for i := 0; i < 20; i++ {
		if got := checkStackPrivateKey(set, ""); got.Error() != first.Error() {
			t.Fatalf("refusal is not deterministic: %q then %q", first, got)
		}
	}
	if !strings.Contains(first.Error(), `"aaa"`) {
		t.Errorf("the lowest-named member should be reported first, got %v", first)
	}
}

// The key is threaded to every member, so a member that needs it has it.
func TestStackServiceRestoreOptionsCarryThePrivateKey(t *testing.T) {
	db, priv := woMember(t, "db")
	got := stackServiceRestoreOptions(db, "n1", StackRestoreOptions{PrivateKey: priv})
	if got.PrivateKey != priv {
		t.Error("the offline key must reach the per-service restore, or the member still fails")
	}
	// Inert when none was given, exactly as before.
	if o := stackServiceRestoreOptions(db, "n1", StackRestoreOptions{}); o.PrivateKey != "" {
		t.Errorf("no key means no key, got %q", o.PrivateKey)
	}
}

// AC3 — the plan marks the write-only member, so the dialog can ask for the key
// before the operator commits rather than after three services are overwritten.
func TestStackPlanFlagsWriteOnlyMembers(t *testing.T) {
	pub, _, err := crypto.NewBackupKeypair()
	if err != nil {
		t.Fatal(err)
	}
	woMan, _ := json.Marshal(Manifest{Service: "db", TargetName: "db", WrappedKeyPub: "sealed", BackupPubFP: crypto.BackupPubFP(pub)})
	plainMan, _ := json.Marshal(Manifest{Service: "app", TargetName: "app"})
	rows := []*store.Backup{
		{ID: "b-db", Stack: "blog", TargetName: "db", Status: "success", CreatedAt: 200, ManifestJSON: string(woMan)},
		{ID: "b-app", Stack: "blog", TargetName: "app", Status: "success", CreatedAt: 100, ManifestJSON: string(plainMan)},
	}

	entries, blocked, perr := planStackFrom(rows, "blog", "")
	if perr != nil || blocked != "" {
		t.Fatalf("plan failed: %v / %q", perr, blocked)
	}
	byService := map[string]StackPlanEntry{}
	for _, e := range entries {
		byService[e.Service] = e
	}
	if !byService["db"].WriteOnly {
		t.Error("a write-only member must be flagged in the plan")
	}
	if byService["app"].WriteOnly {
		t.Error("an ordinary member must not be flagged")
	}

	// The flag has to survive serialization — the dialog reads it off the wire.
	js, _ := json.Marshal(byService["db"])
	if !strings.Contains(string(js), `"write_only":true`) {
		t.Errorf("write_only must be in the JSON: %s", js)
	}
	// omitempty: an ordinary member stays byte-identical to before this feature.
	js2, _ := json.Marshal(byService["app"])
	if strings.Contains(string(js2), "write_only") {
		t.Errorf("an ordinary plan row must not gain a field: %s", js2)
	}
}
