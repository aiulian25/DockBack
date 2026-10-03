package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"dockback/internal/crypto"
	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// Check is one verification step result.
type Check struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Info string `json:"info"`
}

// VerificationReport is the scored outcome of always-on verification.
type VerificationReport struct {
	OK      bool    `json:"ok"`
	At      string  `json:"at"`
	Checks  []Check `json:"checks"`
	Entries int     `json:"entries"`
	Bytes   int64   `json:"bytes"`
}

// Summary returns a short human description of failures.
func (r *VerificationReport) Summary() string {
	var bad []string
	for _, c := range r.Checks {
		if !c.OK {
			bad = append(bad, c.Name+": "+c.Info)
		}
	}
	if len(bad) == 0 {
		return "all checks passed"
	}
	return strings.Join(bad, "; ")
}

func (r *VerificationReport) add(name string, ok bool, info string) {
	r.Checks = append(r.Checks, Check{Name: name, OK: ok, Info: info})
	if !ok {
		r.OK = false
	}
}

// Verify performs always-on verification of a stored backup (PLAN §4.3, §3.4):
//  1. ciphertext SHA-256 matches the recorded value (catches bit-rot/truncation
//     in storage),
//  2. the archive decrypts (every GCM tag authenticates) and decompresses,
//  3. the tar is complete and the manifest is present,
//  4. each database dump is non-empty and has a sane header.
//
// A backup is never marked verified unless every check passes. (Full container
// test-restore drills are the documented fast-follow layer, PLAN §9.4.)
func (e *Engine) Verify(ctx context.Context, b *store.Backup, man *Manifest, source string) *VerificationReport {
	rep := &VerificationReport{OK: true, At: nowRFC3339()}

	// F44: read from the copy named by `source` ("" / "local" → local backend; a
	// destination ID → that destination), so a scrub can verify an offsite copy —
	// or the ONLY copy of an adopted, destination-only backup — not just local.
	be, _, err := e.storageFor(source, b)
	if err != nil {
		rep.add("ciphertext-readable", false, err.Error())
		return rep
	}

	// 1) Ciphertext integrity (re-hash what's in storage).
	rc, err := be.Get(ctx, b.StorageKey)
	if err != nil {
		rep.add("ciphertext-readable", false, err.Error())
		return rep
	}
	gotSHA, err := crypto.CipherSHA256(rc)
	rc.Close()
	if err != nil {
		rep.add("ciphertext-readable", false, err.Error())
		return rep
	}
	if b.CipherSHA256 != "" && gotSHA != b.CipherSHA256 {
		rep.add("ciphertext-sha256", false, "stored hash mismatch (corruption/tamper)")
		return rep
	}
	rep.add("ciphertext-sha256", true, gotSHA[:16]+"…")

	// 1b) Manifest tamper/confidentiality check. Either an encrypted
	// sidecar (decrypts ⇒ confidential + GCM-authenticated) or a readable one with
	// an HMAC signature. Verified when present; legacy backups with neither are
	// skipped.
	if erc, eerr := be.Get(ctx, b.StorageKey+".manifest.json.enc"); eerr == nil {
		// Bounded like the adopt path: same objects, same destination, same
		// hazard. An unreadable sidecar falls through to the verdict below
		// exactly as an undecryptable one always did.
		encBytes, _ := readSidecar(erc, b.StorageKey+".manifest.json.enc")
		if _, derr := crypto.OpenString(encBytes, e.MasterKey()); derr == nil {
			rep.add("manifest-encrypted", true, "")
		} else {
			rep.add("manifest-encrypted", false, "encrypted manifest failed to decrypt (corruption/tamper)")
		}
	} else if sb, serr := be.Get(ctx, b.StorageKey+".manifest.json.sig"); serr == nil {
		sigBytes, _ := readSidecar(sb, b.StorageKey+".manifest.json.sig")
		if mrc, merr := be.Get(ctx, b.StorageKey+".manifest.json"); merr == nil {
			mBytes, _ := readSidecar(mrc, b.StorageKey+".manifest.json")
			if crypto.VerifyManifest(mBytes, e.MasterKey(), strings.TrimSpace(string(sigBytes))) {
				rep.add("manifest-signature", true, "")
			} else {
				rep.add("manifest-signature", false, "sidecar manifest signature invalid (corruption/tamper)")
			}
		}
	}

	// F86: a write-only backup cannot be opened by this instance — that is the
	// whole point of the mode. The structural checks above (ciphertext SHA-256,
	// size, master-key-signed/encrypted manifest) have already run and still
	// prove the archive is intact and untampered; only the decrypt-and-walk is
	// impossible. Record that as a PASSING check with an explicit reason rather
	// than a failure, because a write-only backup is not a broken backup — but
	// never claim the payload was inspected, because it wasn't.
	if IsWriteOnly(man) && e.restorePrivFor() == "" {
		rep.add("archive-payload", true, "write-only encrypted — integrity verified from the ciphertext hash and signed manifest; the payload cannot be inspected without the offline private key")
		return rep
	}

	// 2+3) Decrypt -> decompress -> walk tar.
	rc2, err := be.Get(ctx, b.StorageKey)
	if err != nil {
		rep.add("archive-open", false, err.Error())
		return rep
	}
	defer rc2.Close()

	ak, err := e.archiveKey(man)
	if err != nil {
		rep.add("archive-key", false, err.Error())
		return rep
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(crypto.Decrypt(pw, rc2, ak)) }()

	zr, err := newDecompressReader(pr, man.Format.Algorithm)
	if err != nil {
		rep.add("decompress", false, err.Error())
		return rep
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	seen := map[string]int64{}
	dbHeads := map[string][]byte{}
	dbSHAs := map[string]string{} // F87: sha256 of each stored dump, from this same walk
	// F109: the same treatment for consistent SQLite snapshots, which until now
	// were walked past without a single check — for most self-hosted apps the
	// .dbk IS the backup, so it was the most important object in the archive and
	// the least verified.
	sqliteSHAs := map[string]string{}
	sqliteHeads := map[string][]byte{}
	sqliteWanted := map[string]bool{}
	for _, s := range man.SQLiteDumps {
		if s.Archive != "" {
			sqliteWanted[s.Archive] = true
		}
	}
	volSHA := ""
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			rep.add("archive-complete", false, err.Error())
			return rep
		}
		// Read the full entry to force GCM auth + completeness. For the volume
		// payload, also hash it so we can confirm the manifest checksum.
		var head bytes.Buffer
		var n int64
		switch {
		case hdr.Name == volPayloadMember(man) && man.VolumesSHA256 != "":
			vh := sha256.New()
			n, err = io.Copy(vh, tr)
			volSHA = fmt.Sprintf("%x", vh.Sum(nil))
		case strings.HasPrefix(hdr.Name, "db/"):
			// F87: hash the dump in the SAME pass that already reads it, so a dump
			// that rots in storage is caught by a routine scrub rather than at the
			// moment someone needs to restore it. No extra read.
			dh := sha256.New()
			n, err = io.Copy(dh, headTee(tr, &head, 512))
			dbSHAs[hdr.Name] = hex.EncodeToString(dh.Sum(nil))
		case sqliteWanted[hdr.Name]:
			// Hash in the SAME pass that already reads the entry — no extra read,
			// so a scrub that catches a rotted dump now catches a rotted SQLite
			// snapshot too.
			sh := sha256.New()
			n, err = io.Copy(sh, headTee(tr, &head, 32))
			sqliteSHAs[hdr.Name] = hex.EncodeToString(sh.Sum(nil))
		default:
			n, err = io.Copy(io.Discard, headTee(tr, &head, 512))
		}
		if err != nil {
			rep.add("archive-complete", false, fmt.Sprintf("%s: %v", hdr.Name, err))
			return rep
		}
		seen[hdr.Name] = n
		rep.Entries++
		rep.Bytes += n
		if strings.HasPrefix(hdr.Name, "db/") {
			dbHeads[hdr.Name] = head.Bytes()
		}
		if sqliteWanted[hdr.Name] {
			sqliteHeads[hdr.Name] = head.Bytes()
		}
	}
	rep.add("archive-complete", true, fmt.Sprintf("%d entries, %s", rep.Entries, humanBytes(rep.Bytes)))

	// Volume payload checksum matches the manifest. Only when both
	// the manifest value and a volume payload member were present.
	if man.VolumesSHA256 != "" {
		if volSHA == man.VolumesSHA256 {
			rep.add("volumes-sha256", true, volSHA[:16]+"…")
		} else {
			rep.add("volumes-sha256", false, "volume payload checksum mismatch (corruption/tamper)")
		}
	}

	// Incremental chain integrity (F61): a delta is only restorable if its parent
	// still exists and is the SAME copy it was captured against — so verify the
	// parent is present and its recorded cipher checksum matches the pin. This
	// catches a pruned/orphaned or substituted parent at verify time, not at a
	// panicked restore.
	if man.Incremental && man.Parent != "" {
		if parent, perr := e.Store.GetBackup(man.Parent); perr != nil {
			rep.add("incremental-chain", false, "parent backup "+short(man.Parent)+" is missing — this delta cannot be restored")
		} else if man.ParentCipherSHA256 != "" && parent.CipherSHA256 != "" && parent.CipherSHA256 != man.ParentCipherSHA256 {
			rep.add("incremental-chain", false, "parent backup does not match the pin recorded in this delta (corruption/tamper)")
		} else {
			rep.add("incremental-chain", true, "parent "+short(man.Parent)+" present, depth "+strconv.Itoa(man.ChainDepth))
		}
	}

	if _, ok := seen["manifest.json"]; !ok {
		rep.add("manifest-present", false, "manifest.json missing from archive")
	} else {
		rep.add("manifest-present", true, "ok")
	}

	// 4) Database dump sanity.
	for _, db := range man.Databases {
		key := "db/" + strings.TrimPrefix(db.Path, "db/")
		size, ok := seen[key]
		if !ok || size == 0 {
			rep.add("db:"+db.Service, false, "dump empty or missing")
			continue
		}
		if !dbHeaderLooksValid(db.Engine, dbHeads[key]) {
			rep.add("db:"+db.Service, false, "unexpected dump header for "+db.Engine)
			continue
		}
		rep.add("db:"+db.Service, true, humanBytes(size))

		// F87: the stored dump must still be byte-identical to what capture
		// recorded. This is what turns "the dump was complete when written" into
		// "the dump is complete NOW" — bit-rot, a truncated upload or a tampered
		// copy all show up here, during a routine scrub.
		if db.DumpSHA256 != "" {
			got := dbSHAs[key]
			switch {
			case got == "":
				// Nothing read for this entry — the size check above already failed
				// it, so stay quiet rather than adding a second confusing line.
			case got == db.DumpSHA256:
				detail := got[:16] + "…"
				if db.DumpPrimaryKeys > 0 || db.DumpForeignKeys > 0 {
					detail += fmt.Sprintf(" · %d primary key(s), %d foreign key(s)", db.DumpPrimaryKeys, db.DumpForeignKeys)
				}
				rep.add("db-dump-integrity:"+db.Service, true, detail)
			default:
				rep.add("db-dump-integrity:"+db.Service, false,
					"the stored dump no longer matches the checksum recorded when it was captured — do NOT rely on this backup (corruption or tampering)")
			}
		}
	}

	// 4b) Consistent SQLite snapshot sanity (F109). For an application backup this
	// is usually the whole payload that matters, so it gets the same treatment as
	// a native dump: present, non-empty, a real SQLite file, and still byte-
	// identical to what capture recorded.
	for _, s := range man.SQLiteDumps {
		label := "sqlite:" + sqliteLabel(s.Source)
		size, ok := seen[s.Archive]
		if !ok || size == 0 {
			rep.add(label, false, "consistent snapshot empty or missing from the archive")
			continue
		}
		if !bytes.HasPrefix(sqliteHeads[s.Archive], []byte(sqliteFileMagic)) {
			rep.add(label, false, "not a SQLite database file (header magic missing) — the snapshot is corrupt")
			continue
		}
		detail := humanBytes(size)
		if s.Tables > 0 {
			detail += fmt.Sprintf(" · %d table(s)", s.Tables)
		}
		if s.Rows > 0 {
			detail += fmt.Sprintf(", %d row(s)", s.Rows)
		}
		rep.add(label, true, detail)

		// The stored snapshot must still be the one capture wrote. Bit-rot, a
		// truncated upload and a tampered copy all surface here, during a routine
		// scrub, instead of at the moment someone needs the database back.
		if s.SHA256 == "" {
			continue // pre-F109 backup: nothing recorded to compare against
		}
		switch got := sqliteSHAs[s.Archive]; {
		case got == "":
			// The size check above already failed this entry; a second line would
			// only confuse.
		case got == s.SHA256:
			rep.add("sqlite-integrity:"+sqliteLabel(s.Source), true, got[:16]+"…")
		default:
			rep.add("sqlite-integrity:"+sqliteLabel(s.Source), false,
				"the stored snapshot no longer matches the checksum recorded when it was captured — do NOT rely on this backup (corruption or tampering)")
		}
	}

	// 5) Deep verification — opt-in real test-restore of each DB dump
	// into a throwaway, isolated container with a sanity query. Heavier (spins a
	// DB container per dump), so it's gated behind a setting until the fleet-scale
	// worker pool (§4.13/§9.9) lands. When on, a failed test-restore flips the
	// backup to NOT verified (§4.3: never "Verified" until a restore actually
	// passes).
	if e.deepVerifyEnabled() {
		if len(man.Databases) == 0 {
			// App / file-volume backup: deep verify (DB test-restore) has nothing to
			// do. Say so explicitly — silence looked like the feature wasn't running.
			e.logf(b.ID, "INFO", "Deep verification is ON, but this backup has no database dumps to test-restore (file/volume backup) — nothing to do")
			rep.add("deep-verify", true, "no database dumps in this backup — nothing to test-restore")
		} else {
			e.verifyDeepDB(ctx, b, man, rep)
		}
	}

	return rep
}

// DrillBackup runs a restore drill for one backup (PLAN §9.4) in a SINGLE
// decrypt pass — deliberately NOT the full Verify (which re-reads and decrypts
// the archive three times: a ciphertext-hash pass, a tar-walk pass, and a
// third pass to extract DB dumps). A drill instead streams the archive once,
// authenticating every chunk (GCM = integrity), confirming the volume-payload
// checksum, and spooling each DB dump (and, when the backup has volumes, the
// volume payload) to disk. It then test-restores each database dump into a
// throwaway, network-isolated container AND — for a volume/app backup (F5) —
// extracts the volumes into a fresh throwaway volume and boots an isolated copy
// of the container to prove the app actually comes back. On a NAS-class CPU,
// decrypting + decompressing once instead of three times is the dominant
// speed-up. Read-only: it spins only throwaway sandboxes and never touches the
// stored archive, the backup's verified state, or the live stack. Returns
// pass/fail + a summary.
func (e *Engine) DrillBackup(ctx context.Context, b *store.Backup) (bool, string) {
	man := &Manifest{}
	if err := json.Unmarshal([]byte(b.ManifestJSON), man); err != nil {
		return false, "manifest unreadable: " + err.Error()
	}
	// F86 defence in depth: callers must consult DrillSkipReason first (a recorded
	// "failed" drill would alert and downgrade confidence, which is the opposite
	// of the truth for a write-only backup). This guard only catches a caller
	// that forgot.
	if reason := e.drillSkipReason(man); reason != "" {
		return false, reason
	}
	rc, err := e.Storage.Get(ctx, b.StorageKey)
	if err != nil {
		return false, "archive unreadable: " + err.Error()
	}
	defer rc.Close()
	ak, err := e.archiveKey(man)
	if err != nil {
		return false, "archive key: " + err.Error()
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(crypto.Decrypt(pw, rc, ak)) }()
	defer pr.Close() // unblocks the decrypt goroutine if we bail early
	zr, err := newDecompressReader(pr, man.Format.Algorithm)
	if err != nil {
		return false, "decompress: " + err.Error()
	}
	defer zr.Close()

	// One tar walk: authenticate every entry, hash the volume payload, and spool
	// DB dumps to disk (they're small — MB) for test-restore after the walk.
	tr := tar.NewReader(zr)
	volSHA := ""
	var volTmp string       // spooled volumes.tar (for the F5 volume boot) — cleaned up below
	var inspectBytes []byte // config/inspect.json (to boot the throwaway app the way it really runs)
	type dbFile struct{ svc, tmp, engine string }
	var dumps []dbFile
	defer func() {
		for _, d := range dumps {
			os.Remove(d.tmp)
		}
		if volTmp != "" {
			os.Remove(volTmp)
		}
	}()
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, "archive incomplete (corruption/tamper): " + err.Error()
		}
		switch {
		case hdr.Name == "volumes.tar":
			// Hash the payload (checksum proof) and, when the backup actually has
			// volumes, ALSO spool it to disk in this single decrypt pass so we can
			// boot it in an isolated throwaway container after the walk (F5).
			vh := sha256.New()
			var dst io.Writer = vh
			if len(man.Volumes) > 0 {
				f, ferr := os.CreateTemp(e.WorkDir, "drillvol-*")
				if ferr != nil {
					return false, "temp file: " + ferr.Error()
				}
				volTmp = f.Name()
				if _, cerr := io.Copy(io.MultiWriter(f, vh), tr); cerr != nil {
					f.Close()
					return false, "reading volume payload: " + cerr.Error()
				}
				f.Close()
			} else {
				if _, err := io.Copy(dst, tr); err != nil {
					return false, "reading volume payload: " + err.Error()
				}
			}
			if man.VolumesSHA256 != "" {
				volSHA = fmt.Sprintf("%x", vh.Sum(nil))
			}
		case hdr.Name == "config/inspect.json":
			data, rerr := io.ReadAll(tr)
			if rerr != nil {
				return false, "reading container config: " + rerr.Error()
			}
			inspectBytes = data
		case strings.HasPrefix(hdr.Name, "db/"):
			f, ferr := os.CreateTemp(e.WorkDir, "drill-*")
			if ferr != nil {
				return false, "temp file: " + ferr.Error()
			}
			_, cerr := io.Copy(f, tr)
			f.Close()
			if cerr != nil {
				return false, "reading database dump: " + cerr.Error()
			}
			svc := strings.TrimPrefix(hdr.Name, "db/")
			if i := strings.LastIndexByte(svc, '.'); i > 0 {
				svc = svc[:i] // strip the dump extension (.sql/.archive/.rdb) for a clean label
			}
			dumps = append(dumps, dbFile{svc: svc, tmp: f.Name(), engine: dbEngineForEntry(man, hdr.Name)})
		default:
			if _, err := io.Copy(io.Discard, tr); err != nil {
				return false, "archive incomplete (corruption/tamper): " + err.Error()
			}
		}
	}

	if man.VolumesSHA256 != "" && volSHA != man.VolumesSHA256 {
		return false, "volume payload checksum mismatch (corruption/tamper)"
	}

	hasVol := len(man.Volumes) > 0 && volTmp != ""
	if !hasVol && len(dumps) == 0 {
		return true, "archive intact; nothing to test-restore"
	}

	cli, err := e.Reg.Get(b.NodeID)
	if err != nil {
		return false, "node unavailable for test-restore: " + err.Error()
	}
	var parts []string

	// F5: functional volume/app restore — extract the captured volumes into a fresh
	// throwaway volume and boot an isolated copy of the container to prove the app
	// actually comes back, not just that the archive is intact.
	if hasVol {
		f, oerr := os.Open(volTmp)
		if oerr != nil {
			return false, "reopening volume payload: " + oerr.Error()
		}
		dests := make([]string, 0, len(man.Volumes))
		for _, v := range man.Volumes {
			if v.Destination != "" {
				dests = append(dests, v.Destination)
			}
		}
		summary, bootEnv, verr := dockercli.VerifyRestoreVolumes(ctx, cli, man.Image, inspectBytes, dests, f)
		f.Close()
		if verr != nil {
			return false, "volume restore did not boot: " + verr.Error()
		}
		parts = append(parts, "volumes: "+summary)
		// #38: the drill boots the throwaway from the recorded configuration, so
		// its environment is the same claim a real restore makes. Checking it here
		// costs nothing and catches the case where the recorded inspect did not
		// parse and the app booted with no environment at all — which looks
		// exactly as healthy.
		if envNote, ok := drillEnvVerdict(man, dockercli.ContainerEnv(inspectBytes), bootEnv); !ok {
			return false, envNote
		} else if envNote != "" {
			parts = append(parts, envNote)
		}
	}

	for _, d := range dumps {
		f, oerr := os.Open(d.tmp)
		if oerr != nil {
			return false, "reopening dump: " + oerr.Error()
		}
		summary, verr := dockercli.VerifyRestoreDB(ctx, cli, man.Image, d.engine, f)
		f.Close()
		if verr != nil {
			return false, "database " + d.svc + " did not restore: " + verr.Error()
		}
		parts = append(parts, d.svc+": "+summary)
	}
	return true, strings.Join(parts, "; ")
}

// deepVerifyEnabled reports whether opt-in deep (real test-restore) verification
// is turned on.
func (e *Engine) deepVerifyEnabled() bool {
	v, _ := e.Store.GetSetting("verify.deep", "false")
	return v == "true"
}

// verifyDeepDB re-imports every database dump into a throwaway isolated container
// and records a scored db-restore check per service.
func (e *Engine) verifyDeepDB(ctx context.Context, b *store.Backup, man *Manifest, rep *VerificationReport) {
	cli, err := e.Reg.Get(b.NodeID)
	if err != nil {
		rep.add("db-restore", false, "node unavailable: "+err.Error())
		return
	}
	// Map archive entry -> (engine, service).
	type dbInfo struct{ engine, service string }
	byEntry := map[string]dbInfo{}
	for _, db := range man.Databases {
		byEntry["db/"+strings.TrimPrefix(db.Path, "db/")] = dbInfo{db.Engine, db.Service}
	}
	e.logf(b.ID, "INFO", "Deep verification: test-restoring %d database dump(s) into throwaway containers", len(man.Databases))
	walkErr := e.streamArchive(ctx, b, "", func(tr *tar.Reader, hdr *tar.Header) (bool, error) {
		info, ok := byEntry[hdr.Name]
		if !ok {
			return true, nil
		}
		summary, verr := dockercli.VerifyRestoreDB(ctx, cli, man.Image, info.engine, tr)
		if verr != nil {
			rep.add("db-restore:"+info.service, false, verr.Error())
		} else {
			rep.add("db-restore:"+info.service, true, summary)
			e.logf(b.ID, "INFO", "Deep verify %s: test-restore OK (%s)", info.service, summary)
		}
		return true, nil
	})
	if walkErr != nil {
		rep.add("db-restore", false, walkErr.Error())
	}
}

// headTee returns a reader that copies the first max bytes into head while
// passing all bytes through (used to sniff DB dump headers during the
// completeness read).
func headTee(r io.Reader, head io.Writer, max int) io.Reader {
	return &teeHead{r: r, head: head, remaining: max}
}

type teeHead struct {
	r         io.Reader
	head      io.Writer
	remaining int
}

func (t *teeHead) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 && t.remaining > 0 {
		w := n
		if w > t.remaining {
			w = t.remaining
		}
		t.head.Write(p[:w])
		t.remaining -= w
	}
	return n, err
}

// sqliteFileMagic is the header every SQLite database file starts with. The
// check reads the stored snapshot's own bytes rather than trusting its name, the
// same discipline DetectSQLiteFiles uses at capture.
const sqliteFileMagic = "SQLite format 3"

// sqliteLabel turns a database's absolute in-container path into a short,
// stable check name ("/config/absdatabase.sqlite" -> "absdatabase.sqlite").
// Falls back to the full path when the basename would be ambiguous or empty, so
// a check never loses the ability to identify which database it is about.
func sqliteLabel(source string) string {
	if source == "" {
		return "database"
	}
	if i := strings.LastIndexByte(source, '/'); i >= 0 && i+1 < len(source) {
		return source[i+1:]
	}
	return source
}

func dbHeaderLooksValid(engine string, head []byte) bool {
	s := string(head)
	switch engine {
	case "postgres":
		return strings.Contains(s, "PostgreSQL database") || strings.Contains(s, "--") || strings.Contains(s, "SET ")
	case "mysql":
		return strings.Contains(s, "MySQL dump") || strings.Contains(s, "MariaDB") || strings.Contains(s, "--")
	case "mongodb":
		return len(head) > 0 // mongodump --archive is binary
	case "redis":
		return strings.HasPrefix(s, "REDIS") // every RDB starts with the ASCII magic
	}
	return len(head) > 0
}
