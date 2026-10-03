package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"dockback/internal/backup"
	"dockback/internal/config"
	"dockback/internal/egress"
	"dockback/internal/store"
)

// F207 — the audit record behind the dry run.
//
// The two properties worth locking in: the recorder must be cheap enough to sit
// on the dial path (so it writes once per host, not once per connection), and an
// observation must never be presented as safe to allow-list just because it was
// observed — the dial guard sees redirects, which is the attack the allow-list
// exists to stop.

func auditServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	s := &Server{
		store:  st,
		cfg:    &config.Config{EncryptionKey: key},
		engine: &backup.Engine{Store: st, Key: key, Log: func(string, string, string) {}},
	}
	t.Cleanup(func() { egress.SetObserver(nil); egress.SetAuditMode(false); egress.Configure(nil) })
	return s
}

func readAudit(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleEgressAudit(rec, httptest.NewRequest("GET", "/api/security/egress/audit", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("audit = %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func auditEntries(t *testing.T, s *Server) []egressAuditEntry {
	t.Helper()
	raw, _ := json.Marshal(readAudit(t, s)["entries"])
	var list []egressAuditEntry
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	return list
}

// AC1 — with audit mode on, a non-listed host is allowed through AND appears in
// the audit endpoint.
func TestAuditModeRecordsWhatWouldHaveBeenBlocked(t *testing.T) {
	s := auditServer(t)
	s.installEgressAudit()
	egress.Configure([]string{"allowed.example.com"})
	egress.SetAuditMode(true)

	if err := egress.Default().Enforce("nextcloud.example.net:443"); err != nil {
		t.Fatalf("audit mode must let the connection through: %v", err)
	}

	entries := auditEntries(t, s)
	if len(entries) != 1 {
		t.Fatalf("want one observation, got %+v", entries)
	}
	if entries[0].Host != "nextcloud.example.net" {
		t.Errorf("host = %q", entries[0].Host)
	}
	if entries[0].Count != 1 || entries[0].FirstSeen == 0 {
		t.Errorf("the observation should be timestamped and counted: %+v", entries[0])
	}
	if readAudit(t, s)["audit_mode"] != true {
		t.Error("the endpoint must report that the dry run is on")
	}
	// Nothing configured uses it, so it must NOT be presented as safe to allow.
	if entries[0].Configured {
		t.Error("a host no configured endpoint uses must be marked unconfigured")
	}
}

// AC2 — regression: with audit mode off the same host is refused and nothing is
// recorded. Enforcement must keep working exactly as before.
func TestEnforcementStillBlocksWithAuditOff(t *testing.T) {
	s := auditServer(t)
	s.installEgressAudit()
	egress.Configure([]string{"allowed.example.com"})
	egress.SetAuditMode(false)

	if err := egress.Default().Enforce("nextcloud.example.net:443"); err == nil {
		t.Fatal("with audit mode off the host must be refused")
	}
	if entries := auditEntries(t, s); len(entries) != 0 {
		t.Errorf("enforcement must not populate the audit record: %+v", entries)
	}
}

// A host that IS a configured endpoint is marked as such — that is what makes
// "add all" safe to offer, and what tells the operator this one is a real gap in
// their list rather than something to investigate.
func TestObservationsAreAnnotatedAgainstConfiguredEndpoints(t *testing.T) {
	s := auditServer(t)
	s.installEgressAudit()
	egress.Configure([]string{"allowed.example.com"})
	egress.SetAuditMode(true)

	// A real destination the operator forgot to list.
	sealed, err := s.encryptDestConfig(map[string]string{"url": "https://cloud.example.org/remote.php/dav"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.CreateDestination(&store.Destination{
		ID: "d1", Name: "Nextcloud", Type: "nextcloud", Enabled: true, ConfigEnc: sealed,
	}); err != nil {
		t.Fatal(err)
	}

	_ = egress.Default().Enforce("cloud.example.org:443") // configured
	_ = egress.Default().Enforce("evil.example.net:443")  // arrived some other way

	byHost := map[string]egressAuditEntry{}
	for _, e := range auditEntries(t, s) {
		byHost[e.Host] = e
	}
	known, unknown := byHost["cloud.example.org"], byHost["evil.example.net"]
	if !known.Configured {
		t.Errorf("a configured destination's host must be recognised: %+v", known)
	}
	if !strings.Contains(known.Source, "Nextcloud") {
		t.Errorf("and named: %+v", known)
	}
	if unknown.Configured {
		t.Error("a host nothing configured uses must not be marked configured — it may be a redirect")
	}
	if readAudit(t, s)["unconfigured"] != float64(1) {
		t.Errorf("the count of unrecognised hosts must be reported: %v", readAudit(t, s)["unconfigured"])
	}
}

// An observation resolved by adding the host to the list shows as allowed now,
// rather than sending the operator after a problem they already fixed.
func TestResolvedObservationsAreMarkedAllowedNow(t *testing.T) {
	s := auditServer(t)
	s.installEgressAudit()
	egress.Configure([]string{"allowed.example.com"})
	egress.SetAuditMode(true)
	_ = egress.Default().Enforce("forgot.example.net:443")

	if auditEntries(t, s)[0].AllowedNow {
		t.Fatal("it is not allowed yet")
	}
	// The operator adds it and applies.
	egress.Configure([]string{"allowed.example.com", "forgot.example.net"})
	if !auditEntries(t, s)[0].AllowedNow {
		t.Error("once allow-listed, the observation must show as resolved")
	}
}

// The recorder sits on the dial path: it must write on the FIRST sighting only,
// and must dedup by host however many connections are made.
func TestRecorderDedupsAndPersistsOnce(t *testing.T) {
	s := auditServer(t)
	s.installEgressAudit()
	egress.Configure([]string{"allowed.example.com"})
	egress.SetAuditMode(true)

	for i := 0; i < 50; i++ {
		_ = egress.Default().Enforce("repeat.example.net:443")
	}
	entries := auditEntries(t, s)
	if len(entries) != 1 {
		t.Fatalf("50 connections to one host is one observation, got %d", len(entries))
	}
	if entries[0].Count != 50 {
		t.Errorf("count = %d, want 50", entries[0].Count)
	}

	// It survives a restart — an operator halfway through reviewing an audit must
	// not lose it, and the counts are flushed when the audit is read.
	s2 := &Server{store: s.store, cfg: s.cfg, engine: s.engine}
	s2.installEgressAudit()
	after := auditEntries(t, s2)
	if len(after) != 1 || after[0].Host != "repeat.example.net" || after[0].Count != 50 {
		t.Errorf("observations must survive a restart: %+v", after)
	}
}

// The record is bounded: observations partly come from redirects, so the set is
// not purely operator-controlled.
func TestAuditRecordIsBounded(t *testing.T) {
	s := auditServer(t)
	s.installEgressAudit()
	egress.Configure([]string{"allowed.example.com"})
	egress.SetAuditMode(true)

	for i := 0; i < maxEgressAuditHosts+40; i++ {
		s.egressAudit.record(fmt.Sprintf("host-%d.example.net", i))
	}
	if got := len(s.egressAudit.list()); got > maxEgressAuditHosts {
		t.Fatalf("record grew to %d, cap is %d", got, maxEgressAuditHosts)
	}
	if readAudit(t, s)["truncated"] != true {
		t.Error("a truncated record must say so rather than reading as complete")
	}
	// An absurd hostname is not a hostname anybody configured.
	before := len(s.egressAudit.list())
	s.egressAudit.record(strings.Repeat("x", maxEgressAuditHostLen+1) + ".example.net")
	if len(s.egressAudit.list()) != before {
		t.Error("an over-long host must be dropped")
	}
}

// Clearing starts a fresh audit — the natural thing to do after fixing the list.
func TestAuditClear(t *testing.T) {
	s := auditServer(t)
	s.installEgressAudit()
	egress.Configure([]string{"allowed.example.com"})
	egress.SetAuditMode(true)
	_ = egress.Default().Enforce("gone.example.net:443")
	if len(auditEntries(t, s)) != 1 {
		t.Fatal("setup")
	}

	rec := httptest.NewRecorder()
	s.handleEgressAuditClear(rec, httptest.NewRequest("POST", "/api/security/egress/audit/clear", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("clear = %d: %s", rec.Code, rec.Body.String())
	}
	if got := auditEntries(t, s); len(got) != 0 {
		t.Errorf("the record should be empty, got %+v", got)
	}
	if !auditHas(t, s, "egress.audit.clear") {
		t.Error("clearing the record must be audited")
	}
}

// The mode survives a restart. One that reset to enforcing would silently start
// blocking a list the operator was still testing.
func TestAuditModeSurvivesRestart(t *testing.T) {
	s := auditServer(t)
	if err := s.store.SetSetting(egressAuditModeSetting, "true"); err != nil {
		t.Fatal(err)
	}
	egress.SetAuditMode(false)
	s.installEgressAudit()
	if !egress.AuditMode() {
		t.Error("a persisted audit mode must be restored at boot")
	}

	_ = s.store.SetSetting(egressAuditModeSetting, "false")
	s.installEgressAudit()
	if egress.AuditMode() {
		t.Error("and cleared when it was turned off")
	}
}

// The toggle goes through the settings allow-list like every other knob.
func TestEgressAuditSettingIsCoerced(t *testing.T) {
	if v, ok := coerceSetting("security.egress_audit", "true"); !ok || v != "true" {
		t.Errorf("true = %q (ok=%v)", v, ok)
	}
	if v, _ := coerceSetting("security.egress_audit", "yes-please"); v != "false" {
		t.Errorf("anything that is not \"true\" must be false, got %q", v)
	}
}
