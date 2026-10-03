// Package appbackup creates and restores an encrypted backup of DockBack's
// OWN state — the SQLite database holding nodes (+ secrets), the backup catalog,
// settings, destinations, and the admin account including its sealed TOTP secret
// (PLAN §6.5/§9.3). This is distinct from container backups.
//
// Archive format (self-describing, mirrors the container archive ethos):
//
//	v2 (envelope, F37):  magic "DBCFGv2\n" | uint32_be(hdrLen) | hdr JSON |
//	                     AES-256-GCM( tar{ manifest.json, dockback.db } ) under a DEK
//	v1 (legacy):         AES-256-GCM( tar{ manifest.json, dockback.db } ) under the master key
//
// v2 gives each archive its own random data-encryption key (DEK); the DEK is
// wrapped by the instance master key and stored in the PLAINTEXT header, so
// master-key rotation (F16) re-wraps app-backups in place — exactly like container
// archives — without re-encrypting the body. The header must precede the
// ciphertext because the reader needs wrapped_key before it can decrypt. A v1
// archive has no header and begins directly with the crypto stream; it opens with
// the master key, which still proves the key matches. The reader dispatches on the
// magic — it never guesses.
package appbackup

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"dockback/internal/crypto"
	"dockback/internal/store"
)

const (
	Format       = "dockback-config"
	Version      = 2 // v2 = envelope (per-backup DEK wrapped by the master key) with a plaintext header (F37)
	manifestName = "manifest.json"
	dbName       = "dockback.db"

	// magicV2 marks a v2 (envelope) app-backup. It is distinct from the container
	// crypto stream's own "DBACKv1" magic, so the two are never confused.
	magicV2      = "DBCFGv2\n"
	maxHeaderLen = 1 << 16 // sanity cap on the plaintext header (it is tiny JSON)
)

// Manifest is the small JSON header describing the archive. In v2 it lives in the
// PLAINTEXT header (so wrapped_key can be read before decrypting); a copy without
// the wrapped key is also carried inside the encrypted tar for self-description.
type Manifest struct {
	Format         string `json:"format"`
	Version        int    `json:"version"`
	CreatedAt      int64  `json:"created_at"`
	AppVersion     string `json:"app_version"`
	KeyFingerprint string `json:"key_fingerprint"`
	// WrappedKey is the per-backup DEK wrapped by the master key (base64), present
	// only in the v2 plaintext header. Empty ⇒ a legacy v1 archive encrypted directly
	// with the master key (PLAN §3.3 envelope, F37).
	WrappedKey string `json:"wrapped_key,omitempty"`
}

// Create streams an encrypted v2 (envelope) archive to w from a freshly written DB
// snapshot at snapshotPath (the caller owns/cleans that file). Streaming via
// io.Pipe keeps memory flat regardless of catalog size. It generates a per-backup
// DEK, wraps it with the master key, writes the plaintext header, then encrypts the
// inner tar with the DEK — so master-key rotation can re-wrap it later (F37).
func Create(w io.Writer, snapshotPath string, key []byte, m Manifest) error {
	dek, err := crypto.NewDataKey()
	if err != nil {
		return err
	}
	wrapped, err := crypto.WrapKey(dek, key)
	if err != nil {
		return err
	}
	m.Format = Format
	m.Version = Version
	m.WrappedKey = wrapped
	if err := writeHeader(w, m); err != nil {
		return err
	}

	// The wrapped key belongs ONLY in the plaintext header; the copy inside the
	// (encrypted) tar carries no wrapped key so nothing goes stale after a rotation.
	inner := m
	inner.WrappedKey = ""

	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		if werr := writeArchive(tw, snapshotPath, inner); werr != nil {
			_ = tw.Close()
			pw.CloseWithError(werr)
			return
		}
		pw.CloseWithError(tw.Close())
	}()
	_, err = crypto.Encrypt(w, pr, dek)
	return err
}

// writeHeader writes the v2 plaintext header: magic | uint32_be(len) | JSON(m).
func writeHeader(w io.Writer, m Manifest) error {
	hb, err := json.Marshal(&m)
	if err != nil {
		return err
	}
	if len(hb) > maxHeaderLen {
		return fmt.Errorf("app-backup header too large (%d bytes)", len(hb))
	}
	if _, err := io.WriteString(w, magicV2); err != nil {
		return err
	}
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(hb)))
	if _, err := w.Write(l[:]); err != nil {
		return err
	}
	_, err = w.Write(hb)
	return err
}

// openArchive reads the optional v2 plaintext header from src and returns the
// stream positioned at the ciphertext plus the key that decrypts it: the unwrapped
// per-backup DEK for v2, or the master key for a v1 archive (no header). It
// dispatches strictly on the magic — never guessing. hdr is the parsed v2 header
// (nil for v1).
func openArchive(src io.Reader, master []byte) (stream io.Reader, decKey []byte, hdr *Manifest, err error) {
	head := make([]byte, len(magicV2))
	n, rerr := io.ReadFull(src, head)
	if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
		// Shorter than the magic → cannot be v2; treat as v1 (decrypt will fail clearly).
		return io.MultiReader(bytes.NewReader(head[:n]), src), master, nil, nil
	}
	if rerr != nil {
		return nil, nil, nil, rerr
	}
	if string(head) != magicV2 {
		// v1: no header — hand the peeked bytes back and decrypt with the master key.
		return io.MultiReader(bytes.NewReader(head), src), master, nil, nil
	}
	var l [4]byte
	if _, err := io.ReadFull(src, l[:]); err != nil {
		return nil, nil, nil, fmt.Errorf("reading header length: %w", err)
	}
	hlen := binary.BigEndian.Uint32(l[:])
	if hlen == 0 || hlen > maxHeaderLen {
		return nil, nil, nil, fmt.Errorf("invalid app-backup header length %d", hlen)
	}
	hb := make([]byte, hlen)
	if _, err := io.ReadFull(src, hb); err != nil {
		return nil, nil, nil, fmt.Errorf("reading header: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(hb, &m); err != nil {
		return nil, nil, nil, fmt.Errorf("parsing header: %w", err)
	}
	if m.WrappedKey == "" {
		// A v2 header with no wrapped key shouldn't happen; fall back to the master key.
		return src, master, &m, nil
	}
	dek, err := crypto.UnwrapKey(m.WrappedKey, master)
	if err != nil {
		return nil, nil, &m, fmt.Errorf("cannot unwrap app-backup key — wrong master encryption key: %w", err)
	}
	return src, dek, &m, nil
}

func writeArchive(tw *tar.Writer, snapshotPath string, m Manifest) error {
	mb, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: manifestName, Mode: 0o600, Size: int64(len(mb))}); err != nil {
		return err
	}
	if _, err := tw.Write(mb); err != nil {
		return err
	}
	f, err := os.Open(snapshotPath)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: dbName, Mode: 0o600, Size: fi.Size()}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

// ---------------- On-disk local catalog ----------------

// Entry describes one stored application backup (mirrors the sidecar .meta.json,
// which holds only non-sensitive metadata — never credentials).
type Entry struct {
	File           string `json:"file"`
	CreatedAt      int64  `json:"created_at"`
	AppVersion     string `json:"app_version"`
	KeyFingerprint string `json:"key_fingerprint"`
	Nodes          int    `json:"nodes"`
	Backups        int    `json:"backups"`
	Destinations   int    `json:"destinations"`
	SizeBytes      int64  `json:"size_bytes"`
}

// nameRe is the only filename shape we create/accept — anchors path-traversal out.
var nameRe = regexp.MustCompile(`^dockback-config-[0-9]{8}-[0-9]{6}\.dback$`)

// ValidName reports whether name is a safe app-backup filename (basename only).
func ValidName(name string) bool { return nameRe.MatchString(name) }

// CreateFile writes a stored, encrypted application backup (+ a non-sensitive
// sidecar .meta.json) into dir and returns the completed entry. e carries the
// metadata to record (CreatedAt, counts, etc.); the filename is derived from
// CreatedAt so it's stable and traversal-safe.
func CreateFile(dir, snapshotPath string, key []byte, e Entry) (Entry, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return e, err
	}
	e.File = fmt.Sprintf("dockback-config-%s.dback", time.Unix(e.CreatedAt, 0).UTC().Format("20060102-150405"))
	archivePath := filepath.Join(dir, e.File)

	f, err := os.Create(archivePath)
	if err != nil {
		return e, err
	}
	m := Manifest{Format: Format, Version: Version, CreatedAt: e.CreatedAt, AppVersion: e.AppVersion, KeyFingerprint: e.KeyFingerprint}
	if err := Create(f, snapshotPath, key, m); err != nil {
		f.Close()
		os.Remove(archivePath)
		return e, err
	}
	if err := f.Close(); err != nil {
		os.Remove(archivePath)
		return e, err
	}
	if fi, err := os.Stat(archivePath); err == nil {
		e.SizeBytes = fi.Size()
	}
	if mb, err := json.MarshalIndent(e, "", "  "); err == nil {
		_ = os.WriteFile(archivePath+".meta.json", mb, 0o600)
	}
	return e, nil
}

// List returns stored backups in dir, newest first. A missing dir is empty.
func List(dir string) ([]Entry, error) {
	des, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []Entry{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Entry{}
	for _, de := range des {
		if de.IsDir() || !ValidName(de.Name()) {
			continue
		}
		var e Entry
		if mb, err := os.ReadFile(filepath.Join(dir, de.Name()+".meta.json")); err == nil {
			_ = json.Unmarshal(mb, &e)
		}
		e.File = de.Name()
		if fi, err := de.Info(); err == nil {
			e.SizeBytes = fi.Size()
			if e.CreatedAt == 0 {
				e.CreatedAt = fi.ModTime().Unix()
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}

// OpenFile opens a stored backup for download. name must be a valid basename.
func OpenFile(dir, name string) (io.ReadCloser, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("invalid backup name")
	}
	return os.Open(filepath.Join(dir, name))
}

// RestoreFile stages a restore from a stored backup (see Restore).
func RestoreFile(dir, name string, key []byte, dataDir, tmpDir string) (Manifest, error) {
	if !ValidName(name) {
		return Manifest{}, fmt.Errorf("invalid backup name")
	}
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return Manifest{}, err
	}
	defer f.Close()
	return Restore(f, key, dataDir, tmpDir)
}

// VerifyFile proves a stored app-backup restores (F58) WITHOUT staging it: it
// decrypts + extracts the contained SQLite snapshot into tmpDir only, then runs
// store.CheckSnapshot (read-only integrity check + catalog-table read). It never
// writes into the data dir, never swaps a live DB, and cleans up its temp files.
// Any decrypt/corruption/integrity failure is returned so the drill can alert.
func VerifyFile(dir, name string, key []byte, tmpDir string) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid backup name")
	}
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	defer f.Close()

	// Same header dispatch + decrypt path as Restore (v2 DEK / v1 master key).
	stream, decKey, _, oerr := openArchive(f, key)
	if oerr != nil {
		return fmt.Errorf("decrypt failed — wrong encryption key or corrupt file")
	}
	tmpTar, err := os.CreateTemp(tmpDir, "cfgverify-*.tar")
	if err != nil {
		return err
	}
	defer os.Remove(tmpTar.Name())
	defer tmpTar.Close()
	if err := crypto.Decrypt(tmpTar, stream, decKey); err != nil {
		return fmt.Errorf("decrypt failed — wrong encryption key or corrupt file")
	}
	if _, err := tmpTar.Seek(0, io.SeekStart); err != nil {
		return err
	}

	tmpDB, err := os.CreateTemp(tmpDir, "cfgverify-*.db")
	if err != nil {
		return err
	}
	dbPath := tmpDB.Name()
	defer os.Remove(dbPath)
	defer func() { os.Remove(dbPath + "-wal"); os.Remove(dbPath + "-shm") }()

	var m Manifest
	sawDB := false
	tr := tar.NewReader(tmpTar)
	for {
		h, rerr := tr.Next()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			tmpDB.Close()
			return fmt.Errorf("archive read: %w", rerr)
		}
		switch filepath.Base(h.Name) {
		case manifestName:
			if derr := json.NewDecoder(tr).Decode(&m); derr != nil {
				tmpDB.Close()
				return fmt.Errorf("manifest: %w", derr)
			}
		case dbName:
			if _, cerr := io.Copy(tmpDB, tr); cerr != nil {
				tmpDB.Close()
				return cerr
			}
			sawDB = true
		}
	}
	tmpDB.Close()

	if m.Format != Format {
		return fmt.Errorf("not a DockBack application backup")
	}
	if !sawDB {
		return fmt.Errorf("archive is missing %s", dbName)
	}
	return store.CheckSnapshot(dbPath)
}

// DeleteFile removes a stored backup and its sidecar. name must be a basename.
func DeleteFile(dir, name string) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid backup name")
	}
	if err := os.Remove(filepath.Join(dir, name)); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(dir, name+".meta.json"))
	return nil
}

// RewrapFile re-wraps a v2 app-backup's DEK from oldKey to newKey IN PLACE (F37),
// rewriting ONLY the plaintext header; the encrypted archive body is copied
// byte-for-byte, so the ciphertext is unchanged and no re-encryption happens. It
// mirrors the container-backup rotation (backup.RewrapAll). Returns:
//
//	(true, nil)  — re-wrapped: the DEK now opens under newKey and not oldKey
//	(false, nil) — SKIPPED, left completely untouched: a v1 archive (no header/wrapped
//	               key) or one wrapped by an unrelated key
//	(false, err) — an I/O failure; the original file is left intact
//
// newFP is the fingerprint of newKey (computed by the caller, which owns that
// helper), recorded in the header so the archive reports the key it now belongs to.
func RewrapFile(dir, name, newFP string, oldKey, newKey []byte) (bool, error) {
	if !ValidName(name) {
		return false, fmt.Errorf("invalid backup name")
	}
	path := filepath.Join(dir, name)
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	head := make([]byte, len(magicV2))
	if _, err := io.ReadFull(f, head); err != nil {
		return false, nil // too short to be v2 → legacy; leave untouched
	}
	if string(head) != magicV2 {
		return false, nil // v1 (direct master key) — nothing to re-wrap; leave untouched
	}
	var l [4]byte
	if _, err := io.ReadFull(f, l[:]); err != nil {
		return false, err
	}
	hlen := binary.BigEndian.Uint32(l[:])
	if hlen == 0 || hlen > maxHeaderLen {
		return false, fmt.Errorf("invalid app-backup header length %d", hlen)
	}
	hb := make([]byte, hlen)
	if _, err := io.ReadFull(f, hb); err != nil {
		return false, err
	}
	var m Manifest
	if err := json.Unmarshal(hb, &m); err != nil {
		return false, err
	}
	if m.WrappedKey == "" {
		return false, nil // nothing to re-wrap
	}
	newWrapped, err := crypto.RewrapDEK(m.WrappedKey, oldKey, newKey)
	if err != nil {
		// Doesn't unwrap with the OLD key. If it already unwraps with the NEW key
		// it was re-wrapped earlier (a resumed/re-run rotation) — nothing to do.
		if _, nerr := crypto.UnwrapKey(m.WrappedKey, newKey); nerr == nil {
			return false, nil
		}
		// A v2 envelope sealed under an UNRELATED master key (or corrupt): report
		// it loudly as FAILED, never a silent skip — this file will not open with
		// the new key, and the operator must know before retiring the old one (F72).
		return false, fmt.Errorf("sealed under a different master key (or corrupt) — will NOT open with the new key")
	}
	m.WrappedKey = newWrapped
	m.KeyFingerprint = newFP

	// f is now positioned at the ciphertext. Write a fresh file — new header, then the
	// SAME ciphertext copied verbatim — and atomically replace, so the body bytes are
	// unchanged and a crash can never leave a half-written archive.
	tmp, err := os.CreateTemp(dir, name+".rewrap-*")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if err := writeHeader(tmp, m); err != nil {
		return false, err
	}
	if _, err := io.Copy(tmp, f); err != nil { // ciphertext, byte-for-byte
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	_ = os.Chmod(tmpName, 0o600)
	if err := os.Rename(tmpName, path); err != nil {
		return false, err
	}
	committed = true

	// Best-effort: reflect the new key fingerprint in the non-sensitive sidecar so the
	// UI shows the backup now belongs to the new key.
	updateMetaFingerprint(dir, name, newFP)
	return true, nil
}

// updateMetaFingerprint rewrites the KeyFingerprint in a backup's .meta.json sidecar
// (best-effort; the sidecar holds only non-sensitive metadata).
func updateMetaFingerprint(dir, name, fp string) {
	metaPath := filepath.Join(dir, name+".meta.json")
	mb, err := os.ReadFile(metaPath)
	if err != nil {
		return
	}
	var e Entry
	if json.Unmarshal(mb, &e) != nil {
		return
	}
	e.KeyFingerprint = fp
	if nb, err := json.MarshalIndent(e, "", "  "); err == nil {
		_ = os.WriteFile(metaPath, nb, 0o600)
	}
}

// Restore decrypts and validates an archive from src, then stages the embedded
// database at <dataDir>/restore.db for the next startup to apply (see
// store.ApplyPendingRestore). It returns the manifest. It never touches the live
// DB directly — staging + restart is the only safe swap. tmpDir must be writable.
func Restore(src io.Reader, key []byte, dataDir, tmpDir string) (Manifest, error) {
	var m Manifest

	// Dispatch on the header: v2 unwraps a per-backup DEK, v1 uses the master key.
	stream, decKey, hdr, oerr := openArchive(src, key)
	if oerr != nil {
		return m, fmt.Errorf("decrypt failed — wrong encryption key or corrupt file")
	}

	tmpTar, err := os.CreateTemp(tmpDir, "cfgrestore-*.tar")
	if err != nil {
		return m, err
	}
	defer os.Remove(tmpTar.Name())
	defer tmpTar.Close()

	if err := crypto.Decrypt(tmpTar, stream, decKey); err != nil {
		return m, fmt.Errorf("decrypt failed — wrong encryption key or corrupt file")
	}
	if _, err := tmpTar.Seek(0, io.SeekStart); err != nil {
		return m, err
	}

	staged := filepath.Join(dataDir, "restore.db.tmp")
	_ = os.Remove(staged)
	var sawDB bool
	tr := tar.NewReader(tmpTar)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			os.Remove(staged)
			return m, fmt.Errorf("archive read: %w", err)
		}
		switch filepath.Base(h.Name) {
		case manifestName:
			if err := json.NewDecoder(tr).Decode(&m); err != nil {
				os.Remove(staged)
				return m, fmt.Errorf("manifest: %w", err)
			}
		case dbName:
			out, err := os.Create(staged)
			if err != nil {
				return m, err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				os.Remove(staged)
				return m, err
			}
			out.Close()
			sawDB = true
		}
	}

	if m.Format != Format {
		os.Remove(staged)
		return m, fmt.Errorf("not a DockBack application backup")
	}
	if !sawDB {
		os.Remove(staged)
		return m, fmt.Errorf("archive is missing %s", dbName)
	}
	if err := store.ValidateDBFile(staged); err != nil {
		os.Remove(staged)
		return m, err
	}
	if err := os.Rename(staged, filepath.Join(dataDir, "restore.db")); err != nil {
		os.Remove(staged)
		return m, err
	}
	if hdr != nil {
		// v2: the plaintext header is authoritative (its fingerprint/wrapped key track
		// the current master key after any rotation). The inner manifest was validated
		// above to prove the decrypted content is a config archive. Don't hand the
		// wrapped key back to callers/UI.
		hdr.WrappedKey = ""
		return *hdr, nil
	}
	return m, nil
}
