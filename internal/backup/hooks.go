package backup

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
)

// Hook is a single command run inside the target container around a backup
// (application-aware quiesce, PLAN §9.5).
type Hook struct {
	Cmd     []string
	User    string
	WorkDir string
	Ignore  bool // if true, a failure is logged but doesn't fail the backup
}

// SavedHooks is the per-container custom hook config stored in settings.
type SavedHooks struct {
	Pre  []string `json:"pre"`  // shell command lines run before the snapshot
	Post []string `json:"post"` // shell command lines run after
	User string   `json:"user"` // optional run-as user for custom hooks
	// PostRestore runs after a RESTORE has put the data back and started the
	// container, before the health gate judges it (F140). Added after Pre/Post,
	// so a container with hooks saved before this existed unmarshals unchanged
	// and simply has none.
	PostRestore []string `json:"post_restore,omitempty"`
}

// HooksSettingKey is where a container's custom hooks live (keyed by node+name).
func HooksSettingKey(nodeID, name string) string { return "hooks:" + nodeID + ":" + name }

// AutoHookLabels describes the built-in presets that will apply to a container,
// for display in the UI.
func AutoHookLabels(image string, isDB bool) []string {
	var out []string
	// F121: an embedded database DockBack has no dump tool for (H2 and friends —
	// NOT SQLite, which has its own consistent-snapshot path). The capture works,
	// but only because the container is quiesced for the copy, and nothing
	// otherwise says so. Listed first: it changes what the pause setting below it
	// is actually protecting.
	p := ProfileFor(image)
	if p != nil && p.EmbeddedDBWarning != "" {
		out = append(out, p.EmbeddedDBWarning)
	}
	// F122: an app whose archive is a credential store for OTHER systems. Said at
	// backup-configuration time because that is where write-only mode is chosen —
	// after the fact it is only a regret.
	// F135: sibling services holding only derived data. Said at
	// backup-configuration time because that is where the selection is made.
	if p != nil && p.DerivedServices != "" {
		out = append(out, p.DerivedServices)
	}
	if p != nil && p.CredentialStore != "" {
		out = append(out, "High-value archive: "+p.CredentialStore+". Turn on write-only encryption so the archive can only be opened with an offline key held away from these machines.")
	}
	if isNextcloud(image) {
		out = append(out, "Nextcloud maintenance mode (on → backup → off)")
		// F140: the other half, and the one that makes a restore possible at all.
		out = append(out, "Nextcloud post-restore: maintenance mode off + data-fingerprint (a backup is captured IN maintenance mode, so a restore must take it back out or every page returns 503)")
	}
	// F146: an application whose services are only meaningful together. Said at
	// backup-configuration time because that is where the choice between "back up
	// this container" and "back up this stack" is actually made — and for this
	// class of app the first one produces archives that each verify perfectly and
	// cannot restore a working deployment.
	if p != nil && p.StackAtomic != nil {
		out = append(out, p.StackAtomic.Why+". Use the stack's app-consistent snapshot rather than per-service backups: "+p.StackAtomic.Symptom+".")
	} else if isPangolin(image) {
		out = append(out, "Pangolin: full stop during snapshot for a clean embedded-SQLite copy — back up gerbil + traefik in the same stack")
	}
	// F157: an application that stages its writes is captured correctly by a
	// paused copy — and a backup of it is the moment somebody finds out about the
	// staging directories a past interruption left behind.
	if p != nil && len(p.StagingDebris) > 0 {
		out = append(out, p.Name+" writes by staging each change and renaming it into place, so a paused copy captures a consistent tree. "+
			"DockBack also reports any staging directory left behind by an interrupted write — it never removes one.")
	}
	if isDB {
		out = append(out, "Native database dump (application-consistent)")
	}
	// F139: Mealie writes its own ZIP exports into the data directory, and it is
	// easy to assume those are the restore path. They are not — the JSON inside
	// them is schema-version-sensitive and has broken across major versions.
	// DockBack captures the database and files directly, which is exact and
	// version-matched; the ZIP is worth keeping only as something a human can
	// open.
	if strings.Contains(strings.ToLower(image), "mealie") {
		out = append(out, "Mealie's database and data directory are captured directly, which is what a restore uses. Its own ZIP exports are kept as a portable extra — their JSON import is version-sensitive and is not the restore path.")
	}
	if strings.Contains(strings.ToLower(image), "paperless") {
		out = append(out, "Tip: add a pre-hook `document_exporter /usr/src/paperless/export` for a portable export")
	}
	return out
}

func isNextcloud(image string) bool { return strings.Contains(strings.ToLower(image), "nextcloud") }

// gatherRestoreHooks builds the commands to run after a RESTORE has put the data
// back and started the container, before the health gate judges it (F140).
//
// This closes a gap that made Nextcloud restores impossible. The backup hook puts
// the instance into maintenance mode so the files and the database are captured
// as a matched set — which means `config.php` is captured saying
// `'maintenance' => true`. Restoring it faithfully puts the instance straight
// back into maintenance mode: every page returns 503, the container's own
// healthcheck fails, and the gate correctly rolls the whole thing back. The
// backup was perfect and the restore could never succeed.
//
// The window matters: after the container is running (occ needs its database)
// and before the gate (which is what the maintenance flag defeats).
func (e *Engine) gatherRestoreHooks(image, nodeID, name string) []Hook {
	var out []Hook
	if isNextcloud(image) {
		occ := func(ignore bool, args ...string) Hook {
			return Hook{Cmd: append([]string{"php", "occ"}, args...), User: "www-data", WorkDir: "/var/www/html", Ignore: ignore}
		}
		// Idempotent: turning maintenance off when it is already off is a no-op,
		// so a re-run of a restore converges rather than erroring.
		out = append(out, occ(true, "maintenance:mode", "--off"))
		// Tells desktop and mobile clients the server state came from a backup, so
		// they re-sync rather than assuming their local copy is newer and
		// "helpfully" restoring deleted files. Best-effort — a missing fingerprint
		// is untidy, not broken.
		out = append(out, occ(true, "maintenance:data-fingerprint"))
	}

	// Custom per-container post-restore lines, appended after the built-ins so an
	// operator's own step runs against an app that is already out of maintenance.
	// Same trust model as the pre/post backup hooks: admin-authored, behind the
	// auth+CSRF setter.
	if js, _ := e.Store.GetSetting(HooksSettingKey(nodeID, name), ""); js != "" {
		var sh SavedHooks
		if json.Unmarshal([]byte(js), &sh) == nil {
			for _, line := range sh.PostRestore {
				if strings.TrimSpace(line) != "" {
					out = append(out, Hook{Cmd: []string{"/bin/sh", "-c", line}, User: sh.User, Ignore: true})
				}
			}
		}
	}
	return out
}

// runRestoreHooks executes the post-restore hooks for a restored container.
//
// Every hook is Ignore=true — a failure is logged loudly with the command that
// fixes it, and never fails the restore itself. That is deliberate, and it is a
// narrower choice than it looks:
//
// The SAME Nextcloud image runs both the web service and the cron service. Only
// the web container has a healthcheck; the cron container legitimately may not be
// able to run occ at all. Hard-failing a restore because occ did not run in the
// cron container would break restores that are entirely fine.
//
// Nothing is lost by not failing here. A web container still in maintenance mode
// fails its own healthcheck moments later, and the gate rolls back — now with the
// specific reason logged immediately above it instead of a bare "did not become
// healthy". The diagnosis improves; the safety does not change.
func (e *Engine) runRestoreHooks(ctx context.Context, cli *client.Client, backupID, containerID, image, nodeID, name string) {
	hooks := e.gatherRestoreHooks(image, nodeID, name)
	if len(hooks) == 0 {
		return
	}
	e.logf(backupID, "INFO", "Running %d post-restore step(s) for %s", len(hooks), name)
	for _, h := range hooks {
		if err := e.runHook(ctx, cli, containerID, h, backupID, "post-restore"); err != nil {
			e.logf(backupID, "ERR", "Post-restore step failed: %s — run it by hand with: docker exec -u %s %s %s",
				strings.Join(h.Cmd, " "), h.User, name, strings.Join(h.Cmd, " "))
		}
	}
}

// gatherHooks builds the pre/post hooks for a backup: built-in image presets
// plus the user's custom hooks for this container.
func (e *Engine) gatherHooks(image, nodeID, name string) (pre, post []Hook) {
	// Built-in preset: Nextcloud maintenance mode brackets the snapshot so the
	// files and database are a matched set. Best-effort (Ignore) so backing up a
	// non-app Nextcloud container (e.g. the cron service) never fails.
	if isNextcloud(image) {
		nc := func(on bool) Hook {
			mode := "--off"
			if on {
				mode = "--on"
			}
			return Hook{Cmd: []string{"php", "occ", "maintenance:mode", mode}, User: "www-data", WorkDir: "/var/www/html", Ignore: true}
		}
		pre = append(pre, nc(true))
		post = append(post, nc(false))
	}

	// Custom per-container hooks. By design these run an admin-authored shell line
	// inside the container with its privileges (operator-level ACE) — it is not an
	// external-attacker vector: the setter endpoint is auth+CSRF-gated and only the
	// signed-in admin can define them (SEC-7, documented not "fixed").
	if js, _ := e.Store.GetSetting(HooksSettingKey(nodeID, name), ""); js != "" {
		var sh SavedHooks
		if json.Unmarshal([]byte(js), &sh) == nil {
			for _, line := range sh.Pre {
				if strings.TrimSpace(line) != "" {
					pre = append(pre, Hook{Cmd: []string{"/bin/sh", "-c", line}, User: sh.User})
				}
			}
			for _, line := range sh.Post {
				if strings.TrimSpace(line) != "" {
					post = append(post, Hook{Cmd: []string{"/bin/sh", "-c", line}, User: sh.User, Ignore: true})
				}
			}
		}
	}
	return pre, post
}

// runHook executes one hook, honoring its Ignore flag.
func (e *Engine) runHook(ctx context.Context, cli *client.Client, containerID string, h Hook, backupID, phase string) error {
	e.logf(backupID, "INFO", "%s-hook: %s", phase, strings.Join(h.Cmd, " "))
	if _, err := dockercli.ExecHook(ctx, cli, containerID, h.Cmd, h.User, h.WorkDir); err != nil {
		if h.Ignore {
			e.logf(backupID, "WARN", "%s-hook failed (ignored): %v", phase, err)
			return nil
		}
		return err
	}
	return nil
}
