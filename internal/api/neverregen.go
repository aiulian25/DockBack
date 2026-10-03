package api

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"dockback/internal/backup"
	"dockback/internal/store"
)

// The restore-side half of #11: refusing to lay data down under a key the target
// no longer has.
//
// R2 §Issue 11's failure arrives from two directions. A capture that misses the
// key is caught at backup time. This is the other one: the key was captured
// correctly, and the CONTAINER being restored into has since been given a
// different one. A volumes-only restore does not touch the environment, so the
// data lands encrypted under a value nothing on this host can read — and the
// application starts, logs in, and looks entirely normal.

// inspectReadTimeout bounds reading one small member out of an archive. The
// stored config sits at the front of the layout, so this never streams a payload.
const inspectReadTimeout = 30 * time.Second

// storedInspect is the shape needed out of config/inspect.json: the environment
// the container was created with.
type storedInspect struct {
	Config struct {
		Env []string `json:"Env"`
	} `json:"Config"`
}

// changedIrreplaceableSecrets names the profile's never-regenerate keys whose
// value on the target differs from the one the backup was taken with.
//
// Returns nothing whenever the comparison cannot be made — an unreadable
// archive, an unreachable node, a profile that declares no such keys. A
// comparison that did not happen must not read as one that failed, and blocking
// a restore on it would be refusing recoveries to guard a guess.
//
// The values themselves exist only inside this function.
func (s *Server) changedIrreplaceableSecrets(ctx context.Context, b *store.Backup, man *backup.Manifest, profile *backup.AppProfile, nodeID, targetID string) []string {
	if profile == nil || len(profile.NeverRegenerate) == 0 {
		return nil
	}
	cli, err := s.reg.Get(nodeID)
	if err != nil {
		return nil
	}
	ictx, icancel := context.WithTimeout(ctx, inspectReadTimeout)
	defer icancel()
	live, err := cli.ContainerInspect(ictx, targetID)
	if err != nil || live.Config == nil {
		return nil
	}

	var stored bytes.Buffer
	if err := s.engine.ExtractOne(ictx, b, "", "config/inspect.json", &stored); err != nil {
		return nil
	}
	var recorded storedInspect
	if json.Unmarshal(stored.Bytes(), &recorded) != nil {
		return nil
	}
	return backup.ChangedNeverRegenerateKeys(profile.NeverRegenerate, recorded.Config.Env, live.Config.Env)
}
