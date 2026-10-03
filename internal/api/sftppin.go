package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"dockback/internal/dockercli"
	"dockback/internal/storage"
	"dockback/internal/store"
)

// SFTP destination host-key pinning (F66). The pin lives INSIDE the
// destination's sealed config (key "host_key"), so it rides the existing
// encrypted config path; the browser only ever sees the SHA256 fingerprint.

// isSFTPType reports whether a destination type is the SSH/SFTP backend.
func isSFTPType(typ string) bool { return typ == "sftp" || typ == "ssh" }

// pinSFTPHostKey performs the trust-on-first-use pin at SAVE time (F66): when
// an SFTP config has no pinned host key yet, dial once, record the presented
// key, and write it into the config map (persisted sealed by the caller).
// Refuses when the host is unreachable — a destination that cannot be pinned
// at creation cannot be trusted for uploads either.
func (s *Server) pinSFTPHostKey(ctx context.Context, typ string, cfg map[string]string) error {
	if !isSFTPType(typ) || strings.TrimSpace(cfg["host_key"]) != "" {
		return nil
	}
	be, err := storage.NewFromConfig(typ, cfg)
	if err != nil {
		return err
	}
	sf, ok := be.(*storage.SFTP)
	if !ok {
		return nil
	}
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := sf.Ping(pctx); err != nil {
		return fmt.Errorf("could not reach the SSH host to pin its key (the host must be reachable when adding): %w", err)
	}
	ak, _, learned := sf.LearnedHostKey()
	if !learned {
		return fmt.Errorf("could not pin the SSH host key")
	}
	cfg["host_key"] = ak
	return nil
}

// addSFTPTestFingerprint folds the host-key fingerprint into a Test-connection
// response — the learned key on a first connect, else the already-pinned one —
// so the dialog can show "Host key pinned: SHA256:…".
func addSFTPTestFingerprint(resp map[string]any, b storage.Backend, cfg map[string]string) {
	sf, ok := b.(*storage.SFTP)
	if !ok {
		return
	}
	if _, fp, learned := sf.LearnedHostKey(); learned {
		resp["host_key_fp"] = fp
		return
	}
	if hk := strings.TrimSpace(cfg["host_key"]); hk != "" {
		if _, _, fp, ok := dockercli.ParseHostKey([]byte(hk)); ok {
			resp["host_key_fp"] = fp
		}
	}
}

// handleResetDestHostKey — POST /api/destinations/{id}/reset-hostkey (F66).
// Clears the pinned SSH host key and immediately re-pins from the live host
// (the operator has judged the change legitimate — e.g. a reinstalled box).
// If the host is unreachable the pin stays CLEARED and the next successful
// Test Connection persists the fresh key.
func (s *Server) handleResetDestHostKey(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.GetDestination(r.PathValue("id"))
	if err != nil {
		errJSON(w, http.StatusNotFound, "destination not found")
		return
	}
	if !isSFTPType(d.Type) {
		errJSON(w, http.StatusBadRequest, "this destination type has no pinned host key")
		return
	}
	cfg, err := s.decryptDestConfig(d)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "could not read destination")
		return
	}
	delete(cfg, "host_key")
	repinned := s.pinSFTPHostKey(r.Context(), d.Type, cfg) == nil && cfg["host_key"] != ""
	enc, err := s.encryptDestConfig(cfg)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	d.ConfigEnc = enc
	d.Status = "unknown"
	if err := s.store.UpdateDestination(d); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	fp := ""
	if repinned {
		if _, _, f, ok := dockercli.ParseHostKey([]byte(cfg["host_key"])); ok {
			fp = f
		}
	}
	_ = s.store.Audit(userFrom(r), "destination.hostkey.reset", d.Name, "repinned="+fmt.Sprint(repinned)+" fp="+fp)
	s.logSink("security", "WARN", "SFTP destination "+d.Name+": pinned host key RESET by the operator"+map[bool]string{true: " and re-pinned as " + fp, false: " — will pin on the next successful Test Connection"}[repinned])
	writeJSON(w, http.StatusOK, map[string]any{"status": "reset", "repinned": repinned, "host_key_fp": fp})
}

// persistLearnedHostKey stores a host key learned on a first successful connect
// into the destination's sealed config when none is pinned yet (F66) — so a
// destination saved while its host was offline, and every destination that
// predates pinning, gains its pin the first time it is actually used.
//
// save receives the ALREADY-SEALED config, so the two kinds of destination
// (container backups and the app's own control-plane backups) differ only in
// which row they write to, and neither can accidentally persist the credentials
// in the clear.
func (s *Server) persistLearnedHostKey(destName string, b storage.Backend, cur map[string]string, save func(configEnc []byte) error) {
	sf, ok := b.(*storage.SFTP)
	if !ok || strings.TrimSpace(cur["host_key"]) != "" {
		return
	}
	ak, fp, learned := sf.LearnedHostKey()
	if !learned {
		return
	}
	cur["host_key"] = ak
	enc, err := s.encryptDestConfig(cur)
	if err != nil {
		return
	}
	if err := save(enc); err != nil {
		return
	}
	s.logSink("security", "INFO", "SFTP destination "+destName+": host key pinned ("+fp+")")
}

// persistLearnedDestHostKey is the container-backup destination's persister.
func (s *Server) persistLearnedDestHostKey(d *store.Destination, b storage.Backend, cur map[string]string) {
	s.persistLearnedHostKey(d.Name, b, cur, func(enc []byte) error {
		d.ConfigEnc = enc
		return s.store.UpdateDestination(d)
	})
}

// persistLearnedAppDestHostKey is the same for an APP-backup destination — the
// one that receives the archive holding every node credential, and which had no
// pinning at all. Existing rows have no pin, so the first use after upgrade
// records one (trust on first use), exactly as the dialog already describes.
func (s *Server) persistLearnedAppDestHostKey(d *store.Destination, b storage.Backend, cur map[string]string) {
	s.persistLearnedHostKey(d.Name, b, cur, func(enc []byte) error {
		return s.store.UpdateAppDestinationConfig(d.ID, enc)
	})
}
