package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"dockback/internal/backup"
	"dockback/internal/store"
)

// gradeBackup computes a backup's restore-confidence grade (F50) from signals
// already present on the row — verification state, key match, the latest restore
// drill, the parsed manifest (partial capture), and the parsed locations (offsite
// coverage). It is PURE and takes NO Docker/network inputs, so annotating a whole
// page of backups costs nothing beyond one drills query.
//
// The grade is the worst applicable band; `reasons` lists EVERY negative signal so
// the UI can tell the operator exactly what to fix, not just the one that set the
// grade.
//
//	F — can't be trusted/decrypted: unverified, failed verification, or key mismatch.
//	D — verified but an active failure: an offsite copy failed, or the last drill failed.
//	C — verified, no failures, but incomplete: local-only, or a PARTIAL capture.
//	B — verified + healthy offsite + complete, but never drilled (restore unproven).
//	A — verified + drilled OK + healthy offsite + complete + key matches.
//
// standby (F75) is the target's pilot-light standby config, nil when none is
// configured. A failing standby adds a REASON line only — never a grade change —
// because standby readiness is a property of the target, not of this backup.
func gradeBackup(b *store.Backup, man *backup.Manifest, locs []backup.Location, drill *store.Drill, standby *store.Standby) (grade string, reasons []string) {
	verified := b.Verified == "verified"

	hasHealthyOffsite, offsiteFailed := false, false
	for _, l := range locs {
		if l.Kind != "dest" {
			continue
		}
		switch l.Status {
		case "failed":
			offsiteFailed = true
		case "deferred":
			// queued for its upload window — no data there yet, neither healthy nor failed
		default:
			hasHealthyOffsite = true
		}
	}
	// F83: a skip covered by another container's backups (a shared bind captured
	// once by its owner) is intentional, not data loss — only UNCOVERED skips
	// make a backup PARTIAL.
	partial := man.HasUncoveredSkip()
	// F103: this container was detected as a database engine but its dump tools
	// were missing, so its data was copied as raw FILES from a running database —
	// the torn-copy case the dump feature exists to prevent. Such a backup used to
	// be indistinguishable from an ordinary app backup and could be graded A with
	// no reasons at all.
	dbFallback := strings.TrimSpace(man.DBFallback) != ""
	// F154: the same failure one level down. SQLite databases were found under
	// the captured mounts and none could be snapshotted, so what is in the
	// archive is a raw copy of files that may have been mid-write — and, unlike
	// the case above, the commonest cause is a default nobody chose: the shipped
	// sidecar image has no sqlite3. It caps at C for the same reason DBFallback
	// does: the backup exists and may well be fine, and there is no way to know.
	sqliteFallback := strings.TrimSpace(man.SQLiteFallback) != ""
	// F122: this app's archive is a credential store for OTHER systems, and it is
	// NOT sealed to an offline key — so the key that opens it lives on a running
	// server, frequently one of the very machines the archive grants access to.
	// The backup itself is fine; what it is protected BY is the finding.
	credStore := ""
	if p := backup.ProfileFor(man.Image); p != nil && p.CredentialStore != "" && !backup.IsWriteOnly(man) {
		credStore = p.CredentialStore
	}
	// F141: this application's state is split across volumes that are only
	// meaningful together, and this archive holds only part of the set — so its
	// restore is refused outright. That is strictly worse than PARTIAL, which
	// describes a backup that restores an application missing some of its data;
	// this one does not restore at all.
	atomicIncomplete := backup.AtomicVolumeVerdict(man) != nil
	// F143: every certificate in the archive has already expired. The backup is
	// correct and restores correctly — the certificates simply aged — so this
	// changes no grade and only earns a line, the same treatment a failing
	// standby gets.
	certsExpired := expiredCertificates(man)
	// F146: this backup is one service of an application whose services are only
	// meaningful together, and it was captured on its own rather than inside an
	// app-consistent snapshot of the whole stack. Its restore is refused — both
	// on its own and as part of a stack restore, which would be mixing moments in
	// time. Like an incomplete atomic volume set, that is "will not restore",
	// not "restores something incomplete".
	atomicSolo := man != nil && man.StackAtomic != nil && len(man.StackAtomic.Members) >= 2 && man.StackAtomic.GroupID == ""
	// F147: files inside this archive whose permissions are wider than they
	// should be. A finding about the DATA, not about the backup — the backup
	// copied faithfully — so it earns a line and never a grade.
	secretFindings := backup.SecretFileFindings(man)
	// F157: interrupted-write leftovers this archive faithfully carries. A
	// finding about the SOURCE, not the backup — the copy is correct — so it
	// earns a line and never a grade.
	stagingFindings := backup.StaleStagingFindings(man)
	drillFailed := drill != nil && !drill.OK
	neverDrilled := drill == nil // A requires a PASSED drill, reached as the default band

	// Collect every negative signal, worst-first, for the "what's missing" list.
	if b.KeyMismatch {
		reasons = append(reasons, "encrypted with a different key — set the matching key to restore it")
	}
	if b.Verified == "failed" {
		reasons = append(reasons, "failed verification — do not rely on this backup")
	} else if !verified {
		reasons = append(reasons, "not verified yet")
	}
	if atomicIncomplete {
		reasons = append(reasons, "incomplete — this application keeps one configuration across several volumes and this archive holds only part of it, so a restore is refused. Back it up again with all of them included")
	}
	if atomicSolo {
		reasons = append(reasons, "captured on its own — this application's services are only meaningful together, and restoring one of them alone is refused. Back the whole stack up as one app-consistent snapshot")
	}
	if offsiteFailed {
		reasons = append(reasons, "an offsite copy failed to upload — send it offsite again")
	}
	if drillFailed {
		reasons = append(reasons, "the last restore drill failed")
	}
	if partial {
		// F130: say HOW MUCH. "Some data mounts were skipped" reads like a detail;
		// the Immich case was 20 KB captured and 58 GB skipped by the large-bind
		// default — 99.97% of the data absent from a backup that otherwise looked
		// fine. The size and the path are what make that legible.
		reasons = append(reasons, partialReason(man))
	}
	if dbFallback {
		reasons = append(reasons, "this database was captured as raw files (its dump tools were missing) — the copy may be torn")
	}
	if sqliteFallback {
		n := man.SQLiteFallbackCount
		if n <= 0 {
			n = 1
		}
		reasons = append(reasons, fmt.Sprintf("%d embedded database(s) were captured as raw files — %s. A database being written to can be torn by a raw copy; pause the container during the copy and back up again", n, man.SQLiteFallback))
	}
	if credStore != "" {
		reasons = append(reasons, "decryptable by this server — "+credStore+". Turn on write-only encryption so only an offline key can open it")
	}
	if !hasHealthyOffsite {
		reasons = append(reasons, "no offsite copy — keep a copy off this machine")
	}
	if neverDrilled {
		// F86: telling the operator to "run a restore drill" on a write-only backup
		// is advice they cannot take — DockBack cannot open it. The B cap still
		// stands, and honestly so: an un-drilled backup genuinely carries less
		// proof. That is the trade write-only buys, so the reason names it rather
		// than reading as a chore left undone.
		if backup.IsWriteOnly(man) {
			reasons = append(reasons, "write-only encrypted — DockBack can't drill it; rehearse a restore with your offline key to prove it")
		} else {
			reasons = append(reasons, "never drilled — run a restore drill to prove it restores")
		}
	}
	// F143: informational, for the same reason the standby line below is — the
	// archive is sound, and what aged is not something a re-run of the backup
	// would fix.
	if certsExpired > 0 {
		reasons = append(reasons, fmt.Sprintf("every TLS certificate in this backup has expired (%d) — restoring it restores an expired certificate; renew before relying on this copy", certsExpired))
	}
	// F147: a private key left world-readable is invisible precisely because
	// everything works. Said here because a confidence list is where somebody
	// looks when they are already thinking about what could go wrong.
	reasons = append(reasons, secretFindings...)
	reasons = append(reasons, stagingFindings...)
	// F75: informational only — a broken fallback is worth knowing when judging
	// "can I get this service back", but it doesn't change what THIS backup holds.
	if standby != nil && standby.LastRun > 0 && !standby.LastOK {
		reasons = append(reasons, "standby rehearsal on its fallback node is failing")
	}

	switch {
	// F141: an archive whose restore is refused cannot be trusted to restore, so
	// it belongs in the same band as one that cannot be decrypted — a backup that
	// will not come back, whatever else is right about it.
	case b.KeyMismatch || !verified || atomicIncomplete || atomicSolo:
		return "F", reasons
	case offsiteFailed || drillFailed:
		return "D", reasons
	// PARTIAL, a raw-file DB capture, local-only, or a fleet-credential archive
	// this server can open (F122) — all cap at C.
	case partial || dbFallback || sqliteFallback || credStore != "" || !hasHealthyOffsite:
		return "C", reasons
	case neverDrilled:
		return "B", reasons
	default:
		// All grade signals green. reasons is empty here unless the target's
		// standby is failing (F75) — which rides along without denting the A.
		return "A", reasons
	}
}

// expiredCertificates counts the certificates in an archive that have already
// expired, and returns 0 unless EVERY one of them has (F143).
//
// All-or-nothing on purpose. A deployment routinely holds an old certificate
// alongside its live one, and flagging that would put a permanent warning on a
// perfectly good backup. "Nothing in here is still valid" is the state worth
// saying out loud.
func expiredCertificates(man *backup.Manifest) int {
	if man == nil || len(man.Certificates) == 0 {
		return 0
	}
	now := time.Now()
	n := 0
	for _, c := range man.Certificates {
		t, err := time.Parse(time.RFC3339, c.NotAfter)
		if err != nil {
			return 0 // an unreadable date is not evidence of expiry
		}
		if now.After(t) {
			n++
		}
	}
	if n != len(man.Certificates) {
		return 0
	}
	return n
}

// maxNamedSkips bounds how many paths the partial reason lists before
// summarising — enough to recognise what is missing, not a wall of text.
const maxNamedSkips = 3

// partialReason describes what a PARTIAL backup is missing, with the measured
// size when it is known (F130).
//
// The size is the point. A skip reported as "some data mounts were skipped" is
// indistinguishable from a trimmed cache directory, and that is exactly how a
// metadata-only Immich backup — 20 KB of configuration, no photos at all — read
// as merely imperfect rather than as the wrong backup entirely.
//
// Only UNCOVERED skips count: a shared folder captured by a sibling container
// (F83) is intentional, not missing.
func partialReason(man *backup.Manifest) string {
	var total int64
	var paths []string
	hidden := 0
	for _, sk := range man.SkippedMounts {
		if sk.CoveredBy != "" {
			continue
		}
		total += sk.Bytes
		switch {
		case len(paths) < maxNamedSkips:
			paths = append(paths, sk.Destination)
		default:
			hidden++
		}
	}
	if len(paths) == 0 {
		return "PARTIAL — some data mounts were skipped; re-back up with them included"
	}
	where := strings.Join(paths, ", ")
	if hidden > 0 {
		where += fmt.Sprintf(" and %d more", hidden)
	}
	if total <= 0 {
		// Size unmeasured — say what is missing without inventing a figure.
		return "PARTIAL — " + where + " was NOT captured; re-back up with it included"
	}
	return fmt.Sprintf("PARTIAL — %s of data was NOT captured (%s). This backup does not contain it; re-back up with it included",
		humanBytes(total), where)
}

// annotateConfidence attaches the computed grade to a SUCCESSFUL backup row (F50).
// Non-success rows (running/failed) carry no grade — there's nothing to restore.
// `drills` is the request-scoped backup-id → latest-drill map, so the whole list is
// graded from data already in hand. Must run AFTER annotateKeyMismatch.
// It returns the parsed manifest + locations so list handlers can build the slim
// summary from the SAME parse (perf Fix 8) — nil for non-success rows.
func (s *Server) annotateConfidence(b *store.Backup, drills map[string]*store.Drill, standbys map[string]*store.Standby) (*backup.Manifest, []backup.Location) {
	if b == nil || b.Status != "success" {
		return nil, nil
	}
	var man backup.Manifest
	if b.ManifestJSON != "" {
		_ = json.Unmarshal([]byte(b.ManifestJSON), &man)
	}
	var locs []backup.Location
	if b.LocationsJSON != "" {
		_ = json.Unmarshal([]byte(b.LocationsJSON), &locs)
	}
	grade, reasons := gradeBackup(b, &man, locs, drills[b.ID], standbys[b.NodeID+"\x00"+b.TargetName])
	b.Confidence = &store.ConfidenceView{Grade: grade, Reasons: reasons}
	return &man, locs
}

// summarizeListRow replaces a list row's three raw JSON blobs with the compact
// pre-derived summary the list UI actually reads (perf Fix 8): ~2.4 KB of blob
// per row becomes ~100 bytes. The full-row endpoint (GET /api/backups/{id})
// keeps the blobs — the detail drawer fetches it on open.
func summarizeListRow(b *store.Backup, man *backup.Manifest, locs []backup.Location) {
	sum := &store.BackupSummary{}
	if man != nil {
		for _, sk := range man.SkippedMounts {
			if sk.CoveredBy == "" {
				sum.Partial = true // F83: only UNCOVERED skips are partial
			} else {
				sum.CoveredSkips++
				if sum.CoveredBy == "" {
					sum.CoveredBy = sk.CoveredBy
				}
			}
		}
		sum.WriteOnly = backup.IsWriteOnly(man)                  // F86
		sum.DBFallback = strings.TrimSpace(man.DBFallback) != "" // F103
		// F154: the embedded-database twin, shown by the same chip — from the
		// operator's side both mean "a database in here was copied as a live
		// file", and that is the fact worth seeing in a list.
		if strings.TrimSpace(man.SQLiteFallback) != "" {
			sum.DBFallback = true
		}
		sum.Incremental = man.Incremental
		sum.ChainDepth = man.ChainDepth
		sum.ImageBundled = man.ImageTar != nil
		sum.DBCount = len(man.Databases)
		sum.Image = man.Image
		// The timeline needs the digest only for change-detection + a 7-char
		// label — 12 hex chars keep both while shedding ~60 B on every row.
		if d := man.ImageDigest; len(d) > 19 && strings.HasPrefix(d, "sha256:") {
			sum.ImageDigest = d[:19]
		} else {
			sum.ImageDigest = d
		}
	}
	for _, l := range locs {
		if l.Kind != "dest" && l.Kind != "local" {
			continue
		}
		sum.Copies = append(sum.Copies, store.BackupCopy{Name: l.Name, Type: l.Type, Status: l.Status, Detail: l.Detail, Immutable: l.Immutable})
	}
	b.Summary = sum
	b.ManifestJSON, b.VerificationJSON, b.LocationsJSON = "", "", ""
	// The list never needs these either — the drawer's full-row fetch has them.
	b.CipherSHA256, b.StorageKey = "", ""
}

// drillMap loads every recorded drill once into a backup-id → drill map, so a list
// request grades all rows with a single query.
func (s *Server) drillMap() map[string]*store.Drill {
	m := map[string]*store.Drill{}
	if ds, err := s.store.ListDrills(); err == nil {
		for _, d := range ds {
			m[d.BackupID] = d
		}
	}
	return m
}

// standbyMap is the request-scoped (node,target) → standby config map (F75), so
// grading a whole backup list costs one standby query — mirrors drillMap.
func (s *Server) standbyMap() map[string]*store.Standby {
	m := map[string]*store.Standby{}
	if sbs, err := s.store.ListStandby(); err == nil {
		for _, sb := range sbs {
			m[sb.NodeID+"\x00"+sb.Target] = sb
		}
	}
	return m
}
