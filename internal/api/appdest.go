package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"dockback/internal/appbackup"
	"dockback/internal/backup"
	"dockback/internal/storage"
	"dockback/internal/store"
	"dockback/internal/version"
)

// appRemotePrefix is the folder under each app-backup destination where the
// app's own backups are stored (kept apart from any container backups).
const appRemotePrefix = "dockback-config"

type appDestView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Enabled   bool   `json:"enabled"`
	Reachable bool   `json:"reachable"`
}

// handleAppDestList lists the app-backup destinations with a live reachability dot.
func (s *Server) handleAppDestList(w http.ResponseWriter, r *http.Request) {
	ds, err := s.store.ListAppDestinations()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]*appDestView, len(ds))
	var wg sync.WaitGroup
	for i, d := range ds {
		out[i] = &appDestView{ID: d.ID, Name: d.Name, Type: d.Type, Enabled: d.Enabled}
		cfg, err := s.decryptDestConfig(d)
		if err != nil {
			continue
		}
		b, err := storage.NewFromConfig(d.Type, cfg)
		if err != nil {
			continue
		}
		wg.Add(1)
		go func(v *appDestView, b storage.Backend) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
			defer cancel()
			v.Reachable = storage.Reachable(ctx, b) == nil
		}(out[i], b)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, out)
}

// handleAppDestAdd stores a new app-backup destination (credentials encrypted).
func (s *Server) handleAppDestAdd(w http.ResponseWriter, r *http.Request) {
	var req destReq
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Name == "" || req.Type == "" {
		errJSON(w, http.StatusBadRequest, "name and type are required")
		return
	}
	if _, err := storage.NewFromConfig(req.Type, req.Config); err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	// F66: an SFTP destination pins its host key at save time (TOFU) — refuse the
	// save when the host can't be reached to pin. This archive carries every node
	// credential, so an unauthenticated SSH host is the one destination that must
	// not be accepted on trust alone.
	if err := s.pinSFTPHostKey(r.Context(), req.Type, req.Config); err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	enc, err := s.encryptDestConfig(req.Config)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	d := &store.Destination{ID: randToken()[:12], Name: req.Name, Type: req.Type, Enabled: true, Status: "unknown", ConfigEnc: enc}
	if err := s.store.CreateAppDestination(d); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "app.dest.add", req.Name, req.Type)
	writeJSON(w, http.StatusOK, map[string]string{"id": d.ID})
}

// handleAppDestToggle enables/disables an app-backup destination.
func (s *Server) handleAppDestToggle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := s.store.SetAppDestinationEnabled(r.PathValue("id"), req.Enabled); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": req.Enabled})
}

// handleAppDestDelete removes an app-backup destination (its stored backups are
// left in place on the remote).
func (s *Server) handleAppDestDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteAppDestination(r.PathValue("id")); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "app.dest.delete", r.PathValue("id"), "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// errNoEnabledAppDest signals that a push was requested but no app-backup
// destination is enabled (a caller-distinguishable, non-fatal condition).
var errNoEnabledAppDest = errors.New("no enabled external destinations")

// appExternalBackupCore snapshots the app state, builds the encrypted archive
// once, and uploads it to every enabled app-backup destination. Shared by the
// manual "Back up to external now" handler and the automatic schedule (F4), so
// both take an identical archive and push. Best-effort per destination: a
// failure is recorded in `failed` rather than aborting the batch. Each upload is
// bounded by a 10-minute per-destination timeout derived from parent.
func (s *Server) appExternalBackupCore(parent context.Context) (fname string, pushed []string, failed map[string]string, err error) {
	ds, lerr := s.store.ListAppDestinations()
	if lerr != nil {
		return "", nil, nil, lerr
	}
	enabled := make([]*store.Destination, 0, len(ds))
	for _, d := range ds {
		if d.Enabled {
			enabled = append(enabled, d)
		}
	}
	if len(enabled) == 0 {
		return "", nil, nil, errNoEnabledAppDest
	}

	// Build the encrypted archive once into a temp file, then upload to each.
	snap := filepath.Join(s.cfg.TmpDir, fmt.Sprintf("cfgsnap-%d.db", time.Now().UnixNano()))
	defer os.Remove(snap)
	if err := s.store.SnapshotTo(snap); err != nil {
		return "", nil, nil, fmt.Errorf("snapshot failed: %w", err)
	}
	arch := filepath.Join(s.cfg.TmpDir, fmt.Sprintf("cfgarch-%d.dback", time.Now().UnixNano()))
	defer os.Remove(arch)
	af, cerr := os.Create(arch)
	if cerr != nil {
		return "", nil, nil, fmt.Errorf("archive failed: %w", cerr)
	}
	m := appbackup.Manifest{Format: appbackup.Format, Version: appbackup.Version, CreatedAt: time.Now().Unix(), AppVersion: version.Version, KeyFingerprint: s.engine.MasterKeyFP()}
	if aerr := appbackup.Create(af, snap, s.cfg.EncryptionKey, m); aerr != nil {
		af.Close()
		return "", nil, nil, fmt.Errorf("archive failed: %w", aerr)
	}
	af.Close()

	fname = fmt.Sprintf("dockback-config-%s.dback", time.Now().UTC().Format("20060102-150405"))
	key := appRemotePrefix + "/" + fname
	pushed = []string{}
	failed = map[string]string{}
	for _, d := range enabled {
		cfg, derr := s.decryptDestConfig(d)
		if derr != nil {
			failed[d.Name] = "decrypt config"
			continue
		}
		b, berr := storage.NewFromConfig(d.Type, cfg)
		if berr != nil {
			failed[d.Name] = berr.Error()
			continue
		}
		f, ferr := os.Open(arch)
		if ferr != nil {
			failed[d.Name] = ferr.Error()
			continue
		}
		ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
		_, perr := b.Put(ctx, key, f)
		cancel()
		f.Close()
		if perr != nil {
			failed[d.Name] = perr.Error()
			continue
		}
		s.persistLearnedAppDestHostKey(d, b, cfg)
		pushed = append(pushed, d.Name)
	}
	return fname, pushed, failed, nil
}

// handleAppExternalBackup snapshots the app state and uploads the encrypted
// archive to every enabled app-backup destination.
func (s *Server) handleAppExternalBackup(w http.ResponseWriter, r *http.Request) {
	fname, pushed, failed, err := s.appExternalBackupCore(r.Context())
	if err == errNoEnabledAppDest {
		errJSON(w, http.StatusBadRequest, "no enabled external destinations")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "app.external.backup", fname, fmt.Sprintf("pushed=%d failed=%d", len(pushed), len(failed)))
	writeJSON(w, http.StatusOK, map[string]any{"file": fname, "pushed": pushed, "failed": failed})
}

type remoteEntry struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

// handleAppExternalList lists app backups stored on a given destination.
func (s *Server) handleAppExternalList(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.GetAppDestination(r.PathValue("id"))
	if err != nil {
		errJSON(w, http.StatusNotFound, "destination not found")
		return
	}
	cfg, err := s.decryptDestConfig(d)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "decrypt config")
		return
	}
	b, err := storage.NewFromConfig(d.Type, cfg)
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	keys, err := storage.ListKeys(ctx, b, appRemotePrefix)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	s.persistLearnedAppDestHostKey(d, b, cfg)
	out := []remoteEntry{}
	for _, k := range keys {
		name := filepath.Base(k)
		if appbackup.ValidName(name) {
			out = append(out, remoteEntry{Key: k, Name: name})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// rewrapAppDestBackups re-wraps every remote app-config archive on the ENABLED
// app-backup destinations onto the new master key (F72): download → re-wrap the
// small envelope header (ciphertext untouched) → re-upload under the same key
// name. Called AFTER the in-memory key switch, so the just-re-sealed destination
// credentials open with the current key; the archive re-wrap itself uses the
// explicit old/new keys. Best-effort: every failure is counted for the rotation
// warnings (remote copies are re-creatable via "Back up to destinations now");
// v1 (pre-envelope) archives are left untouched and not counted either way.
func (s *Server) rewrapAppDestBackups(ctx context.Context, oldKey, newKey []byte) (rewrapped, failed int) {
	ds, err := s.store.ListAppDestinations()
	if err != nil {
		return 0, 0
	}
	newFP := backup.KeyFingerprint(newKey)
	tmp, err := os.MkdirTemp(s.cfg.TmpDir, "rewrap-appdest-*")
	if err != nil {
		return 0, 0
	}
	defer os.RemoveAll(tmp)

	for _, d := range ds {
		if !d.Enabled {
			continue
		}
		cfg, derr := s.decryptDestConfig(d)
		if derr != nil {
			s.logSink("security", "WARN", "Key rotation: cannot open app-destination "+d.Name+" config: "+derr.Error())
			failed++
			continue
		}
		be, berr := storage.NewFromConfig(d.Type, cfg)
		if berr != nil {
			s.logSink("security", "WARN", "Key rotation: cannot reach app-destination "+d.Name+": "+berr.Error())
			failed++
			continue
		}
		lctx, lcancel := context.WithTimeout(ctx, 30*time.Second)
		keys, kerr := storage.ListKeys(lctx, be, appRemotePrefix)
		lcancel()
		if kerr != nil {
			s.logSink("security", "WARN", "Key rotation: cannot list app-backups on "+d.Name+": "+kerr.Error())
			failed++
			continue
		}
		for _, k := range keys {
			name := filepath.Base(k)
			if !appbackup.ValidName(name) {
				continue
			}
			changed, rerr := s.rewrapOneRemote(ctx, be, k, name, tmp, newFP, oldKey, newKey)
			switch {
			case rerr != nil:
				s.logSink("security", "WARN", "Key rotation: remote app-backup "+d.Name+"/"+name+": "+rerr.Error())
				failed++
			case changed:
				rewrapped++
			}
		}
	}
	return rewrapped, failed
}

// rewrapOneRemote downloads one remote app-backup, re-wraps its envelope onto
// the new key, and re-uploads it in place. changed=false with a nil error means
// there was nothing to do (v1 archive, or already on the new key).
func (s *Server) rewrapOneRemote(ctx context.Context, be storage.Backend, key, name, tmp, newFP string, oldKey, newKey []byte) (changed bool, err error) {
	fctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	rc, err := be.Get(fctx, key)
	if err != nil {
		return false, fmt.Errorf("download: %w", err)
	}
	local := filepath.Join(tmp, name)
	f, err := os.Create(local)
	if err != nil {
		rc.Close()
		return false, err
	}
	_, cerr := io.Copy(f, rc)
	rc.Close()
	f.Close()
	defer os.Remove(local)
	defer os.Remove(local + ".meta.json") // best-effort sidecar RewrapFile may touch
	if cerr != nil {
		return false, fmt.Errorf("download: %w", cerr)
	}
	changed, err = appbackup.RewrapFile(tmp, name, newFP, oldKey, newKey)
	if err != nil || !changed {
		return changed, err
	}
	up, err := os.Open(local)
	if err != nil {
		return false, err
	}
	defer up.Close()
	if _, err := be.Put(fctx, key, up); err != nil {
		return false, fmt.Errorf("re-upload: %w", err)
	}
	return true, nil
}

// handleAppExternalRestore downloads a backup from a destination and restores it.
func (s *Server) handleAppExternalRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		File string `json:"file"`
		stepUpBody
	}
	if err := readJSON(r, &req); err != nil || !appbackup.ValidName(req.File) {
		errJSON(w, http.StatusBadRequest, "invalid backup name")
		return
	}
	// F199: same gate as the local restore — this one also pulls the archive
	// across the network before replacing the control plane with it.
	if !s.requireFreshAuth(w, r, req.stepUpBody) {
		return
	}
	d, err := s.store.GetAppDestination(r.PathValue("id"))
	if err != nil {
		errJSON(w, http.StatusNotFound, "destination not found")
		return
	}
	cfg, err := s.decryptDestConfig(d)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "decrypt config")
		return
	}
	b, err := storage.NewFromConfig(d.Type, cfg)
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	rc, err := b.Get(ctx, appRemotePrefix+"/"+req.File)
	if err != nil {
		errJSON(w, http.StatusBadGateway, "could not download: "+err.Error())
		return
	}
	defer rc.Close()
	// Recorded before the restore replaces the database: if the restore fails the
	// pin survives, and if it succeeds the restored row carries its own.
	s.persistLearnedAppDestHostKey(d, b, cfg)
	m, err := appbackup.Restore(rc, s.cfg.EncryptionKey, s.cfg.DataDir, s.cfg.TmpDir)
	if err != nil {
		_ = s.store.Audit(userFrom(r), "app.restore.failed", req.File, "ip="+s.clientIP(r)+" err="+err.Error())
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	s.finishRestore(w, r, "external:"+d.Name+"/"+req.File, m.CreatedAt, m.KeyFingerprint)
}
