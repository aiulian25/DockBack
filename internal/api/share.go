package api

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"dockback/internal/crypto"
)

// Signed, expiring, revocable share links for the DR runbook (F59). The token is
// `id.exp.sig` where sig = HMAC-SHA256("runbook-share|id|exp") under the master key
// (via crypto.SignManifest — the same vetted, constant-time, master-derived HMAC
// used for manifest signatures, domain-separated by the "runbook-share" prefix).
// Verification is stateless (HMAC + expiry); a small issued-links list adds early
// revocation. The rendered page is a REDACTED, read-only snapshot — no master-key
// fingerprint, no location details.

const (
	shareSettingKey = "share.runbook"
	maxShareLinks   = 20
	shareMaxHours   = 168 // 7 days
)

// shareLink is one issued runbook link, persisted (bounded) so it can be revoked.
type shareLink struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Exp     int64  `json:"exp"`
	Revoked bool   `json:"revoked"`
	Viewed  bool   `json:"viewed"` // first-view audited (dedup so views don't spam the log)
}

// signShare / shareSigValid bind an (id, exp) pair to the master key.
func signShare(id string, exp int64, key []byte) string {
	return crypto.SignManifest([]byte(fmt.Sprintf("runbook-share|%s|%d", id, exp)), key)
}
func shareSigValid(id string, exp int64, sig string, key []byte) bool {
	return crypto.VerifyManifest([]byte(fmt.Sprintf("runbook-share|%s|%d", id, exp)), key, sig)
}

func (s *Server) loadShareLinks() []shareLink {
	var links []shareLink
	if raw, _ := s.store.GetSetting(shareSettingKey, ""); raw != "" {
		_ = json.Unmarshal([]byte(raw), &links)
	}
	return links
}

// saveShareLinks persists the list newest-first, bounded to the newest
// maxShareLinks — EXCEPT that a revoked link is retained until it expires.
//
// Revocation is enforced by this list; the token itself verifies statelessly
// (HMAC + expiry). Dropping a revoked record therefore does not just lose a
// row, it silently RE-ENABLES the link for the rest of its lifetime, and
// leaves no id to revoke a second time.
func (s *Server) saveShareLinks(links []shareLink) {
	now := time.Now().Unix()
	// Forget anything already expired: the expiry check in handleSharedRunbook
	// refuses it anyway, so it no longer needs a tombstone.
	live := make([]shareLink, 0, len(links))
	for _, l := range links {
		if l.Exp > now {
			live = append(live, l)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Created > live[j].Created })
	if len(live) > maxShareLinks {
		kept := live[:maxShareLinks]
		// Carry every revoked-but-unexpired record past the cap.
		for _, l := range live[maxShareLinks:] {
			if l.Revoked {
				kept = append(kept, l)
			}
		}
		live = kept
	}
	b, _ := json.Marshal(live)
	_ = s.store.SetSetting(shareSettingKey, string(b))
}

// handleShareRunbook mints a signed, expiring share link (F59).
func (s *Server) handleShareRunbook(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Hours int `json:"hours"`
	}
	_ = readJSON(r, &body)
	h := body.Hours
	if h < 1 {
		h = 1
	}
	if h > shareMaxHours {
		h = shareMaxHours
	}
	active := 0
	for _, l := range s.loadShareLinks() {
		if !l.Revoked && l.Exp > time.Now().Unix() {
			active++
		}
	}
	if active >= maxShareLinks {
		errJSON(w, http.StatusConflict,
			"too many active share links — revoke one first (a revoked link is kept until it expires so it can never come back)")
		return
	}
	id := randToken()[:16] // 64-bit unique id (the HMAC is the real gate)
	exp := time.Now().Add(time.Duration(h) * time.Hour).Unix()
	sig := signShare(id, exp, s.cfg.EncryptionKey)

	s.saveShareLinks(append(s.loadShareLinks(), shareLink{ID: id, Created: time.Now().Unix(), Exp: exp}))
	_ = s.store.Audit(userFrom(r), "runbook.share", id, fmt.Sprintf("hours=%d expires=%d", h, exp))

	writeJSON(w, http.StatusOK, map[string]any{
		"url": fmt.Sprintf("/share/runbook/%s.%d.%s", id, exp, sig),
		"id":  id,
		"exp": exp,
	})
}

// handleShareRunbookList lists issued links (metadata only — never the signature,
// so a link can't be reconstructed from this endpoint).
func (s *Server) handleShareRunbookList(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	type view struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Exp     int64  `json:"exp"`
		Revoked bool   `json:"revoked"`
		Expired bool   `json:"expired"`
	}
	out := []view{}
	for _, l := range s.loadShareLinks() {
		out = append(out, view{ID: l.ID, Created: l.Created, Exp: l.Exp, Revoked: l.Revoked, Expired: l.Exp < now})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleShareRunbookRevoke revokes a link early (F59).
func (s *Server) handleShareRunbookRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	links := s.loadShareLinks()
	found := false
	for i := range links {
		if links[i].ID == id {
			links[i].Revoked = true
			found = true
		}
	}
	if !found {
		errJSON(w, http.StatusNotFound, "share link not found")
		return
	}
	s.saveShareLinks(links)
	_ = s.store.Audit(userFrom(r), "runbook.share.revoke", id, "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// parseShareToken splits `id.exp.sig`. Each part is dot-free by construction.
func parseShareToken(tok string) (id string, exp int64, sig string, ok bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
		return "", 0, "", false
	}
	e, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", 0, "", false
	}
	return parts[0], e, parts[2], true
}

// handleSharedRunbook is the ONE unauthenticated dynamic route (F59): it verifies a
// signed, unexpired, non-revoked token and renders a REDACTED, read-only runbook.
// Every failure mode (expired / revoked / tampered / malformed) returns an
// indistinguishable 404, so the endpoint reveals nothing about a token's fate.
func (s *Server) handleSharedRunbook(w http.ResponseWriter, r *http.Request) {
	// Never cache or index a shared (albeit redacted) recovery plan.
	w.Header().Set("Cache-Control", "no-store, max-age=0")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Referrer-Policy", "no-referrer")

	if !shareRL.allow(s.clientIP(r), 30, 30) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	id, exp, sig, ok := parseShareToken(r.PathValue("token"))
	if !ok || exp < time.Now().Unix() || !shareSigValid(id, exp, sig, s.cfg.EncryptionKey) {
		http.NotFound(w, r)
		return
	}
	// Early-revocation check, fail-closed: a token whose record is not in the
	// issued list is treated as revoked. The list now retains every revoked,
	// unexpired link, so "not present" can only mean an id this instance never
	// issued (or issued under a database that has since been replaced) — and a
	// link nobody can revoke must not be a link that works.
	known := false
	for _, l := range s.loadShareLinks() {
		if l.ID != id {
			continue
		}
		if l.Revoked {
			http.NotFound(w, r)
			return
		}
		known = true
		break
	}
	if !known {
		http.NotFound(w, r)
		return
	}

	s.auditFirstShareView(id, s.clientIP(r))
	renderSharedRunbook(w, redactRunbook(s.buildRunbook()), exp)
}

// auditFirstShareView records the FIRST view of a link once (dedup via the Viewed
// flag), so a monitored/scraped link doesn't flood the audit log.
func (s *Server) auditFirstShareView(id, ip string) {
	links := s.loadShareLinks()
	for i := range links {
		if links[i].ID == id && !links[i].Viewed {
			links[i].Viewed = true
			s.saveShareLinks(links)
			_ = s.store.Audit("share:"+id, "runbook.share.viewed", id, "ip="+ip)
			return
		}
	}
}

// redactRunbook strips secrets from the runbook before it leaves the authenticated
// boundary (F59): the master-key fingerprint + posture, internal node ids, and every
// per-copy Detail (which can carry paths/stall reasons). Service/restore-order data
// — the point of the runbook — is kept.
func redactRunbook(r runbookResp) runbookResp {
	r.Key = runbookKey{} // no fingerprint, no ephemeral/ack posture
	for i := range r.Nodes {
		r.Nodes[i].NodeID = "" // internal id — not needed by a helper
		for j := range r.Nodes[i].Services {
			svc := &r.Nodes[i].Services[j]
			for k := range svc.Locations {
				svc.Locations[k].Detail = ""
			}
		}
	}
	return r
}

// renderSharedRunbook writes the redacted runbook as self-contained, print-friendly
// HTML with NO JavaScript. html/template auto-escapes every value, so service names,
// image refs, etc. cannot inject markup.
func renderSharedRunbook(w http.ResponseWriter, rb runbookResp, exp int64) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := struct {
		RB        runbookResp
		ExpiresAt string
	}{RB: rb, ExpiresAt: time.Unix(exp, 0).UTC().Format("2006-01-02 15:04 UTC")}
	if err := sharedRunbookTmpl.Execute(w, data); err != nil {
		// Headers may be partially written; nothing more we can safely do.
		return
	}
}

func shareDateShort(ts int64) string {
	if ts <= 0 {
		return "never"
	}
	return time.Unix(ts, 0).UTC().Format("2006-01-02 15:04 UTC")
}

func shareLocs(locs []runbookLoc) string {
	if len(locs) == 0 {
		return "local"
	}
	parts := make([]string, 0, len(locs))
	for _, l := range locs {
		s := l.Name
		if l.Status == "failed" {
			s += " (FAILED)"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

var sharedRunbookTmpl = template.Must(template.New("runbook").Funcs(template.FuncMap{
	"date":  shareDateShort,
	"locs":  shareLocs,
	"join":  func(v []string) string { return strings.Join(v, ", ") },
	"plus1": func(i int) int { return i + 1 },
}).Parse(sharedRunbookHTML))

// sharedRunbookHTML mirrors the section order of Recovery.tsx's toMarkdown, minus
// the redacted fields. Self-contained (inline CSS, print-friendly), no scripts.
const sharedRunbookHTML = `<!doctype html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<title>DockBack Disaster Recovery Runbook (shared)</title>
<style>
  :root { color-scheme: light; }
  body { font: 15px/1.55 -apple-system, Segoe UI, Roboto, Helvetica, Arial, sans-serif; color: #1a1a1a; background: #fff; max-width: 820px; margin: 2rem auto; padding: 0 1.25rem; }
  h1 { font-size: 1.6rem; margin: 0 0 .25rem; }
  h2 { font-size: 1.2rem; margin: 1.6rem 0 .5rem; border-bottom: 1px solid #e2e2e2; padding-bottom: .25rem; }
  h3 { font-size: 1.02rem; margin: 1.1rem 0 .35rem; }
  .meta { color: #666; font-size: .85rem; margin-bottom: 1rem; }
  .banner { background: #f4f6f8; border: 1px solid #e2e6ea; border-radius: 6px; padding: .6rem .8rem; font-size: .85rem; color: #555; margin: .5rem 0 1rem; }
  ul { margin: .25rem 0 .75rem 1.25rem; padding: 0; }
  li { margin: .15rem 0; }
  code { background: #f2f2f2; border-radius: 3px; padding: .05rem .3rem; font-size: .85em; word-break: break-all; }
  .svc { margin: .5rem 0 .75rem; }
  .svc .sub { color: #555; font-size: .88rem; margin: .1rem 0 .1rem 1.25rem; }
  .warn { color: #a15c00; }
  @media print { body { margin: 0; max-width: none; } h2 { break-after: avoid; } .svc { break-inside: avoid; } }
</style>
</head><body>
<h1>DockBack Disaster Recovery Runbook</h1>
<div class="meta">Generated {{date .RB.GeneratedAt}} · DockBack {{.RB.AppVersion}} · shared copy, valid until {{.ExpiresAt}}</div>
<div class="banner">This is a redacted, read-only copy of the recovery plan. Secrets (the encryption key, storage locations' internal details) are omitted. You still need the master encryption key — held by the administrator — to actually restore.</div>

<h2>1. Before you begin</h2>
<h3>Control plane (DockBack itself)</h3>
<ul>
{{if .RB.AppBackup.Exists}}<li>Newest application backup: {{date .RB.AppBackup.NewestAt}} ({{.RB.AppBackup.Count}} on file). Restore DockBack from this first, using the master encryption key.</li>
{{else}}<li class="warn"><strong>No application backup on file</strong> — the administrator should create one so DockBack can be rebuilt.</li>{{end}}
<li>You will need the master <strong>encryption key</strong> (kept off-box by the administrator) — without it, no backup can be restored.</li>
</ul>

{{if .RB.Destinations}}
<h2>2. Where your copies live</h2>
<ul>
{{range .RB.Destinations}}<li>{{.Name}} ({{.Type}}){{if not .Enabled}} — disabled{{end}}</li>
{{end}}</ul>
{{end}}

<h2>3. Restore order</h2>
<p>Restore in this order on each node: <strong>databases first</strong> (with their extensions), then dependent applications.</p>
{{range .RB.Nodes}}{{if .Services}}
<h3>{{.NodeName}}</h3>
{{range .Services}}
<div class="svc">
  <div><strong>{{.Order}}. {{.Container}}</strong> — {{if eq .Role "database"}}{{if .Engine}}{{.Engine}}{{else}}database{{end}}{{else}}app{{end}}{{if .Stack}} (stack: {{.Stack}}){{end}}</div>
  {{if .Image}}<div class="sub">Image: <code>{{.Image}}{{if .ImageDigest}}@{{.ImageDigest}}{{end}}</code>{{if .ImageBundled}} — bundled (restores offline){{end}}</div>{{end}}
  <div class="sub">Copies: {{locs .Locations}}</div>
  <div class="sub">Last backup: {{date .LastBackupAt}}; last proven restore: {{date .LastDrillAt}}</div>
  {{if .Extensions}}<div class="sub warn">Reinstall extensions before import: {{join .Extensions}}</div>{{end}}
  {{if .Partial}}<div class="sub warn">PARTIAL backup — some data not captured; recover it separately.</div>{{end}}
  {{if .OrigCompose}}<div class="sub">Original compose file(s) captured — prefer them over the reconstruction.</div>{{end}}
  {{range .Notes}}<div class="sub">{{.}}</div>{{end}}
</div>
{{end}}{{end}}{{end}}

<div class="meta">DockBack · redacted share · this link expires {{.ExpiresAt}}</div>
</body></html>`
