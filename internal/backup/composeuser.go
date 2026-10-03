package backup

import (
	"fmt"
	"strings"

	"github.com/docker/docker/api/types"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// Host-specific ids a container carries, in the two places it can declare them
// (#24, #5).
//
// A host-specific uid hardcoded in the stack file (#24).
//
// R3 §Issue 24: `user: 1026:100` on Nextcloud's redis — "Synology's uid/gid,
// hardcoded in compose. On the target, 1026 does not exist." R4 found the same
// three numbers again on redis, tika and gotenberg.
//
// The finding exists because this model is invisible from the two places an
// operator looks. It is not in the image, and it is not in the environment; it
// is one line in a compose file that DockBack reproduces verbatim, and the
// symptom on the target is an application that starts and then cannot read its
// own data.
//
// F117 aligns the FILES to the ids the target declares. That is the other half:
// without this, a restore chowns the data to 1000 and leaves the process running
// as 1026, which is the same mismatch pointing the other way.

// idConventions name the numbering schemes that never match a stock Linux host,
// as PLAYBOOK §7.2 lists them: "Synology 1026+/100 (`users`), Unraid 99:100,
// QNAP. Detect and flag them."
//
// Recognising the number is what turns the finding from "check this id" into
// "this is your NAS's id, and the machine you are restoring onto is not it".
func idConvention(uid, gid int) string {
	switch {
	case uid >= 1026 && gid == 100:
		return "Synology numbers its users from 1026 and puts them in group 100 (`users`), so this looks like an id from a DSM machine"
	case uid == 99 && gid == 100:
		return "99:100 is Unraid's `nobody`/`users` pair"
	case uid >= 1000000:
		return "an id this large is usually a container-runtime subordinate id, which is meaningful only inside the namespace that allocated it"
	}
	return ""
}

// hasDataMount reports whether this container has anything to own — a named
// volume, or a host bind that is real application data.
//
// It decides which half of the finding applies. R4: "The `user: 1026:100`
// override on tika and gotenberg bought nothing — neither has a volume — but
// would still have failed on a host without uid 1026."
func hasDataMount(insp types.ContainerJSON) bool {
	for _, m := range insp.Mounts {
		switch string(m.Type) {
		case "volume":
			return true
		case "bind":
			if m.Source != "" && backupableBind(m.Source) {
				return true
			}
		}
	}
	return false
}

// restatesImageUser reports whether this container's `user:` says nothing the
// image did not already say.
//
// Two ways to be the same: the literal strings match, or both are numeric and
// name the same uid. An image declaring a NAME (`USER redis`) against a
// container declaring that account's number is NOT provably the same — resolving
// it needs the image's own /etc/passwd — so it is treated as a real override.
// That costs an occasional finding on a container restating its image's uid by
// number, which is a cheap wrong answer next to reading a passwd file out of
// every image at backup time.
func restatesImageUser(containerUser string, uid int, man *Manifest) bool {
	if man == nil || man.ImageConfig == nil {
		return false
	}
	imageUser := strings.TrimSpace(man.ImageConfig.User)
	if imageUser == "" {
		return false
	}
	if imageUser == containerUser {
		return true
	}
	imageUID, _, numeric := dockercli.NumericUser(imageUser)
	return numeric && imageUID == uid
}

// reportComposeUser records a `user:` override expressed as raw ids.
//
// Silent for the cases that are portable by construction: no override at all,
// a named user, root, and a container merely restating what its image already
// runs as. Every one of those means the same thing on the target machine, and a
// finding about them would be noise on top of the ones that matter.
func (e *Engine) reportComposeUser(man *Manifest, logID, name string, insp types.ContainerJSON) {
	if insp.Config == nil {
		return
	}
	user := strings.TrimSpace(insp.Config.User)
	uid, gid, numeric := dockercli.NumericUser(user)
	if !numeric || uid == 0 || restatesImageUser(user, uid, man) {
		return
	}

	lead := fmt.Sprintf("%s is pinned to the raw id %s by a `user:` line in its stack file — the third way a container can say which user it runs as, after PUID/PGID in the environment and USER in the image, and the only one that lives outside both. ", name, user)
	if convention := idConvention(uid, gid); convention != "" {
		lead += convention + ", and a plain Linux host will not have that account. "
	} else {
		lead += "That number means whatever the machine it was written on says it means, and nothing on any other machine. "
	}

	if !hasDataMount(insp) {
		e.addFinding(man, logID, findingComposeUserHostSpecific, FindingWarn, name, lead+
			"This container has no volumes or data binds, so the override owns nothing and buys nothing — it only constrains where this stack can run. "+
			"The fix is to delete the `user:` line and let the image's own user apply; there is no data to keep in step with it.")
		return
	}

	e.addFinding(man, logID, findingComposeUserHostSpecific, FindingWarn, name, lead+
		"Restoring this backup reproduces that line exactly, so on a host without that account the application runs as a number with no passwd entry, against data owned by somebody else. "+
		"Aligning the restored FILES is only half of it — the process has to agree. Set this container's restore ownership to the ids it should run as here and DockBack rewrites the `user:` line to match at the same time it chowns the data; leave it unset and the line is reproduced as recorded, exactly as it is now. "+
		dropTheLineHint(man))
}

// dropTheLineHint offers the other fix — the one taken in both migrations that
// hit this — naming the image's own user when the image states one.
//
// When it does not, the sentence stops rather than gesturing at a value nobody
// can see: an image that declares no USER usually drops privileges in its own
// entrypoint, so naming a number here would be a guess dressed as a fact.
func dropTheLineHint(man *Manifest) string {
	const lead = "Deleting the `user:` line is the other way, and usually the right one"
	if man != nil && man.ImageConfig != nil {
		if u := strings.TrimSpace(man.ImageConfig.User); u != "" {
			return lead + ": the image runs as " + u + ", so chown the data to that instead."
		}
	}
	return lead + " — the image then runs as whatever user it was built to use, and the data should be chowned to match it. Check with `docker exec <container> id` once it is up."
}

// The same question of the OTHER model (#5).
//
// The env-configurable pair is already handled end to end — F117 aligns the
// restored files to it, F189 rewrites it to a pin, and #5's semantic matching
// now finds it whatever an image chose to call it. What none of that does is
// say, at backup time, that the numbers themselves came from a NAS.
//
// Which matters because the machinery works perfectly and still leaves the
// operator somewhere they may not want to be: the restore reproduces
// USERMAP_UID=1026, chowns the data to 1026, and the two agree — on a host where
// no account 1026 exists. Nothing is broken; nothing is native either. Saying so
// once, at info level, is what turns that into a decision.

// reportRunAsConvention names a run-as pair whose ids belong to a numbering
// scheme no ordinary Linux host uses.
//
// Info, not warn, and the difference is the point: unlike a `user:` override
// this is configuration DockBack can and does correct, given a pin. The finding
// exists to tell the operator the pin is worth setting, not that anything failed.
//
// Silent for every ordinary id. PLAYBOOK §7.2 lists the schemes worth naming and
// this reports only those — a finding on every container that sets PUID=1000
// would bury the ones that mean something.
func (e *Engine) reportRunAsConvention(man *Manifest, logID, name string, insp types.ContainerJSON) {
	if insp.Config == nil {
		return
	}
	uid, gid, key, ok := dockercli.RunAsIDs(insp.Config.Env)
	if !ok {
		return
	}
	convention := idConvention(uid, gid)
	if convention == "" {
		return
	}
	e.addFinding(man, logID, findingRunAsConventionForeign, FindingInfo, key, fmt.Sprintf(
		"%s tells its image to run as %d:%d through %s — and %s. Nothing is wrong: this is the model DockBack can correct, so a restore reproduces the pair and chowns the restored files to match it, and the application and its data agree wherever they land. "+
			"What they agree on is a number from the other machine. If you would rather this container used an account that exists on the host it is restored to, set its restore ownership — DockBack then rewrites %s and chowns the data to the new ids together, in one restore.",
		name, uid, gid, key, convention, key))
}

// applyRunAsUser points a numeric `user:` override at the pinned ids (#24).
//
// The half F189 never covered: it rewrites the ENVIRONMENT pair an image
// declares, and redis, tika and gotenberg declare no such pair — their id comes
// from the stack file, so there was nothing for it to find and nothing changed.
//
// Only ever on an explicit pin. Without one there is no answer to rewrite TO,
// and inventing one is how an application ends up unable to read the files this
// same restore just gave it.
func (e *Engine) applyRunAsUser(b *store.Backup, inspectBytes []byte, uid, gid int) ([]byte, bool) {
	was := dockercli.ConfigUser(inspectBytes)
	out, changed, err := dockercli.RewriteConfigUser(inspectBytes, uid, gid)
	if err != nil {
		e.logf(b.ID, "WARN", "Could not set the `user:` override on the recreated container (%v) — it keeps the recorded %q, which is an id from the machine this backup came from", err, was)
		return inspectBytes, false
	}
	if !changed {
		return inspectBytes, false
	}
	e.logf(b.ID, "INFO", "Rewrote this container's `user:` override from %s to %d:%d, matching the ownership set for it — the recorded id belongs to the machine the backup came from, and the process now agrees with the files this restore is about to chown.", was, uid, gid)
	return out, true
}

// reportCarriedRunAsEnv says, at restore, that the env-pair model is carrying
// the previous machine's ids onto this one (#N8).
//
// The twin of reportCarriedComposeUser below, for the OTHER of the two writable
// UID models. That one covers a `user:` line in the stack file; this one covers
// PUID/PGID and its four spellings — the model the EACCES incident was actually
// using, and the one nothing said anything about at restore time.
//
// Cross-host and unpinned only, and only for ids that belong to a numbering
// scheme no ordinary Linux host uses. A same-host restore keeps the operator's
// own account numbers and needs no comment; a pinned one has already been
// answered and the rewrite logs its own line; an ordinary 1000:1000 is not news
// on any machine.
func (e *Engine) reportCarriedRunAsEnv(b *store.Backup, man *Manifest, inspectBytes []byte, opts RestoreOptions) {
	if !crossHostRestore(b, man, opts) {
		return
	}
	uid, gid, key, ok := dockercli.RunAsIDs(dockercli.ContainerEnv(inspectBytes))
	if !ok {
		return
	}
	convention := idConvention(uid, gid)
	if convention == "" {
		return
	}
	e.logf(b.ID, "WARN", "%s is being recreated with %s=%d:%d, exactly as recorded — but this is a different machine, and %s. "+
		"The restored files are owned by those ids too, so the application will start and work; what it will not be is native to this host, and changing PUID/PGID later re-owns only the top-level mount directories, leaving everything beneath them unreadable to the new user. "+
		"To have DockBack use this host's ids instead, set this container's restore ownership on its container page; it then rewrites %s and chowns the data together.",
		b.TargetName, key, uid, gid, convention, key)
}

// reportCarriedComposeUser says, at restore, that a host-specific `user:` is
// being reproduced onto a different machine with nothing to correct it.
//
// Cross-host and unpinned only. Reproducing it is the correct default (#31) —
// the operator may well have created that account here — but a move with no pin
// is the one combination where the recorded id is most likely wrong and nothing
// downstream will notice.
func (e *Engine) reportCarriedComposeUser(b *store.Backup, man *Manifest, inspectBytes []byte, opts RestoreOptions) {
	if !crossHostRestore(b, man, opts) {
		return
	}
	user := dockercli.ConfigUser(inspectBytes)
	uid, _, numeric := dockercli.NumericUser(user)
	if !numeric || uid == 0 {
		return
	}
	e.logf(b.ID, "WARN", "%s is being recreated with `user: %s`, exactly as recorded — but this is a different machine, and that id was written for the previous one. "+
		"If no such account exists here the application runs as a bare number with no passwd entry, against data this restore owns to somebody else. "+
		"To have DockBack rewrite it, set this container's restore ownership to the ids it should use on this host; it then changes the `user:` line and chowns the data together.",
		b.TargetName, user)
}
