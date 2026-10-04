package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"dockback/internal/storage"
	"dockback/internal/store"
)

// F199 — a decrypted export needs a fresh password, and the ticket that carries
// that decision is worth nothing twice.
//
// The asymmetry this closes: revealing the master key demanded a password and a
// TOTP code, while downloading a DECRYPTED archive — more secret material than
// the key that opens it — needed only a session cookie.

// exportServer is stepUpServer plus one backup row to export, and an engine wired
// to a real (empty) local backend.
//
// The engine matters: everything F199 adds happens BEFORE decryption, but the
// handler goes on to call DecryptTo, and a half-built engine panics there rather
// than failing — which would hide the gate's behaviour behind a crash. With a
// store and a local backend it reaches a clean "archive not found" instead, so
// the assertions below are about the gate and nothing else.
func exportServer(t *testing.T) (*Server, string) {
	t.Helper()
	s := stepUpServer(t)
	const id = "bk-export-1"
	if err := s.store.CreateBackup(&store.Backup{
		ID: id, NodeID: "n1", TargetName: "app", Status: "success", CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	be, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.engine.Store = s.store
	s.engine.Storage = be
	s.engine.Log = func(string, string, string) {}
	return s, id
}

func grantPost(s *Server, id, body string) (*httptest.ResponseRecorder, map[string]any) {
	r := httptest.NewRequest("POST", "/api/backups/"+id+"/export-grant", strings.NewReader(body))
	r.SetPathValue("id", id)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "sess1"})
	rec := httptest.NewRecorder()
	s.handleExportGrant(rec, r)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func downloadGet(s *Server, id, ticket, session string) *httptest.ResponseRecorder {
	url := "/api/backups/" + id + "/download"
	if ticket != "" {
		url += "?ticket=" + ticket
	}
	r := httptest.NewRequest("GET", url, nil)
	r.SetPathValue("id", id)
	if session != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	}
	rec := httptest.NewRecorder()
	s.handleDownload(rec, r)
	return rec
}

// AC1 + AC2 + AC3: no ticket is refused; a wrong password is refused with the
// step-up marker; a correct one yields a ticket that works exactly once.
func TestExportRequiresStepUpAndTicketIsSingleUse(t *testing.T) {
	s, id := exportServer(t)

	// AC1 — a live session alone no longer exports anything.
	if rec := downloadGet(s, id, "", "sess1"); rec.Code != http.StatusForbidden {
		t.Fatalf("download without a ticket = %d, want 403", rec.Code)
	}

	// AC2 — a wrong password is refused, and the response tells the UI to prompt
	// rather than looking like a generic failure.
	rec, out := grantPost(s, id, `{"password":"wrong"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("grant with a wrong password = %d, want 401", rec.Code)
	}
	if out["step_up_required"] != true {
		t.Errorf("the 401 must carry step_up_required so the prompt appears: %v", out)
	}
	if out["ticket"] != nil {
		t.Fatal("a failed grant must not issue a ticket")
	}

	// AC3 — a correct password issues a ticket.
	rec, out = grantPost(s, id, `{"password":"`+stepUpTestPass+`","purpose":"download"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("grant with the right password = %d, want 200", rec.Code)
	}
	ticket, _ := out["ticket"].(string)
	if ticket == "" {
		t.Fatal("a successful grant must return a ticket")
	}

	// The ticket is stored only as a HASH — a database read must not yield a
	// working download URL.
	if v, _ := s.store.GetSetting("export_ticket:"+ticket, ""); v != "" {
		t.Error("the raw ticket must never be a settings key")
	}
	if v, _ := s.store.GetSetting(exportTicketKey(ticket), ""); v == "" {
		t.Error("the hashed ticket should be stored")
	}

	// First use passes the gate (the archive itself is absent in this fixture,
	// which is past the point F199 governs); second use is refused outright.
	if rec := downloadGet(s, id, ticket, "sess1"); rec.Code == http.StatusForbidden {
		t.Fatalf("a valid ticket must pass the gate, got 403")
	}
	if v, _ := s.store.GetSetting(exportTicketKey(ticket), ""); v != "" {
		t.Error("a redeemed ticket must be spent immediately")
	}
	if rec := downloadGet(s, id, ticket, "sess1"); rec.Code != http.StatusForbidden {
		t.Fatalf("second use of the same ticket = %d, want 403 — tickets are single-use", rec.Code)
	}
}

// AC4 — exactly one audit row per successful export, and the grant is recorded
// separately so "asked" and "took" are distinguishable in the trail.
func TestExportIsAudited(t *testing.T) {
	s, id := exportServer(t)

	countAudit := func(action string) int {
		es, err := s.store.ListAudit(1000)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range es {
			if e.Action == action {
				n++
			}
		}
		return n
	}

	_, out := grantPost(s, id, `{"password":"`+stepUpTestPass+`"}`)
	ticket, _ := out["ticket"].(string)
	if rec := downloadGet(s, id, ticket, "sess1"); rec.Code == http.StatusForbidden {
		t.Fatalf("a valid ticket must pass the gate, got 403")
	}

	if got := countAudit("export.download"); got != 1 {
		t.Errorf("export.download audit rows = %d, want exactly 1", got)
	}
	if got := countAudit("export.grant"); got != 1 {
		t.Errorf("export.grant audit rows = %d, want exactly 1", got)
	}
	// A refused attempt writes no export.download row — the trail must not claim
	// data left the building when it did not.
	downloadGet(s, id, "not-a-real-ticket", "sess1")
	if got := countAudit("export.download"); got != 1 {
		t.Errorf("a refused download must not be audited as one, got %d rows", got)
	}
}

// A ticket is bound to its purpose and its backup: a whole-archive grant cannot
// be replayed against the single-file extract, or against a different backup.
func TestExportTicketIsBoundToPurposeAndTarget(t *testing.T) {
	s, id := exportServer(t)

	// Minted for the whole archive…
	_, out := grantPost(s, id, `{"password":"`+stepUpTestPass+`","purpose":"download"}`)
	ticket, _ := out["ticket"].(string)

	// …and refused for the extract, which is a different exposure.
	r := httptest.NewRequest("GET", "/api/backups/"+id+"/extract?path=x&ticket="+ticket, nil)
	r.SetPathValue("id", id)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "sess1"})
	rec := httptest.NewRecorder()
	if s.requireExportTicket(rec, r, exportPurposeExtract, id) {
		t.Error("a download ticket must not authorise an extract")
	}

	// A ticket for one backup must not open another.
	_, out = grantPost(s, id, `{"password":"`+stepUpTestPass+`"}`)
	ticket, _ = out["ticket"].(string)
	if rec := downloadGet(s, "some-other-backup", ticket, "sess1"); rec.Code != http.StatusForbidden {
		t.Errorf("a ticket for %s must not open another backup, got %d", id, rec.Code)
	}
}

// A ticket lands in browser history and possibly a proxy log, so it is bound to
// the session that asked for it: recovered elsewhere, it opens nothing.
func TestExportTicketIsBoundToTheSession(t *testing.T) {
	s, id := exportServer(t)
	_, out := grantPost(s, id, `{"password":"`+stepUpTestPass+`"}`)
	ticket, _ := out["ticket"].(string)

	// A second, valid session for the same user presents the leaked ticket.
	u, err := s.store.GetUserByName("admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.CreateSession("sess2", u.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if rec := downloadGet(s, id, ticket, "sess2"); rec.Code != http.StatusForbidden {
		t.Fatalf("a ticket used by a different session = %d, want 403", rec.Code)
	}
}

// An expired ticket is refused — and spent, so the deadline cannot be waited out
// and retried.
func TestExportTicketExpires(t *testing.T) {
	s, id := exportServer(t)
	_, out := grantPost(s, id, `{"password":"`+stepUpTestPass+`"}`)
	ticket, _ := out["ticket"].(string)

	// Backdate it past its deadline.
	g := exportGrant{Purpose: exportPurposeDownload, Target: id, SessionFP: sha256Hex("sess1"), Expires: time.Now().Add(-time.Second).Unix()}
	_ = s.store.SetSetting(exportTicketKey(ticket), g.encode())

	if rec := downloadGet(s, id, ticket, "sess1"); rec.Code != http.StatusForbidden {
		t.Fatalf("an expired ticket = %d, want 403", rec.Code)
	}
	if v, _ := s.store.GetSetting(exportTicketKey(ticket), ""); v != "" {
		t.Error("a rejected ticket must still be spent, not left for another try")
	}
}

// Single-use has to hold under concurrency, which is the whole reason the
// redemption is one transaction rather than a read followed by a delete: two
// requests arriving together could otherwise both pass the read.
func TestExportTicketSurvivesConcurrentRedemption(t *testing.T) {
	s, id := exportServer(t)
	_, out := grantPost(s, id, `{"password":"`+stepUpTestPass+`"}`)
	ticket, _ := out["ticket"].(string)

	const racers = 8
	var wg sync.WaitGroup
	codes := make([]int, racers)
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func(i int) {
			defer wg.Done()
			codes[i] = downloadGet(s, id, ticket, "sess1").Code
		}(i)
	}
	wg.Wait()

	passed := 0
	for _, c := range codes {
		if c != http.StatusForbidden {
			passed++
		}
	}
	if passed != 1 {
		t.Errorf("%d of %d concurrent redemptions passed the gate, want exactly 1", passed, racers)
	}
}

// Expired tickets are swept so an abandoned download does not leave a settings
// row behind for good.
func TestPruneExportTickets(t *testing.T) {
	s, id := exportServer(t)

	live := randToken()
	_ = s.store.SetSetting(exportTicketKey(live),
		exportGrant{Purpose: exportPurposeDownload, Target: id, Expires: time.Now().Add(time.Hour).Unix()}.encode())
	dead := randToken()
	_ = s.store.SetSetting(exportTicketKey(dead),
		exportGrant{Purpose: exportPurposeDownload, Target: id, Expires: time.Now().Add(-time.Hour).Unix()}.encode())

	s.pruneExportTickets()

	if v, _ := s.store.GetSetting(exportTicketKey(live), ""); v == "" {
		t.Error("a live ticket must survive the sweep")
	}
	if v, _ := s.store.GetSetting(exportTicketKey(dead), ""); v != "" {
		t.Error("an expired ticket should be swept")
	}
}

// Step 25: one download for a whole stack, behind the same step-up and one-shot
// ticket as a single backup's.
func TestStackDownloadIsGuardedLikeABackupDownload(t *testing.T) {
	s := stackKeyServer(t)
	mkStackMember(t, s, "b-app", "n1", "arr", "sonarr", "arr-sonarr")

	grant := func(project string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/nodes/n1/stacks/"+project+"/export-grant", strings.NewReader("{}"))
		r.SetPathValue("id", "n1")
		r.SetPathValue("project", project)
		rec := httptest.NewRecorder()
		s.handleStackExportGrant(rec, r)
		return rec
	}
	if rec := grant("nothing-here"); rec.Code != http.StatusNotFound {
		t.Errorf("a stack with no backups is refused before any password prompt: %d", rec.Code)
	}
	if rec := grant("arr"); rec.Code != http.StatusUnauthorized {
		t.Errorf("the grant must not be issued without a fresh password: %d %s", rec.Code, rec.Body.String())
	}

	r := httptest.NewRequest("GET", "/api/nodes/n1/stacks/arr/download?ticket=forged", nil)
	r.SetPathValue("id", "n1")
	r.SetPathValue("project", "arr")
	rec := httptest.NewRecorder()
	s.handleStackDownload(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a download without a valid ticket is refused before a byte is decrypted: %d", rec.Code)
	}
}

// Step 28: a node's evidence is exported like a backup — behind a fresh
// password and a one-shot ticket.
func TestEvidenceIsGuardedLikeAnExport(t *testing.T) {
	s := stackKeyServer(t)
	call := func(handler http.HandlerFunc, method, node string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/nodes/"+node+"/evidence", strings.NewReader("{}"))
		r.SetPathValue("id", node)
		rec := httptest.NewRecorder()
		handler(rec, r)
		return rec
	}
	if rec := call(s.handleEvidenceGrant, "POST", "no-such-node"); rec.Code != http.StatusNotFound {
		t.Errorf("an unknown node is refused before any password prompt: %d", rec.Code)
	}
	if rec := call(s.handleEvidence, "GET", "n1"); rec.Code != http.StatusForbidden {
		t.Errorf("evidence without a ticket must be refused: %d", rec.Code)
	}
}
