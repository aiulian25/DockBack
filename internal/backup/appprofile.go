package backup

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Application restore profiles (F107).
//
// detectDBEngine answers "is this container a database server?". This answers a
// different question the restore path had no way to ask: "does this APPLICATION
// have preconditions a different host must satisfy before its data means
// anything there?"
//
// Three preconditions matter in practice, and none of them are visible from a
// container inspect:
//
//   - PathEmbedding — the app stores ABSOLUTE CONTAINER PATHS inside its own
//     database. Restore the database onto a host that mounts the same data at a
//     different container destination and every row still points at the old
//     path: the app comes up healthy and reports its whole library as missing.
//     The archive is fine; the deployment is wrong. Only a pre-restore warning
//     that names the recorded destinations catches this.
//
//   - LocalOnly — the app's database must live on LOCAL disk. SQLite over
//     CIFS/NFS breaks the locking its durability depends on, so a restore that
//     lands /config on a network share produces corruption that surfaces days
//     later, far from the restore that caused it.
//
//   - OneWayMigration — the app migrates its schema forward on start and ships
//     no down-migrations. Restoring a NEWER backup into an OLDER image is
//     unrecoverable in place, so it is blocked rather than warned.
//
// Matching is by image substring, the same convention detectDBEngine and
// AutoHookLabels already use. Every consumer is advisory or explicitly
// overridable: a private tag that matches by accident must never trap an
// operator who knows better.
type AppProfile struct {
	// Name is the application as a human writes it, used in every message.
	Name string

	// PathEmbedding, when non-empty, is the reason this app's data is bound to
	// its container mount destinations — shown verbatim in the restore dialog
	// above the list of recorded destinations.
	PathEmbedding string

	// LocalOnly lists container paths whose backing storage must not be a
	// network filesystem on the restore target.
	LocalOnly []string

	// OneWayMigration, when non-empty, is the reason a downgrade cannot be
	// undone — shown when the gate blocks a newer-into-older restore.
	OneWayMigration string

	// NoHealthcheck marks an image that ships no HEALTHCHECK, so Docker reports
	// nothing better than "running" about it.
	//
	// That matters specifically when combined with OneWayMigration: the health
	// gate accepts "running" the instant the container starts, which for an app
	// that migrates its schema at startup is BEFORE the migration has finished.
	// A migration that then fails takes the container down after the restore has
	// already been reported healthy. See migrationSettle in restore.go.
	NoHealthcheck bool

	// Address describes where this application records the address it believes it
	// is served at, and what has to happen to each place on a move (F114). Empty
	// for an app that records its address nowhere.
	Address []AddressBinding

	// EmbeddedDBWarning, when non-empty, warns that this image keeps its state in an
	// embedded database DockBack cannot dump — H2, LevelDB, BoltDB and the like
	// (F121). SQLite is deliberately NOT in that set: it has a first-class
	// consistent-snapshot path, so it needs no warning.
	//
	// The capture still works — the file is archived like any other — but it is
	// only sound if the container is quiesced for the copy, which is the default
	// and which an operator can switch off without realising what it protected.
	// Worth saying out loud precisely because the risk is invisible: same app,
	// same UI, and for CommaFeed only the image TAG distinguishes the deployment
	// that has this problem from the one that doesn't.
	EmbeddedDBWarning string

	// CredentialStore, when non-empty, says what a DECRYPTED archive of this app
	// would grant an attacker (F122).
	//
	// A handful of apps are not "an app with some secrets in it" — their entire
	// purpose is holding credentials to OTHER systems, so their backup is a
	// higher-value target than anything it protects. Dockhand's archive grants
	// Docker-daemon control of every host it manages; Termix's holds SSH keys for
	// the fleet; Guacamole stores connection passwords recoverably.
	//
	// For these, the archive's own encryption IS the security boundary, and the
	// ordinary envelope leaves the unwrapping key on a running server — often one
	// of the very hosts the archive grants access to. Write-only mode (F86) seals
	// the key offline instead, which is why its ABSENCE is worth reporting here
	// rather than left as an unmentioned default.
	CredentialStore string

	// ControlPlane, when non-empty, describes what else a move affects for an app
	// that manages other machines — what does NOT need reconfiguring, and what the
	// new host must be able to reach (F123).
	ControlPlane string

	// VersionLock, when non-empty, blocks a restore into ANY different version —
	// in both directions (F164).
	//
	// The strongest of the three version verdicts, and the rarest. OneWayMigration
	// blocks a downgrade because the schema only moves forward; NoConfigMigration
	// warns both ways because nothing migrates at all. This one blocks both
	// because the on-disk format's compatibility is genuinely UNKNOWN — a young
	// project that has recently reworked how it stores or encrypts its data, where
	// neither direction has been shown to be safe.
	//
	// "Unknown" is the operative word. For an ordinary application, guessing wrong
	// costs a broken restore that can be retried. For a store of credentials to
	// other systems, a database that opens but decrypts wrong is a much worse
	// outcome, and pinning the image is a cheap way to avoid ever finding out.
	//
	// It is recoverable by the operator: pin the target to the recorded version.
	// A digest-pinned restore — the normal path — reproduces that version anyway,
	// so this only fires when the image has been allowed to move.
	VersionLock string

	// NoConfigMigration, when non-empty, marks an app that does NOT migrate its
	// own configuration between versions (F129).
	//
	// The mirror image of OneWayMigration, and it needs the opposite treatment.
	// An app that migrates forward is safe in one direction and blocked in the
	// other. An app that migrates NOT AT ALL is a risk in BOTH: a newer image may
	// have renamed keys the old config still uses, and an older image may not
	// understand keys the config has since gained. Neither fails loudly —
	// Homepage simply stops rendering the affected widgets — so it warns on any
	// difference and blocks nothing.
	NoConfigMigration string

	// DerivedServices, when non-empty, explains which OTHER services in this
	// app's stack hold only data rebuilt from the one being backed up (F135).
	//
	// Distinct from Regenerable below, which is a directory inside a captured
	// mount. This is a whole sibling container — a search index, a cache tier —
	// whose data is a projection of somebody else's. Backing it up costs space
	// and, worse, couples the archive to that service's exact version.
	//
	// Advisory: DockBack does not decide which services an operator selects, it
	// just says which ones there is no point selecting.
	DerivedServices string

	// Regenerable lists directories INSIDE a captured mount that the application
	// rebuilds by itself, so an operator can trade them away for archive size
	// (F132).
	//
	// Mount-level selection cannot express these: Jellyfin's 11 GB of trickplay
	// thumbnails lives at data/trickplay inside the same /config mount as its
	// 54 MB database, so the only choices without this were "back up 12 GB" or
	// "back up nothing".
	//
	// Excluding one is INTENTIONAL and never grades a backup down. That
	// distinction matters: a backup missing 11 GB of thumbnails the server will
	// redraw is not the same thing as a backup missing 58 GB of photographs, and
	// treating them alike would blunt the warning that catches the second.
	Regenerable []RegenerablePath

	// NeverBackup lists paths ALWAYS left out — logs, temp and scratch (F138).
	//
	// Read against Regenerable above, which they are deliberately not. That field
	// holds data with real value that merely happens to be rebuildable, so
	// excluding it is the operator's call and the default is to keep it. These
	// have no value at all: Mealie's rotated logs were ~37 MB of a 93 MB archive,
	// and no log has ever restored anything.
	//
	// So there is no toggle. Offering one would imply a reason to keep rotated
	// logs inside an encrypted archive, and there isn't one — they are noise that
	// costs space and, since logs routinely carry request paths and identifiers,
	// widens what a leaked archive exposes.
	//
	// Entries may be globs; they are matched against paths inside the container.
	NeverBackup []NeverBackupPath

	// AtomicVolumes declares container paths that are only meaningful TOGETHER
	// (F141).
	//
	// Mount selection is per-mount, and for almost every app that is exactly
	// right: leaving out a media bind produces a smaller backup of the same
	// application. For a handful it is not a selection at all but a way to build
	// an archive that cannot be restored — Nginx Proxy Manager's proxy-host rows
	// live in /data and reference certificate files in /etc/letsencrypt, so half
	// of that pair restores into either `nginx: [emerg] cannot load certificate`
	// or, worse, a silent factory reset.
	//
	// Declaring the set does two things: capture keeps the members together even
	// when one was deselected, and restore REFUSES an archive that holds only
	// part of it.
	AtomicVolumes *AtomicVolumeSet

	// StackAtomic declares that this application's state is split across SEVERAL
	// CONTAINERS that are only meaningful together (F146).
	//
	// AtomicVolumes above is the same idea one level down: paths inside one
	// container. This is the whole stack. Pangolin is the case that forces it —
	// the tunnel concentrator's WireGuard private key lives in one service and the
	// database holding the site secrets that key authenticates lives in another,
	// so restoring either without the other leaves every site-to-site tunnel down
	// with nothing in the logs to say why.
	//
	// Declared on ONE anchor image (the application proper). Every container in
	// the same compose project is then a member of its set: the members are
	// discovered from the deployment rather than listed here, so a stack with a
	// service added, removed or renamed is handled without a code change — which
	// is the only way this can work on somebody else's machine.
	StackAtomic *StackAtomicSet

	// PostRestoreNotes are read-only questions asked of this application's
	// database AFTER a restore, each with something to tell the operator when the
	// answer comes back yes (F170).
	//
	// A restore can be perfect and still leave work to do, because an application
	// may depend on things outside itself that a database cannot bring back. Wiki.js
	// is the case: with its default settings a restore is complete, but if an
	// external search engine has been configured, its index lives in that engine
	// and must be rebuilt — and if a Git mirror has been set up, its remote and
	// deploy key want checking before it syncs.
	//
	// Neither of those is knowable from the image, the manifest, or anything else
	// DockBack can see. Only the application's own configuration knows, and after
	// a restore that configuration is right there.
	//
	// Strictly informational and strictly read-only: a SELECT, and a sentence. It
	// never fails a restore and never writes anything.
	PostRestoreNotes []PostRestoreNote

	// Upstream declares where this application records the address of a service
	// it CONNECTS OUT TO, and how to check that it can still reach it (F160).
	//
	// Every Address binding above answers "where is THIS application served?".
	// This answers the opposite question, and it moves in the opposite direction:
	// moving this application changes nothing, and moving the OTHER one is what
	// breaks it. Tautulli is the case — it holds the address of the media server
	// it reads from, along with a token that stays valid wherever that server
	// goes, so a move of Tautulli needs nothing at all and a move of Plex needs
	// exactly one value changed.
	//
	// That asymmetry is worth encoding because the failure is quiet: the
	// application starts, reports healthy, and simply stops collecting anything.
	Upstream *UpstreamAddress

	// StagingDebris declares the directories this application creates while
	// writing and renames away on success (F157).
	//
	// The pattern is how careful software writes: stage the new content beside
	// the target, then rename it into place, so a crash leaves either the old
	// content or the new one and never a half of each. It follows that a staging
	// directory still sitting there long afterwards is the fossil of a write that
	// was interrupted — and that nothing will ever clean it up, because the
	// application is not looking for it.
	//
	// It is inert, which is exactly why it survives: two of them sat in one
	// production tree for a year, quietly doubling the item count of every backup
	// of it. A backup walks the tree anyway, so it is the natural place to notice.
	//
	// Reported, never deleted. Deleting what looks like debris is not a thing a
	// backup tool should do on its own — see StagingDebrisPattern.
	StagingDebris []StagingDebrisPattern

	// StorageCompat states which versions of this application can read the
	// on-disk format this backup holds (F158).
	//
	// Distinct from OneWayMigration and NoConfigMigration, which are gates. This
	// is a plain statement for the cases where neither applies: a format that is
	// stable across a whole major version has nothing to block, and the useful
	// thing to say is where the edge actually is.
	StorageCompat string

	// CloneRedactions declares values that must be REMOVED from a clone before it
	// is allowed to start (F155).
	//
	// A clone is supposed to be a copy nobody notices — brought up beside the
	// original to prove a backup restores, then deleted. For most applications
	// it is: an isolated clone has fresh volumes, no published ports and a
	// throwaway network, so nothing outside the host can find it.
	//
	// That reasoning fails for an application that registers itself with a cloud
	// service. Plex is the case: its server identity and account token live in a
	// configuration file, and a restored copy will happily announce itself to
	// Plex's servers as the SAME server the operator is still running — from a
	// throwaway network, which is all the outbound access it needs. The rehearsal
	// that was supposed to prove the backup works instead makes two servers
	// contend for one identity.
	//
	// So the clone's own copy of the file is edited before its first start.
	// Never the original: this runs only on a clone, against data that is a
	// throwaway by construction.
	CloneRedactions []CloneRedaction

	// NeutralizeOnClone lists ENVIRONMENT VARIABLES that must be defanged before
	// a clone of this application is allowed to start (#17).
	//
	// CloneRedactions above solves the same problem one layer down — a value in a
	// FILE that makes a copy announce itself as the original. This is the wider
	// case R2 §Issue 17 found: a BookStack clone inherited working Gmail SMTP
	// credentials, so a test restore could send real invites and password resets
	// to real recipients from the real account. It generalises well past mail —
	// webhooks, push tokens, S3 write credentials, *arr callbacks.
	//
	// The failure is not that the clone is insecure. It is that the operator
	// believes they are looking at a sandbox: "a restored clone is not inert, it
	// is a second live system with the first system's authority".
	//
	// Clone mode only, matching F155's scope. A cutover restore must keep its
	// credentials, or the thing it restored does not work.
	NeutralizeOnClone []EnvNeutralization

	// SecretFiles lists files inside this application's data whose PERMISSIONS
	// matter, so an over-permissive one is reported rather than silently carried
	// from backup to backup (F147).
	//
	// DockBack does not change them. A restore that quietly tightened a mode
	// would be changing the operator's security posture without being asked,
	// which is the same rule that stops it quietly loosening one — and the rule
	// is worth more than the fix. So the finding is stated, with the command,
	// at the two moments it is actionable: when the backup is taken, and when
	// the files land on a new machine.
	SecretFiles []SecretFile

	// VolatileTables names tables this application writes to by itself, so a
	// content difference in one of them after a restore is expected rather than a
	// defect (#18/#26).
	//
	// The content baseline compares the restored database against what was
	// captured, at the one moment the data is the restore's alone. Even then an
	// application can have written: R4 §31's corrected comparison was 73 of 74
	// tables identical, "the single remaining difference being genuine runtime
	// state". Which table that is cannot be guessed — a session store is runtime
	// state in one schema and could be durable data in another — so it is
	// declared, per application, or not classified at all.
	//
	// Declaring them is what ARMS the failure policy: an application that says
	// which of its tables change by themselves gets a mismatch in any OTHER table
	// treated as a failed restore. One that says nothing gets the same report as
	// a warning, because failing there would roll back good restores.
	//
	// Bare table names, without a database or schema qualifier.
	VolatileTables []string

	// VolatileFiles are files this application rewrites by itself, so a content
	// difference in one after a restore is expected rather than a defect
	// (#41/#19).
	//
	// The file half of VolatileTables above, and R5 §9.3 is why it has to exist
	// separately. Immich writes a 13-byte `.immich` marker into each media folder
	// on startup; a clone rewrote all six with its own timestamps, and the whole
	// 58 GB library then hashed as different. Re-hashing with those six excluded
	// returned IDENTICAL for all 34,288 other files.
	//
	// Two properties made it nasty and both argue for classification rather than
	// tolerance: the marker is a FIXED WIDTH, so a path+size manifest cannot see
	// the change at all, and the clone caused it — the application had modified
	// its own tree before anything compared it.
	//
	// Entries are globs. A pattern without a "/" matches a file's NAME anywhere in
	// the tree; one with a "/" matches the whole archive-relative path.
	VolatileFiles []string

	// HTTPVerify describes how to prove this application is USABLE after a
	// restore, not merely answering (#33).
	//
	// The redirect check needs nothing from a profile — every HTTP application
	// can be asked for "/" and read without following. This is for the other
	// half: a round-trip POST that exercises origin validation, which needs the
	// application's own login path and the name of its anti-forgery field.
	//
	// Seeded only where a report gives the exact recipe. Guessing the field name
	// wrong would POST without a token, earn the 403 that means "no token", and
	// report it as "this origin is rejected" — a false danger finding about the
	// very thing being tested.
	HTTPVerify *HTTPVerifySpec

	// CriticalTables names tables whose EMPTINESS after a restore means the
	// application did not come back — it came up factory-fresh (F142).
	//
	// The generic contract (F109/F124) compares restored counts against what
	// capture recorded, which catches this whenever a contract exists. This adds
	// the thing a count comparison cannot say: what an empty table MEANS. "user
	// came back with 0 of 1 rows" and "the restored database has no accounts, so
	// starting it would hand a stranger the setup wizard" are the same fact and
	// very different sentences.
	CriticalTables []CriticalTable

	// CertificateRoots lists container directories holding TLS certificates that
	// travel inside this app's data (F143).
	//
	// Only certificates are read — the PUBLIC half. A private key's presence is
	// asserted by testing that the file exists; its bytes are never read out of
	// the container, never parsed, and never recorded.
	CertificateRoots []string

	// DefaultPause overrides the quiesce mode used when the operator has not
	// chosen one (F145), and DefaultPauseWhy is the reason, logged when it
	// applies.
	//
	// The shipped default — a brief `docker pause` around the volume copy — is
	// right for an app whose files could be written mid-copy. It is the wrong
	// trade for an ingress proxy: freezing it stalls every request to every
	// service behind it, and buys nothing, because the only thing in there that
	// could tear is a SQLite database that is captured by an online snapshot
	// which does not depend on quiescing.
	DefaultPause    string
	DefaultPauseWhy string

	// NeverRegenerate names ENVIRONMENT VARIABLES holding a value this
	// application can never re-create, so a capture that does not carry one is a
	// failure rather than a smaller backup (#11).
	//
	// R2 §Issue 11 is the shape: BookStack's APP_KEY is Laravel's encryption key.
	// Restore the database without it and the app "starts fine, logs in fine, and
	// has permanently unreadable encrypted fields" — there is no recovery, because
	// the ciphertext is all that is left and the key was only ever in the
	// environment. This is the one class where the BACKUP is the last copy, so a
	// capture that silently misses it is worthless in exactly the disaster it
	// exists for.
	//
	// Entries are asserted at capture and the backup FAILS if one is absent. That
	// is deliberate and it is why this list is short and evidence-led: a
	// false entry here does not degrade a backup, it stops one.
	//
	// Environment only. An application that keeps its irreplaceable secret in a
	// configuration FILE — Nextcloud's secret and passwordsalt live in config.php
	// — is not expressible here, and must not be listed: the values are simply
	// absent from the environment, so every one of that app's backups would fail.
	// See neverregen.go.
	NeverRegenerate []string

	// NeverRegenerateFiles is NeverRegenerate for applications that keep their
	// irreplaceable value in a CONFIGURATION FILE rather than the environment.
	//
	// Nextcloud is why this exists. Its secret and passwordsalt live in
	// config.php — secret encrypts stored credentials, passwordsalt salts the
	// password hashes — and R3 confirms both must survive verbatim for a restored
	// instance to log in. Neither is ever in the environment, so listing them in
	// NeverRegenerate above would find them absent on every container and refuse
	// every Nextcloud backup.
	//
	// The check reads only whether each key is PRESENT with a non-empty value.
	// The values are never read out, never logged, and never recorded.
	NeverRegenerateFiles []NeverRegenerateFile

	// EmbeddedDump declares a real database SERVER running inside this app's own
	// container, which can be dumped with native tools (F126).
	//
	// Read that against EmbeddedDBWarning above: that field means "there is a
	// database in here and DockBack has NO dump tool for it" (H2). This one means
	// the opposite — there is one, and it must be used.
	//
	// The distinction matters because detectDBEngine keys on the IMAGE NAME, and
	// an all-in-one image says nothing about what it bundles. `jwetzell/guacamole`
	// runs a full PostgreSQL; nothing in its name hints at it, so DockBack saw an
	// ordinary application and raw-copied the live data directory — a hot file
	// copy of a running database, the precise failure the dump path exists to
	// prevent.
	EmbeddedDump *EmbeddedDump
}

// AtomicVolumeSet is a group of container paths that must travel together
// (F141).
type AtomicVolumeSet struct {
	// Paths are the container destinations in the set.
	Paths []string
	// Why explains what binds them, in the operator's terms.
	Why string
	// Symptom is what restoring only part of the set actually does — the thing
	// worth refusing over, stated as what they would see.
	Symptom string
}

// Has reports whether dest is a member of the set (or lives under one).
func (a *AtomicVolumeSet) Has(dest string) bool {
	if a == nil || dest == "" {
		return false
	}
	for _, p := range a.Paths {
		if dest == p || strings.HasPrefix(dest, strings.TrimSuffix(p, "/")+"/") {
			return true
		}
	}
	return false
}

// StackAtomicSet declares an application whose containers form one unit (F146).
type StackAtomicSet struct {
	// Why explains what binds the services together, in the operator's terms.
	Why string
	// Symptom is what restoring part of the set actually does.
	Symptom string
	// SoloRestore is the extra sentence shown when someone restores ONE member —
	// the case that looks most reasonable and is most damaging.
	SoloRestore string
}

// PostRestoreNote is one read-only question and what to say when the answer is
// yes (F170).
type PostRestoreNote struct {
	// SQL must be a SELECT that returns at least one row when the note applies
	// and no rows when it does not. It runs against the application's own
	// database, through the database container's own client and credentials.
	//
	// Written by DockBack, never by an operator: this is registry data, not a
	// setting, so there is no path by which a query here becomes user input.
	SQL string
	// Engine is the dialect the query is written in ("postgres", "mysql"), so a
	// note is never run against a database that would not understand it.
	Engine string
	// Note is what the operator is told. It should name the thing that is active
	// and the one action it needs, because the reason this exists is that the
	// restore looked finished and was not.
	Note string
}

// UpstreamAddress is a dependency's address, recorded inside this application's
// own configuration (F160).
type UpstreamAddress struct {
	// Service names the dependency in the operator's terms ("Plex").
	Service string
	// File is the configuration file inside the container holding the address.
	File string
	// Section is the INI section the keys live under, empty for a flat file.
	// Section-scoped so a key name that recurs elsewhere in the file cannot be
	// caught by accident.
	Section string
	// URLKeys take the full address ("http://host:port"); HostKeys take only the
	// host part. Both are written when the operator supplies a new address.
	URLKeys  []string
	HostKeys []string
	// Why explains what the value is for.
	Why string
	// Symptom is what a stale value actually does — which for this class is
	// always something quiet, or it would not need explaining.
	Symptom string
	// Probe, when set, is a command run inside the container AFTER the restore is
	// healthy, to confirm the dependency is genuinely reachable with the
	// credentials that were restored (F161).
	//
	// It must print a verdict and NOTHING ELSE. The credential stays inside the
	// container: the command reads it from the application's own configuration,
	// uses it, and prints a status — it never passes through DockBack, and it
	// never reaches a log.
	Probe []string
	// ProbeOK is the marker the probe prints when the dependency answered and
	// identified itself as the same one the configuration expects.
	ProbeOK string
}

// StagingDebrisPattern is one shape of interrupted-write leftover (F157).
type StagingDebrisPattern struct {
	// Glob matches the directory NAME (not its path), in shell-glob form.
	Glob string
	// MinAge is how old a match must be before it counts. Below it, this is a
	// write in progress and reporting it would be reporting normal operation —
	// which is how a warning stops being read.
	MinAge time.Duration
	// What and WhatPlural name it in the operator's terms, so the finding says
	// what happened rather than which pattern matched.
	//
	// Two forms rather than one with "(s)" tacked on: a finding is read once,
	// under pressure, and "1 leftover staging director(ies)" is the kind of
	// sentence that makes a reader stop trusting the rest of it. English plurals
	// are not derivable in general, so the profile supplies both.
	What       string
	WhatPlural string
	// Cleanup is the one thing DockBack will not do for them, spelled out: what
	// to remove, and what has to be true first.
	Cleanup string
}

// noun picks the form that fits the count, falling back to whichever is set.
func (p StagingDebrisPattern) noun(n int) string {
	if n == 1 && p.What != "" {
		return p.What
	}
	if p.WhatPlural != "" {
		return p.WhatPlural
	}
	if p.What != "" {
		return p.What
	}
	if n == 1 {
		return "leftover from an interrupted write"
	}
	return "leftovers from interrupted writes"
}

// CloneRedaction is one file whose named values a clone must not start with
// (F155).
// Neutralisation modes. Two, because the two jobs are different: a credential
// has no safe value and is emptied, while a MODE selector has one and must be
// set to it — emptying MAIL_DRIVER leaves the application with no mailer at all,
// which for some is a crash rather than a quiet clone.
const (
	NeutralizeClear = "clear"
	NeutralizeSet   = "set"
)

// EnvNeutralization is one environment variable made harmless in a clone.
type EnvNeutralization struct {
	// Key is the variable name.
	Key string
	// Mode is NeutralizeClear or NeutralizeSet.
	Mode string
	// Value is what to set, for NeutralizeSet. Ignored otherwise.
	Value string
	// Why explains what the clone would otherwise do, in the operator's terms.
	Why string
}

// HTTPVerifySpec is one application's login round-trip.
type HTTPVerifySpec struct {
	// Port the application listens on INSIDE its container. Zero means "discover
	// it from the container", which is right for most images and necessary for
	// any that are configurable.
	Port int
	// LoginPath is the form's own path.
	LoginPath string
	// TokenPattern captures the anti-forgery token from the rendered form in its
	// first group, and TokenField is the form field it goes back in.
	TokenPattern string
	TokenField   string
}

type CloneRedaction struct {
	// Path is the file inside the container.
	Path string
	// Attrs are XML/INI-style attribute names whose value is emptied in place —
	// `Name="value"` becomes `Name=""`. Emptying rather than deleting keeps the
	// file's shape, which matters for an application that expects the key to be
	// present.
	Attrs []string
	// Why explains, in the operator's terms, what the clone would otherwise do.
	Why string
	// Required marks a redaction that must SUCCEED or the clone is not started.
	// True where the consequence of starting anyway reaches beyond the clone —
	// which is the only reason this feature exists.
	Required bool
}

// SecretFile is one file whose permissions are worth auditing (F147).
type SecretFile struct {
	// Path inside the container.
	Path string
	// MaxMode is the widest permission bits acceptable, as an octal literal
	// (0o600 for a private key). A file wider than this is reported.
	MaxMode uint32
	// What names the file in the operator's terms, so the finding says what is
	// exposed rather than which path is wrong.
	What string
}

// CriticalTable is one table whose emptiness is a restore failure (F142).
type CriticalTable struct {
	// DB is the database file's absolute path inside the container.
	DB string
	// Table is the table name as the application's schema spells it.
	Table string
	// Means is what zero rows there means for the operator — not "the table is
	// empty" but what the application will do about it.
	Means string
}

// NeverBackupPath is one path always left out of a capture (F138).
type NeverBackupPath struct {
	// Path inside the container. May be a glob.
	Path string
	// Why names what it is, so the archive's record of the exclusion reads as a
	// decision rather than a gap.
	Why string
}

// RegenerablePath is one directory an application rebuilds on its own (F132).
// The json tags are load-bearing (F183). This type is one of the few in this
// file that is serialized STRAIGHT to the browser, and without them Go named the
// fields Path/Label/Cost while the page read path/label/cost — so every field
// arrived undefined and `label.toLowerCase()` took the whole container page down
// for any image declaring a regenerable directory.
type RegenerablePath struct {
	// Path is the directory inside the container.
	Path string `json:"path"`
	// Label names it as a person would — shown on the toggle.
	Label string `json:"label"`
	// Cost states what excluding it costs, so the trade is legible before it is
	// made rather than discovered afterwards.
	Cost string `json:"cost"`
}

// EmbeddedDump describes how to dump a database server bundled inside an
// application's own container (F126).
type EmbeddedDump struct {
	// Engine is the dump dialect: "postgres" or "mysql".
	Engine string
	// DataDir is the server's data directory INSIDE the container. Excluded from
	// the file capture once the dump succeeds, exactly as a standalone database
	// container's data directory already is — a logical dump supersedes it, and
	// shipping both means shipping a torn copy alongside a good one.
	DataDir string
	// User and DBName identify what to dump. Empty falls back to the container's
	// own environment, which most images set.
	User   string
	DBName string

	// Socket is the server's unix socket, when the application puts it somewhere
	// the client will not find on its own (F166). Empty uses the client's
	// default, which is right for an image whose configuration already points at
	// its own socket.
	Socket string

	// When, if set, is a command run inside the container whose SUCCESS means
	// this application really is running the embedded engine (F166).
	//
	// Needed because some applications ship ONE image that can run several ways.
	// Uptime Kuma 2 is the case: the same image runs an embedded MariaDB or a
	// SQLite file depending on a setting, and the MariaDB client is present
	// either way. Without a probe, a SQLite deployment would be handed a dump
	// command whose client exists but whose server does not — which is not the
	// "tools missing" signal that falls back gracefully, but a hard connection
	// failure that FAILS THE BACKUP of a perfectly healthy application.
	//
	// So the declaration becomes conditional: probe first, and when the answer is
	// no, take the ordinary file path — which for that deployment finds the
	// SQLite database and snapshots it properly.
	//
	// A probe that cannot run is read as "no". An application that might be
	// running an embedded server is better backed up as files than not at all.
	When []string

	// CountTables are the application's own tables whose row counts are recorded
	// at capture and checked after the import (F168).
	//
	// The generic contract for this engine counts TABLES, which catches a
	// truncated dump. It cannot catch an import that created every table and
	// filled few of them. For an application whose value IS the rows — a history
	// of checks, a set of monitors — naming the handful that matter turns "the
	// schema is there" into "the data is there".
	CountTables []string
}

// Address bindings (F114).
//
// Several applications store the address they are served at inside their own
// state, and a faithful restore faithfully reinstates the OLD one. The backup is
// intact, the health gate is green, the container is running — and the app is
// unreachable, because it is answering at an address it does not believe in.
//
// Nextcloud is the sharpest case: `trusted_domains` lives in config.php inside a
// captured volume, so the restore writes the old machine's address straight back
// and the site answers "You are accessing the server from an untrusted domain."
// Homepage does the same with HOMEPAGE_ALLOWED_HOSTS and returns HTTP 400.
//
// These are NOT one problem. They differ along the axis that decides whether
// DockBack may act on the operator's behalf:
//
//	REVERSIBILITY.
//
//	  BindEnv            — an environment variable. Undone by recreating again,
//	                       so it is applied automatically when the operator
//	                       supplies a new address.
//	  BindAppCommand     — the app's own supported CLI (Nextcloud's occ). Setting
//	                       a value is undone by setting it again, so this is also
//	                       applied automatically — but ONLY through the app's own
//	                       tool, never by editing its config file. DockBack does
//	                       not rewrite an application's state by hand; a regex
//	                       through config.php is how a working restore becomes a
//	                       broken one.
//	  BindContentRewrite — rewrites content rows in place and CANNOT be undone
//	                       (BookStack's bookstack:update-url). Never automated:
//	                       printed, filled in, for the operator to run once.
//	  BindManual         — no command exists; it is a step in the app's own UI.
//
// None of this runs unless the operator explicitly supplies a new address. The
// common move — same domain, DNS or tunnel re-pointed at the new host — needs
// none of it, and running a rewrite "to be safe" on a move that did not change
// the address is its own way to break things.
type BindingKind string

const (
	BindEnv            BindingKind = "env"
	BindAppCommand     BindingKind = "app-command"
	BindContentRewrite BindingKind = "content-rewrite"
	BindManual         BindingKind = "manual"
	// BindExternal is a change that must be made in a DIFFERENT SYSTEM
	// entirely — an identity provider's redirect URI being the archetype (F136).
	//
	// Every other kind describes something inside the application, which DockBack
	// can at least read and often set. This one it cannot reach at all, and the
	// failure it causes is the most misleading of the set: the app comes up
	// perfectly healthy and only LOGIN breaks, so it reads as a broken restore
	// rather than a setting left behind somewhere else.
	BindExternal BindingKind = "external"
)

// EnvShape is which part of a supplied address an environment variable holds.
type EnvShape string

const (
	// EnvURL writes a full URL. A scheme is ADDED when the operator supplied an
	// address without one, because every variable in this shape is parsed as a
	// URL by the application that reads it — and a bare host in one of them is
	// not a weaker value, it is a fatal one (F186).
	//
	// This is the zero value, so it is also what an unset Shape means for a
	// single-value binding. A LIST with no Shape means bare hosts instead; see
	// shapeAddress.
	EnvURL EnvShape = ""
	// EnvHost writes the hostname alone, with no scheme and no path.
	EnvHost EnvShape = "host"
	// EnvScheme writes just "http" or "https".
	EnvScheme EnvShape = "scheme"
	// EnvOrigin writes scheme://host — no path, no trailing slash. Needed
	// explicitly because a LIST defaults to bare hosts, and a CSRF trusted-origin
	// list is the opposite: Django rejects an entry with no scheme.
	EnvOrigin EnvShape = "origin"
)

// AddressBinding is one place an application records its own address.
type AddressBinding struct {
	Kind BindingKind

	// Keys are the environment variables this binding covers (BindEnv only).
	Keys []string
	// List marks an env value that is a separated SET of accepted hosts rather
	// than a single address, so the new one is ADDED to the set instead of
	// replacing it wholesale — dropping the existing entries would lock the app
	// out at every address it currently answers on.
	List bool
	// ListSep is what separates those entries. Empty means a comma, which is the
	// common case; Nextcloud's NEXTCLOUD_TRUSTED_DOMAINS is space-separated,
	// because its entrypoint word-splits the value.
	ListSep string
	// Shape is how the supplied address is written into the variable. Empty means
	// "exactly as given" for a single value (these are usually full URLs) and
	// "just the host" for a list, which is what every list of accepted hosts
	// wants. Set it where an application splits the same address across several
	// variables — Nextcloud records the host, the URL and the scheme separately.
	Shape EnvShape

	// Apply names the built-in procedure that performs a BindAppCommand binding.
	// The procedure lives in code (it must read the app's current state before it
	// can change it, which no template can express); the profile only says which
	// one applies.
	Apply string

	// Commands are copy-ready command lines for the kinds DockBack does NOT run,
	// with {{old}} and {{new}} substituted.
	Commands []string

	// Blocking marks a binding whose staleness makes the application
	// UNREACHABLE, as opposed to merely leaving links pointing at the old
	// address. That is the difference between "a restore that looks successful
	// and isn't" and "a cosmetic follow-up", and the pre-restore panel says which.
	Blocking bool
	// Symptom is what the operator will actually SEE when this is stale — the
	// error page or status code in their words, so they can match it against what
	// their browser is showing them.
	Symptom string
	// Note is the caveat printed with the binding.
	Note string
}

// siteAddressUnsafe matches characters that must never reach a command
// invocation or a trust list. Every command built from these values is executed
// as argv with no shell involved at any point, so this is defence in depth
// rather than the boundary — but a value containing them is malformed as an
// address regardless.
var siteAddressUnsafe = regexp.MustCompile(`[\s;&|$<>()\\'"` + "`" + `]`)

// siteAddressShape is a permissive but bounded shape for "an address a web app
// is reached at": an optional scheme, a hostname / IPv4 / bracketed IPv6, an
// optional port, an optional trailing slash.
var siteAddressShape = regexp.MustCompile(`^(?:[a-zA-Z][a-zA-Z0-9+.\-]*://)?(?:\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9](?:[A-Za-z0-9.\-]*[A-Za-z0-9])?)(?::[0-9]{1,5})?/?$`)

// ValidSiteAddress checks an operator-supplied address before it is written into
// a trust list or an environment variable (F114).
//
// The wildcard rejection is the one that matters. Every binding here is either a
// trust list or an address the app answers on, and "*" would make the restore
// succeed by disabling the protection the value exists to provide. A feature
// that quietly widens a security control to make itself work is worse than one
// that fails.
func ValidSiteAddress(s string) error {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return errors.New("enter the address the application will be reached at")
	case len(s) > 255:
		return errors.New("that address is too long")
	case strings.Contains(s, "*"):
		return errors.New("a wildcard is not accepted here — this value is a trust list, and widening it to \"*\" would let the application answer for any host. Enter the specific address")
	case siteAddressUnsafe.MatchString(s):
		return errors.New("that address contains characters that are not valid in a hostname")
	case !siteAddressShape.MatchString(s):
		return errors.New("that does not look like a hostname, IP address or URL (for example cloud.example.com, 10.168.1.50:8080, or https://cloud.example.com)")
	}
	return nil
}

// AddressHost reduces an address to the bare host[:port] a trust list wants,
// dropping any scheme and trailing path. `trusted_domains` and
// HOMEPAGE_ALLOWED_HOSTS hold HOSTS, not URLs, and an entry with a scheme in it
// silently never matches — which presents to the operator as "the fix did not
// work", with nothing to indicate why.
func AddressHost(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return s
}

// appProfiles is the registry, most specific match first. Kept as a slice
// rather than a map so ordering is explicit: a future "audiobookshelf-lite"
// style fork must be able to sit ahead of its parent.
var appProfiles = []struct {
	match   string
	profile AppProfile
}{
	{
		match: "audiobookshelf",
		profile: AppProfile{
			Name: "Audiobookshelf",
			// Library items store the container-side media path (/audiobooks/…),
			// never the host path — so the same media mounted at a different
			// destination reads as a library of missing files.
			PathEmbedding: "Audiobookshelf stores each library item's path as it appears INSIDE the container, so the target must mount the media at the same container destinations",
			// /config holds absdatabase.sqlite, which also carries the token
			// secret that keeps existing sessions valid.
			LocalOnly:       []string{"/config"},
			OneWayMigration: "Audiobookshelf migrates its database forward on start and ships no down-migrations",
		},
	},
	{
		match: "bookstack",
		profile: AppProfile{
			Name: "BookStack",
			// The entrypoint runs `php artisan migrate --force` on EVERY start,
			// so a restored stack self-migrates the moment it comes up — forward
			// only. Combined with a floating :latest tag this ratchets: once an
			// image has advanced the schema, older images can no longer run it.
			OneWayMigration: "BookStack runs its database migrations automatically on every start and ships no down-migrations",
			// solidnerd/bookstack and the linuxserver build both ship without a
			// HEALTHCHECK, so "running" is all Docker can report — which is not a
			// safe verdict for an app that migrates at startup.
			NoHealthcheck: true,
			// #17 / R2 §Issue 17. This deployment's environment carried working
			// Gmail credentials, so a clone could send real invites and password
			// resets from the real account. The report's own list is
			// [MAIL_PASSWORD, MAIL_DRIVER], with MAIL_DRIVER=log as the safe value.
			//
			// MAIL_DRIVER is what actually stops the sending: Laravel's "log"
			// mailer writes the message to the log instead of delivering it, so
			// the clone still exercises every code path that composes mail.
			NeutralizeOnClone: []EnvNeutralization{
				{Key: "MAIL_DRIVER", Mode: NeutralizeSet, Value: "log",
					Why: "so mail this copy composes is written to its log instead of delivered"},
				{Key: "MAIL_PASSWORD", Mode: NeutralizeClear,
					Why: "so the copy cannot authenticate to the real mail account"},
			},
			// #11 / R2 §Issue 11. Laravel encrypts database columns with APP_KEY.
			// A backup of the database without it restores into an application
			// that works in every visible way and can never read those columns
			// again.
			NeverRegenerate: []string{"APP_KEY"},
			Address: []AddressBinding{
				{
					// Reversible: recreate again and it is back. Applied for you.
					Kind: BindEnv,
					Keys: []string{"APP_URL"},
					Note: "APP_URL is set to the new address on the recreated container.",
				},
				{
					// Irreversible: rewrites URLs across the content tables.
					Kind: BindContentRewrite,
					Commands: []string{
						"php artisan bookstack:update-url {{old}} {{new}}",
						"php artisan cache:clear",
					},
					Symptom: "links inside your pages keep pointing at the old address",
					Note: "This rewrites stored URLs across your content tables IN PLACE and cannot be undone, so DockBack will not run it for you. " +
						"Take a backup first, then run it once. The command asks for confirmation before it changes anything.",
				},
			},
		},
	},
	{
		// jc21/nginx-proxy-manager and the community rebuilds all carry this in
		// the image name.
		match:   "nginx-proxy-manager",
		profile: nginxProxyManagerProfile,
	},
	{
		match:   "radicale",
		profile: radicaleProfile,
	},
	{
		match:   "uptime-kuma",
		profile: uptimeKumaProfile,
	},
	{
		match:   "tautulli",
		profile: tautulliProfile,
	},
	{
		// Matched on the two real Plex Media Server images rather than on "plex",
		// which would also claim Plexamp, an unrelated "complex-…" image, and
		// anything else with those four letters in its name. A false match here
		// is not cosmetic: it would attach a downgrade block and a clone
		// redaction to something that is not Plex.
		match:   "pms-docker",
		profile: plexProfile,
	},
	{
		match:   "linuxserver/plex",
		profile: plexProfile,
	},
	{
		// The anchor of the Pangolin stack. Matched on the application image
		// only: gerbil, traefik and crowdsec are members of its set by virtue of
		// sharing its compose project, not by being listed here.
		match:   "fosrl/pangolin",
		profile: pangolinProfile,
	},
	{
		match: "nextcloud",
		profile: AppProfile{
			Name: "Nextcloud",
			// #19/#26 / R3 §7.2. Of 234 tables, 229 were identical and these five
			// differed — "all runtime state". oc_filecache, Nextcloud's index of
			// every file and the strongest single signal there is, was identical
			// with all 67,234 rows. Naming these is what turns "229/234 tables
			// identical", which invites a support ticket, into a claim the tool
			// can stand behind.
			VolatileTables: []string{
				"oc_appconfig", "oc_authtoken", "oc_job_runs", "oc_jobs", "oc_preferences",
			},
			// #11 / R3: "Preserved verbatim: secret, passwordsalt, instanceid".
			// secret encrypts stored credentials and passwordsalt salts the
			// password hashes, so an instance restored without them logs nobody in
			// and cannot read what it had encrypted. Both live in config.php and
			// neither is ever in the environment.
			NeverRegenerateFiles: []NeverRegenerateFile{{
				Path: "/var/www/html/config",
				Keys: []string{"secret", "passwordsalt"},
			}},
			Address: []AddressBinding{
				{
					// F175: the official image reads these from the ENVIRONMENT on
					// every request, through its own config/reverse-proxy.config.php,
					// which is loaded AFTER config.php. So the environment wins over
					// anything occ writes — a restore that fixed the address with occ
					// alone was silently overridden by the variable it had just
					// carried over from the previous machine, and the site kept
					// redirecting there.
					//
					// Three variables, three different parts of the same address.
					Kind:     BindEnv,
					Keys:     []string{"OVERWRITEHOST"},
					Shape:    EnvHost,
					Blocking: true,
					Symptom:  "every request is redirected to the machine the backup came from, whatever address the visitor used",
				},
				{
					Kind:  BindEnv,
					Keys:  []string{"OVERWRITECLIURL"},
					Shape: EnvURL,
				},
				{
					Kind:  BindEnv,
					Keys:  []string{"OVERWRITEPROTOCOL"},
					Shape: EnvScheme,
				},
				{
					// Read only when the image INSTALLS Nextcloud, so on a restored
					// instance the live trust list is the one occ manages below.
					// Kept accurate anyway: it is what a later rebuild from this
					// compose file would install with, and a stale value there is a
					// trap set for the next move.
					//
					// Space-separated: the entrypoint word-splits it.
					Kind:    BindEnv,
					Keys:    []string{"NEXTCLOUD_TRUSTED_DOMAINS"},
					List:    true,
					ListSep: " ",
					Shape:   EnvHost,
				},
				{
					// The sharpest case in the set: trusted_domains lives in
					// config.php INSIDE a captured volume, so the restore puts
					// the old machine's address back and the site refuses to
					// answer. Reversible through occ, so DockBack applies it —
					// but only through occ, never by editing config.php.
					Kind:     BindAppCommand,
					Apply:    applyNextcloudTrustedDomain,
					Blocking: true,
					Symptom:  "\"You are accessing the server from an untrusted domain\" — the site refuses to load at all",
					Note: "Set with Nextcloud's own occ tool, which is reversible: the stale entry is replaced in place where it exists, " +
						"and overwritehost / overwrite.cli.url are set to match, with config.php copied aside first. trusted_proxies is decided separately — it is your reverse proxy's IP, " +
						"a different value answering a different question. It is CLEARED only when the container moved to a different machine and its address changed with it, because the recorded upstream is then provably not the one in front of it; " +
						"on a same-host restore it is kept, because the proxy almost certainly still is. Either way the old value is printed as the command that puts it back.",
				},
			},
		},
	},
	{
		// Paperless-ngx. The variable names and the SHAPE each one wants were read
		// out of the image's own src/paperless/settings/__init__.py rather than
		// from documentation: PAPERLESS_URL is parsed as a URL and appended to
		// CSRF_TRUSTED_ORIGINS and CORS_ALLOWED_ORIGINS, with its HOSTNAME added
		// to ALLOWED_HOSTS. So the three lists want three different forms of the
		// same address, and writing one string into all of them would silently
		// produce entries that never match.
		match: "paperless",
		profile: AppProfile{
			// #33 / R4's own method, verbatim: "fetch the login page, extract the
			// csrfmiddlewaretoken, and POST deliberately-wrong credentials with an
			// explicit Origin header" — 200 means the origin was accepted, 403 that
			// it was rejected. Seeded because that report gives both the path and
			// the field name. Nothing else is guessed: a wrong field name would
			// POST without a token, earn the 403 that means "no token", and report
			// it as an origin rejection.
			HTTPVerify: &HTTPVerifySpec{
				LoginPath:    "/accounts/login/",
				TokenPattern: `name=["']csrfmiddlewaretoken["'][^>]*value=["']([^"']+)["']`,
				TokenField:   "csrfmiddlewaretoken",
			},
			Name: "Paperless-ngx",
			Address: []AddressBinding{
				{
					// The one setting that covers all three when it is present.
					Kind:     BindEnv,
					Keys:     []string{"PAPERLESS_URL"},
					Shape:    EnvURL,
					Blocking: true,
					Symptom:  "the login page returns \"CSRF verification failed\" or \"Bad Request (400)\" and no session can be established",
				},
				{
					// Django requires a SCHEME on every trusted origin; a bare host
					// is rejected outright at startup.
					Kind:  BindEnv,
					Keys:  []string{"PAPERLESS_CSRF_TRUSTED_ORIGINS", "PAPERLESS_CORS_ALLOWED_HOSTS"},
					List:  true,
					Shape: EnvOrigin,
				},
				{
					// ALLOWED_HOSTS is hostnames — a scheme here matches nothing.
					Kind:  BindEnv,
					Keys:  []string{"PAPERLESS_ALLOWED_HOSTS"},
					List:  true,
					Shape: EnvHost,
				},
			},
			PostRestoreNotes: []PostRestoreNote{
				{
					// F188: mail accounts using OAuth rather than a password.
					//
					// A password account needs nothing after a restore — the
					// credential is a column in the database that just came back. A
					// TOKEN account is different in kind: the refresh token is issued
					// to an app registration at Google or Microsoft, it expires, and
					// re-authorising is a round trip through the provider's consent
					// screen that no restore can perform. The documents are all
					// there; the thing that fetches new ones has quietly stopped.
					//
					// Read from the application's own table (paperless_mail_mailaccount,
					// Django's default name for that model), and only a count comes
					// back — the row also holds credentials.
					Engine: "postgres",
					SQL:    `SELECT 1 FROM paperless_mail_mailaccount WHERE is_token = true`,
					Note: "Paperless-ngx fetches documents from a mail account that authenticates with OAuth, not a password. " +
						"The refresh token was restored with the database, but those are issued to an app registration and expire — " +
						"if fetching has stopped, re-authorise the account in Settings -> Mail. Password-based accounts need nothing; " +
						"their credentials came back with the database.",
				},
			},
		},
	},
	{
		match: "gethomepage/homepage",
		profile: AppProfile{
			Name: "Homepage",
			// The app is trivial; its CONFIG is not. services.yaml embeds working
			// API keys for ~20 OTHER services — Sonarr, Immich, Nextcloud,
			// Proxmox, Portainer, Tailscale, Dockhand — so one decrypted archive
			// is a combined credential dump of most of a homelab. That is access
			// to other systems, which is what puts it in this class; contrast
			// Gotify, whose tokens reach only Gotify.
			CredentialStore: "Homepage's configuration embeds working API keys for every service it shows a widget for, so a decrypted archive is a combined credential dump for most of your other applications at once",
			// Homepage reads its YAML as written and migrates nothing, so a
			// version change in either direction can silently stop widgets
			// rendering.
			NoConfigMigration: "Homepage does not migrate its configuration between versions",
			Address: []AddressBinding{
				{
					// Homepage 0.9+ rejects any request whose Host header is not
					// in this list, with a bare HTTP 400 and no explanation — so
					// a cross-machine restore looks perfect and the dashboard is
					// simply gone.
					Kind:     BindEnv,
					Keys:     []string{"HOMEPAGE_ALLOWED_HOSTS"},
					List:     true,
					Blocking: true,
					Symptom:  "every request returns HTTP 400 \"Bad Request\" — the dashboard does not load at all",
					Note: "The new address is ADDED to HOMEPAGE_ALLOWED_HOSTS rather than replacing it, so the dashboard keeps answering at the addresses it already accepts. " +
						"Remove any entry for a decommissioned host by hand.",
				},
			},
		},
	},
	{
		// immich-server / immich-machine-learning. NOT the Immich Postgres image,
		// which contains "postgres" and is already detected as a database engine
		// — matching it here would attach an application profile to a database.
		match: "immich-app/immich",
		profile: AppProfile{
			Name: "Immich",
			// #41 / R5 §9.3. Immich writes a `.immich` mount-check marker into each
			// media folder on startup — 13 bytes of millisecond timestamp, which
			// is what populates system_metadata's mountChecks. A clone rewrote all
			// six of them on first boot, and that alone made 58 GB compare as
			// different.
			VolatileFiles: []string{".immich"},
			// The server runs its own schema migrations at start, forward only.
			OneWayMigration: "Immich migrates its database schema on start and provides no way to downgrade it",
			// The photo library is far too large to keep on a network share for the
			// database, but the DATABASE is what must stay local; the media tree
			// itself lives wherever the operator points UPLOAD_LOCATION.
			PathEmbedding: "Immich records each asset's path relative to its upload location, so the media tree must be mounted at the same container path",
			Address: []AddressBinding{
				{
					// Immich stores no self-URL that breaks internally; what holds
					// an address is every phone with the app installed.
					Kind:    BindManual,
					Symptom: "the mobile and desktop apps stop syncing, which looks like a broken login but is not — the accounts are fine, the apps are simply still calling the old address",
					Note: "Immich keeps no server address of its own, so a move needs no change inside it and every login, API key and shared link stays valid. " +
						"What holds the address is each installed app: if the address people reach Immich at actually changes, update the server URL in those apps. " +
						"Keeping the same address needs nothing at all.",
				},
			},
		},
	},
	{
		// Karakeep, formerly Hoarder — both image names are in circulation.
		match:   "karakeep",
		profile: karakeepProfile,
	},
	{
		match:   "hoarder",
		profile: karakeepProfile,
	},
	{
		match: "mealie",
		profile: AppProfile{
			Name:            "Mealie",
			OneWayMigration: "Mealie migrates its database when it starts and provides no way to downgrade it",
			// Rotated logs were ~37 MB of a 93 MB archive, dwarfing the ~10 MB of
			// recipe images that are the actual point of the data directory.
			NeverBackup: []NeverBackupPath{
				{Path: "/app/data/mealie.log*", Why: "rotated application logs"},
				{Path: "/app/data/.temp", Why: "scratch directory"},
			},
			Address: []AddressBinding{
				{
					// BASE_URL is read from the environment at runtime, so it is
					// set on the recreated container and no data is rewritten.
					Kind: BindEnv,
					Keys: []string{"BASE_URL"},
					Note: "BASE_URL is set to the new address on the recreated container. Mealie reads it at runtime, so nothing in the database needs rewriting — it only affects the links Mealie puts in emails and shared pages.",
				},
			},
		},
	},
	{
		match: "jellyfin",
		profile: AppProfile{
			Name: "Jellyfin",
			// A migrated database will not open on an older server, and Jellyfin's
			// own guidance is to back up before every upgrade for exactly this
			// reason.
			OneWayMigration: "Jellyfin migrates its database when it starts and an older version cannot open a migrated one",
			// The report's biggest cross-machine risk: library sections AND
			// playlist files store absolute media paths, so the same shares
			// mounted somewhere else leave every library "unavailable".
			PathEmbedding: "Jellyfin stores absolute media paths in its libraries and playlists, so the media must be mounted at the same container paths",
			Regenerable: []RegenerablePath{{
				Path:  "/config/data/trickplay",
				Label: "Trickplay scrubbing previews",
				Cost:  "Jellyfin redraws them in the background after a restore, which re-decodes every video and can take hours on a large library",
			}},
		},
	},
	{
		match: "gotify",
		profile: AppProfile{
			Name: "Gotify",
			// The older binary cannot run a newer schema, so a downgrade fails at
			// start — after the data has been written.
			OneWayMigration: "Gotify migrates its database schema on start and an older version cannot open a newer one",
			Address: []AddressBinding{
				{
					// Not a setting inside Gotify at all — the address lives in
					// every RECEIVING app. Called out because the symptom
					// (notifications stop arriving) looks exactly like a broken
					// token, and people respond by re-issuing credentials that
					// were never the problem.
					Kind:    BindManual,
					Symptom: "clients stop receiving notifications, which looks like a broken token but is not — the tokens are fine, the apps are simply still calling the old address",
					Note: "Gotify's tokens are opaque database values and survive a move untouched, so nothing needs re-issuing and no client needs re-pairing. " +
						"What each receiving app stores is the SERVER URL: if the address people reach Gotify at actually changes, update it in those apps. " +
						"Keeping the same address (re-pointing DNS or your reverse proxy) needs nothing at all.",
				},
			},
		},
	},
	{
		match: "dockhand",
		profile: AppProfile{
			Name: "Dockhand",
			// The database's sensitive columns are encrypted with a key that lives
			// in a DIFFERENT volume, so a usable backup necessarily contains both —
			// and then the archive's own encryption is the only thing standing
			// between a leak and root on every managed host.
			CredentialStore: "Dockhand's backup contains both its database and the .encryption_key that unlocks it, so a decrypted archive grants Docker-daemon control of every host Dockhand manages — the ability to run any container as root across the fleet",
			ControlPlane: "Dockhand dials OUT to its agents, so moving it needs no change on any managed host — but the new machine must be able to reach every agent address, including any on Tailscale or another private network. " +
				"Its local socket environment points at /var/run/docker.sock, so after a move it manages the NEW host's Docker rather than the old one's; re-point or remove it if that is not what you want. " +
				"Managed containers keep running throughout a restore — only Dockhand's own interface is briefly unavailable.",
			OneWayMigration: "Dockhand migrates its database schema on start and provides no way to downgrade it",
		},
	},
	{
		match:   "termix",
		profile: termixProfile,
	},
	{
		match: "linux-update-dashboard",
		profile: AppProfile{
			Name: "Linux Update Dashboard",
			// Worth stating the second half explicitly. On disk this app is well
			// designed: the vault's master key lives in the container's
			// environment, NOT beside the database, so copying the data directory
			// off the NAS yields ciphertext and a salt and gets you nowhere. A
			// backup necessarily puts both halves in one archive and collapses
			// that separation — which is precisely why the archive's own
			// encryption has to carry the weight the split was carrying.
			CredentialStore: "This dashboard stores the SSH passwords, private keys and certificates it uses to patch and reboot the machines it watches, so a decrypted archive is root-capable access to every one of them — and unlike the running deployment, where the vault key lives in the environment rather than beside the database, a backup necessarily holds both halves together",
			OneWayMigration: "Linux Update Dashboard migrates its database when it starts and provides no way to downgrade it",
			// WAL-mode SQLite under continuous writes from its scheduled checks.
			LocalOnly: []string{"/data"},
			Address: []AddressBinding{
				{
					Kind: BindEnv,
					Keys: []string{"LUDASH_BASE_URL"},
					Note: "LUDASH_BASE_URL is set to the new address on the recreated container.",
				},
				{
					// The one nothing here can reach, and the one whose symptom
					// points at the wrong culprit.
					Kind:    BindExternal,
					Symptom: "single sign-on fails with a redirect-mismatch error while everything else works, which reads as a broken restore and is not one",
					Note: "If you sign in through an identity provider, its registered redirect URI still points at the OLD address. Update it wherever that provider is configured. " +
						"Connections to the servers this dashboard manages are unaffected by a move — those target the managed machines, not this app.",
				},
			},
		},
	},
	{
		match: "guacamole",
		profile: AppProfile{
			Name: "Guacamole",
			// Worse than the others in one specific way: Guacamole must be able to
			// replay connection passwords to the remote host, so it stores them
			// RECOVERABLY rather than hashed. The dump is the credentials.
			CredentialStore: "Guacamole stores the passwords for the systems it connects to in a recoverable form — it has to replay them — so a decrypted archive is direct access to every remote desktop and server it reaches",
			// The all-in-one image bundles guacd, Tomcat AND a full PostgreSQL in
			// one container. Nothing in the image name says so, so DockBack saw an
			// ordinary app and raw-copied the live PGDATA.
			EmbeddedDump: &EmbeddedDump{
				Engine:  "postgres",
				DataDir: "/config/postgres",
				User:    "guacamole",
				DBName:  "guacamole_db",
			},
			// Guacamole needs explicit schema/upgrade SQL between versions and has
			// no downgrade path at all.
			OneWayMigration: "Guacamole upgrades its database schema on start and provides no way to downgrade it",
			LocalOnly:       []string{"/config"},
		},
	},
	{
		// CommaFeed ships the SAME application against two completely different
		// storage backends, distinguished only by the image tag:
		//
		//   :latest-postgresql — an external Postgres, dumped logically. All the
		//                        state is in the database; /commafeed/data is empty.
		//   :latest            — an EMBEDDED H2 database in /commafeed/data, which
		//                        DockBack has no dump tool for.
		//
		// The tagged variant must be matched first, or the Postgres deployment
		// picks up a warning about an embedded database it does not have.
		match:   "commafeed:latest-postgresql",
		profile: commafeedProfile,
	},
	{
		match:   "commafeed",
		profile: commafeedEmbeddedProfile,
	},
	{
		// calibre-web / calibre-web-automated must be matched BEFORE plain
		// "calibre": every calibre-web image name contains the string "calibre",
		// so the more specific entry has to win or the wrong profile applies.
		match:   "calibre-web",
		profile: calibreWebProfile,
	},
	{
		match:   "calibre",
		profile: calibreProfile,
	},
	{
		// Wiki.js ships under several image names and none of them are a reliable
		// single substring: the official image is `requarks/wiki`, while the
		// LinuxServer build is `linuxserver/wikijs`. Both are matched so the
		// registry works for whichever one a given deployment actually runs.
		match:   "wikijs",
		profile: wikiJSProfile,
	},
	{
		match:   "requarks/wiki",
		profile: wikiJSProfile,
	},
}

// nginxProxyManagerProfile covers Nginx Proxy Manager (F141–F145).
//
// NPM is the only app in the registry whose state is split across two volumes
// that are individually useless. /data holds the proxy hosts, the users and the
// JWT signing keys; /etc/letsencrypt holds the certificates those rows point at,
// plus the ACME account and the DNS-provider credentials renewals need. Restore
// one without the other and NPM either refuses to load a certificate it is told
// to serve, or — the failure that actually costs people their configuration —
// comes up factory-fresh and offers the setup wizard to whoever reaches it first.
//
// It is also, in most deployments, the single most sensitive archive on the
// machine: a wildcard private key impersonates every service behind it, and a
// DNS-01 credential can mint new certificates for the whole domain.
var nginxProxyManagerProfile = AppProfile{
	Name: "Nginx Proxy Manager",
	AtomicVolumes: &AtomicVolumeSet{
		Paths: []string{"/data", "/etc/letsencrypt"},
		Why: "Nginx Proxy Manager splits one configuration across two volumes: /data holds the proxy hosts, users and signing keys, and /etc/letsencrypt holds the certificates those hosts are configured to serve. " +
			"Neither half is usable alone, so DockBack keeps them in one archive and restores them together",
		Symptom: "restoring only one of them leaves nginx refusing to start with \"cannot load certificate\", or brings the proxy up factory-fresh with every route gone and the setup wizard waiting for whoever opens it first",
	},
	CriticalTables: []CriticalTable{
		{
			DB:    "/data/database.sqlite",
			Table: "user",
			Means: "the restored database has no accounts at all. That is the state a brand-new Nginx Proxy Manager creates, and starting it would offer the setup wizard to whoever reaches the admin port first",
		},
		{
			DB:    "/data/database.sqlite",
			Table: "proxy_host",
			Means: "the restored database has no proxy hosts, so nothing behind this proxy would be reachable",
		},
	},
	// live/ is certbot's layout; custom_ssl/ is where NPM keeps certificates that
	// were uploaded rather than issued.
	CertificateRoots: []string{"/etc/letsencrypt/live", "/data/custom_ssl"},
	// Stopping — or even briefly pausing — the fleet's ingress stalls every
	// request to every service behind it. There is nothing to gain: the database
	// is captured by the online SQLite snapshot, which is consistent without
	// quiescing, and everything else in both volumes is static files.
	DefaultPause: PauseNone,
	DefaultPauseWhy: "this is the ingress proxy for everything behind it, and pausing it stalls every request to every one of those services. " +
		"Nothing is lost by not pausing: the database is captured by a consistent online snapshot that does not need the container held still, and the certificates are static files",
	// 10 MB of rotated nginx logs against ~130 KB of configuration, and they
	// restore nothing.
	NeverBackup: []NeverBackupPath{
		{Path: "/data/logs", Why: "rotated nginx access and error logs"},
	},
	// database.sqlite is written continuously while the proxy runs.
	LocalOnly: []string{"/data"},
	CredentialStore: "This archive holds the private keys for every certificate Nginx Proxy Manager serves — a wildcard key impersonates every service behind it — together with the DNS-provider API credentials it uses to renew them, which can create further certificates for the whole domain. " +
		"It is worth treating as the most sensitive backup on the machine",
	OneWayMigration: "Nginx Proxy Manager migrates its database schema on start and provides no way to downgrade it — and its own documented behaviour on a schema it does not understand is to fall back to a fresh configuration",
	ControlPlane: "Proxy hosts route by domain to an upstream address, not by Nginx Proxy Manager's own address, so moving it needs no change to a single route — but the new host must be able to reach every upstream those routes point at, and ports 80, 443 and 81 must be free there. " +
		"Renewals need outbound access to the certificate authority, and to your DNS provider's API if you use DNS-01 validation.",
	Address: []AddressBinding{
		{
			// NPM genuinely stores no address of its own. What points clients at
			// it is DNS, a tunnel or a router forward — none of which DockBack
			// can see, let alone change.
			Kind:    BindManual,
			Symptom: "nothing reaches the restored proxy at all, which looks like a failed restore and is not one — the proxy is correct and simply nothing is being sent to it",
			Note: "Nginx Proxy Manager keeps no address of its own, and its certificates are bound to domain names rather than to an IP, so they stay valid on any machine and nothing needs reissuing. " +
				"What points clients at it does need updating if the machine's address changes: the DNS record, tunnel or router forward that sends traffic to it. That lives outside DockBack's reach.",
		},
	},
}

// termixProfile covers Termix, which stores SSH credentials for a fleet
// (F163–F165).
//
// The most sensitive archive DockBack produces, and for a reason that is a
// property of the application rather than of its data.
//
// Termix encrypts its database, and keeps the key that decrypts it in the same
// directory. That is a reasonable design for its actual threat — a disk taken
// out of a machine — and it means something specific for a backup: copying the
// data directory copies both halves. Its at-rest encryption offers no protection
// at all to anyone holding a copy of the directory, which is exactly what a
// backup is.
//
// So for this application the ARCHIVE'S OWN encryption is not one layer among
// several. It is the only one. And an archive sealed to the master key is
// protected by a key that lives on a running server — frequently one of the
// machines whose credentials are inside it.
//
// DockBack never opens the database. It is captured as the opaque blob it
// already is, so no plaintext credential exists anywhere in the pipeline at any
// stage — nothing to leak from a log, a temporary file or a report.
var termixProfile = AppProfile{
	Name: "Termix",
	CredentialStore: "Termix stores SSH credentials and private keys for the machines it connects to, so a decrypted archive is shell access to every one of them — and because Termix keeps the key that decrypts its database in the same directory as the database, this archive's own encryption is the only thing protecting them. " +
		"Turn on write-only encryption and mark this container as requiring it. If an archive and its offline key are ever exposed together, treat it as a full compromise: rotate every credential and key Termix holds",
	// The database is encrypted, so it is not a SQLite file DockBack can
	// snapshot — the quiesce below is the entire consistency mechanism.
	EmbeddedDBWarning: "Termix keeps its whole state in an ENCRYPTED database, which DockBack captures as an opaque blob and never opens. That is the right thing for the credentials inside it, and it means there is no consistent-snapshot path available: " +
		"the container is stopped for the copy instead, so the application flushes and closes its database cleanly first. Leave the consistency setting on stop, or a copy can be taken mid-write.",
	// A stop, not a pause. Pausing freezes the process with its state still in
	// memory; stopping asks it to flush and close, which is the difference
	// between a consistent encrypted file and a plausible-looking one.
	DefaultPause: PauseStop,
	DefaultPauseWhy: "its database is encrypted, so there is no way to take a consistent snapshot of it from outside — stopping the container asks the application to flush and close it cleanly, which a pause does not. " +
		"The database is small, so this is seconds",
	// Blocked in BOTH directions. Termix is young and reworked its database layer
	// recently; neither an upgrade nor a downgrade has a demonstrated
	// compatibility story, and for a credential store "it opened but decrypted
	// wrong" is a far worse outcome than a refused restore.
	VersionLock: "Termix reworked how it stores and encrypts its data recently, and neither restoring forward nor restoring back has a demonstrated compatibility story for that format",
	// The key file. Termix generates it; its mode is worth knowing, because it is
	// the file that makes the database readable.
	SecretFiles: []SecretFile{
		{Path: "/app/data/.env", MaxMode: 0o600, What: "the file holding the keys that decrypt this application's database"},
	},
	// The whole directory is the deployment's identity. Nothing in it is
	// regenerable, and losing the key file makes the database permanently
	// unreadable — so nothing here is ever excluded.
	LocalOnly: []string{"/app/data"},
	Address: []AddressBinding{
		{
			// Termix records no address of its own, and its stored connections
			// point at OTHER machines — which are unaffected by moving Termix.
			Kind:    BindManual,
			Symptom: "nothing, in the common case — the stored connections point at other machines, and those have not moved",
			Note: "Termix keeps no address of its own, and the hosts it connects to are recorded against their own addresses — so moving Termix changes nothing about what it can reach, and no connection needs re-entering. " +
				"Its keys travel inside the archive with the database, because one is useless without the other. " +
				"Restore each Termix instance from its OWN backup: two deployments have separate keys and separate databases, and an archive from one cannot open in the other.",
		},
	},
}

// tautulliProfile covers Tautulli, which watches a media server and keeps the
// history (F160–F162).
//
// Its whole state is one directory, and two thirds of what used to be backed up
// from it was cache and rotated logs — 133 MB of an archive that should be 25.
//
// What makes it worth a profile is the dependency. Tautulli holds the address of
// the media server it reads from and a token that authenticates to it, and the
// token is bound to that server's IDENTITY rather than to any address. So moving
// Tautulli needs nothing at all — not a re-link, not a wizard — while moving the
// media server needs exactly one value changed. That asymmetry is invisible from
// the outside, and getting it wrong is quiet: the application starts, reports
// healthy, and collects nothing.
var tautulliProfile = AppProfile{
	Name: "Tautulli",
	// Two thirds of the old archive, and neither restores anything.
	NeverBackup: []NeverBackupPath{
		{Path: "/config/cache", Why: "image and artwork cache, refetched on demand"},
		{Path: "/config/logs", Why: "rotated application logs"},
	},
	// Tautulli's own scheduled database copies. Real value as a portable extra —
	// they are consistent snapshots the application can restore itself from — so
	// this is a choice rather than an always-exclusion. They also hold the same
	// credentials the live configuration does, which is worth knowing before
	// deciding.
	Regenerable: []RegenerablePath{{
		Path:  "/config/backups",
		Label: "Tautulli's own scheduled backup copies",
		Cost: "nothing is lost for a DockBack restore — the live database is captured directly by a consistent snapshot, and that is what a restore uses. " +
			"They are a portable extra Tautulli can restore itself from, and they carry the same credentials the live configuration does",
	}},
	Upstream: &UpstreamAddress{
		Service:  "Plex",
		File:     "/config/config.ini",
		Section:  "PMS",
		URLKeys:  []string{"pms_url"},
		HostKeys: []string{"pms_ip"},
		Why:      "Tautulli records the address of the media server it reads from, alongside a token that is bound to that server's identity rather than to its address",
		Symptom:  "Tautulli starts, reports healthy, and silently stops collecting anything, because the server it reads from is no longer where it was told to look",
		// Reads the address, token and expected identity out of the application's
		// own configuration, asks the server who it is, and prints ONLY a verdict.
		// The token is used inside the container and never printed, returned or
		// logged. A missing python3 or an unparsable configuration prints nothing
		// recognisable, which the caller reads as "no verdict" rather than a
		// failure.
		Probe:   []string{"/bin/sh", "-c", tautulliPlexProbe},
		ProbeOK: "PLEXCHK ok",
	},
	OneWayMigration: "Tautulli migrates its database when it starts and provides no way to downgrade it",
	CredentialStore: "Tautulli's configuration holds a live token for the media server it watches, its own API key, and the credentials for every notifier it sends to — so a decrypted archive reaches the media server and everyone's viewing history. " +
		"If one ever leaks, sign that token out from the media account's authorised devices and rotate the API key",
	// F142: an empty history after a restore is not a shortfall, it is a fresh
	// install wearing the same name.
	CriticalTables: []CriticalTable{
		{DB: "/config/tautulli.db", Table: "session_history", Means: "the restored database has no watch history at all, which is the state a brand-new install starts in"},
		{DB: "/config/tautulli.db", Table: "users", Means: "the restored database knows no users, so nothing that was recorded can be attributed to anyone"},
	},
	// F147: the configuration file holds the media-server token and the hashed
	// web password.
	SecretFiles: []SecretFile{
		{Path: "/config/config.ini", MaxMode: 0o640, What: "the file holding the media-server token, the API key and the hashed web password"},
	},
	Address: []AddressBinding{
		{
			// Tautulli records no address of its own by default. Said anyway,
			// because the interesting half is the sentence after it.
			Kind:    BindManual,
			Symptom: "links in notifications and newsletters point at the old address, while everything else works",
			Note: "Tautulli usually stores no address of its own, so a move needs nothing — check Settings → Web Interface only if you deliberately set a public URL there. " +
				"What DOES matter on a move is the other direction: if the media server it watches has also moved, give its new address in the field above. Nothing is re-authenticated either way.",
		},
	},
}

// tautulliPlexProbe asks the media server who it is, using the token already in
// the application's own configuration, and prints one line.
//
// Everything happens inside the container. The token is read from the file, used
// for one request, and never printed — the only output is `PLEXCHK ok`,
// `PLEXCHK different-server`, or `PLEXCHK unreachable <status>`. Written in
// Python because the application is a Python one, so an interpreter is the one
// tool guaranteed to be there.
const tautulliPlexProbe = `command -v python3 >/dev/null 2>&1 || exit 0; python3 - <<'PY'
import configparser, urllib.request, ssl, re
try:
    c = configparser.ConfigParser()
    c.read("/config/config.ini")
    pms = c["PMS"]
    url = pms.get("pms_url", "").strip().strip('"').rstrip("/")
    tok = pms.get("pms_token", "").strip().strip('"')
    want = pms.get("pms_identifier", "").strip().strip('"')
    if not url or not tok:
        raise SystemExit(0)
    req = urllib.request.Request(url + "/identity", headers={"X-Plex-Token": tok, "Accept": "application/xml"})
    ctxs = ssl._create_unverified_context()
    with urllib.request.urlopen(req, timeout=10, context=ctxs) as r:
        body = r.read(4096).decode("utf-8", "replace")
        code = r.status
    m = re.search(r'machineIdentifier="([^"]+)"', body)
    got = m.group(1) if m else ""
    if code == 200 and want and got == want:
        print("PLEXCHK ok")
    elif code == 200 and got and want and got != want:
        print("PLEXCHK different-server")
    elif code == 200:
        print("PLEXCHK reachable-unverified")
    else:
        print("PLEXCHK unreachable", code)
except SystemExit:
    raise
except Exception as e:
    print("PLEXCHK unreachable", type(e).__name__)
PY`

// uptimeKumaProfile covers Uptime Kuma (F166–F169).
//
// Interesting for one reason: the SAME IMAGE runs two completely different
// databases. Version 2 can keep its state in a plain SQLite file, or run a full
// MariaDB server inside the application container — and the MariaDB client is
// present either way, so nothing about the image tells you which.
//
// That matters because the right backup method is different for each, and
// getting it wrong is not a near miss. The deployment investigated here was
// running the embedded MariaDB and was being backed up by copying its live
// 322 MB data directory while the server wrote heartbeats into it every twenty
// seconds — a hot copy of a running database, which is the one thing the dump
// path exists to prevent. The logical dump replaces it, is consistent, needs no
// downtime at all, and is a fraction of the size.
//
// So the declaration is conditional: probe the application's own configuration,
// and take the file path — which snapshots the SQLite database properly — when
// the answer is that there is no server to dump.
var uptimeKumaProfile = AppProfile{
	Name: "Uptime Kuma",
	EmbeddedDump: &EmbeddedDump{
		Engine:  "mysql",
		DataDir: "/app/data/mariadb",
		User:    "root",
		DBName:  "kuma",
		// The application puts its socket where the client will not look on its
		// own, so every connection would fail for a reason that reads like an
		// authentication problem.
		Socket: "/app/data/run/mariadb.sock",
		// The application's own configuration is the only thing that knows which
		// way this deployment runs. Read from the file it actually uses, so a
		// deployment that switched between the two is followed rather than assumed.
		When: []string{"/bin/sh", "-c",
			`[ -f /app/data/db-config.json ] || exit 1; grep -q '"embedded-mariadb"' /app/data/db-config.json`},
		// What the application is FOR. Its tables being present says very little;
		// these counts are what say the history came back.
		CountTables: []string{"monitor", "heartbeat", "notification", "user", "status_page", "api_key"},
	},
	// Runtime scratch and a log. Neither restores anything, and the socket in
	// particular is meaningless once copied.
	NeverBackup: []NeverBackupPath{
		{Path: "/app/data/run", Why: "the running server's socket and process id"},
		{Path: "/app/data/error.log", Why: "application log"},
	},
	// The application's own documentation is explicit that its database on a
	// network share corrupts — and this is the deployment's whole history.
	LocalOnly: []string{"/app/data"},
	CredentialStore: "Uptime Kuma's database holds the credentials for every notification channel it sends through, its own API keys, and the sign-in and two-factor secrets of its users — so a decrypted archive can send alerts as you and reach whatever those channels connect to. " +
		"If one ever leaks, rotate the notification credentials and API keys inside Uptime Kuma, and the database password in its configuration file",
	// Knex migrations run on start and there are no down-migrations.
	OneWayMigration: "Uptime Kuma runs its database migrations when it starts and provides no way to downgrade them",
	ControlPlane: "Uptime Kuma's monitors check absolute addresses, so moving Uptime Kuma itself changes none of them — every check keeps working from the new host, provided it can reach the same things. " +
		"What the new host must be able to do is reach each monitored target, and reach whatever the notification channels send to.",
	Address: []AddressBinding{
		{
			// Nothing inside Uptime Kuma needs to change on a move — but two
			// things it PUBLISHES have the old address baked in, and both fail
			// quietly rather than visibly.
			Kind:    BindManual,
			Symptom: "the public status page and any outgoing links keep pointing at the old address, which nothing in the application will complain about",
			Note: "Uptime Kuma stores no address of its own that it needs in order to run, so a move needs no reconfiguration and every monitor keeps checking. " +
				"Two things do carry the old address and are set in the application's own settings rather than anywhere DockBack can reach: the status page's public URL, and any notification that includes a link back. " +
				"Update those in Settings after a move — nothing will warn you.",
		},
	},
}

// radicaleProfile covers Radicale, the CalDAV/CardDAV server (F157–F158).
//
// The simplest application in the registry, and it earns its entry for two
// reasons rather than for any difficulty in backing it up.
//
// The first is what its data is: calendars, contacts, and a file of password
// hashes, in a tree small enough that nobody thinks about it. Small and
// sensitive is a combination that gets overlooked.
//
// The second is that Radicale writes the way careful software does — stage the
// change in a temporary directory, rename it into place — which means an
// interrupted write leaves a staging directory behind that nothing will ever
// clean up. Two of them sat in one production tree for a year, roughly doubling
// the item count of every backup taken of it. Nothing was wrong; nobody looked.
var radicaleProfile = AppProfile{
	Name: "Radicale",
	StagingDebris: []StagingDebrisPattern{{
		Glob:       ".Radicale.tmp-*",
		MinAge:     time.Hour,
		What:       "leftover staging directory from a write that was interrupted",
		WhatPlural: "leftover staging directories from writes that were interrupted",
		Cleanup: "Radicale ignores this and the backup is correct — nothing is broken. " +
			"DockBack never deletes anything it finds, so clearing it is yours to do: stop the container, remove the directory listed above, and start it again. Only after a backup you have verified.",
	}},
	// A file of bcrypt hashes is only as protected as its mode, and this one sits
	// in a config directory people copy around.
	SecretFiles: []SecretFile{
		{Path: "/config/users", MaxMode: 0o640, What: "the file holding this server's password hashes"},
	},
	CredentialStore: "Radicale's backup holds your calendars and contacts together with the file of password hashes that guards them, so a decrypted archive is both the data and a head start on the credentials for it. " +
		"If one ever leaks, change the affected account passwords — the hashes are bcrypt, which buys time rather than safety",
	// The on-disk format has been stable across the whole of Radicale 3. There is
	// nothing to gate, and the useful thing to say is where the edge actually is.
	StorageCompat: "Radicale's collection format is stable across all of version 3, so a backup restores into any 3.x server. Version 2 used a different storage layout — do not restore this into a 2.x image.",
	Address: []AddressBinding{
		{
			// Collections are addressed by path, and the path comes back verbatim
			// with the tree. So whether a client survives a move depends entirely
			// on what the operator pointed it at — which is a decision made long
			// before the restore.
			Kind:    BindManual,
			Symptom: "clients configured against the server's IP address stop syncing and have to be re-added by hand, one device at a time",
			Note: "Radicale addresses each collection by its path, and the paths come back exactly as they were — so a client reaches the same collection on the new machine with no change, provided it can still find the server. " +
				"A client pointed at a DOMAIN follows automatically once that domain points at the new host. A client pointed at an IP address does not, and has to be re-configured on every device. " +
				"Moving clients onto a domain name before a move is what turns a device-by-device job into a single change.",
		},
	},
}

// pmsDataDir is Plex's data directory inside the container. Every path below
// hangs off it, and every one of them contains spaces — which is exactly the
// kind of detail an exclusion list gets wrong silently, so they are written out
// in full rather than assembled from fragments.
const pmsDataDir = "/config/Library/Application Support/Plex Media Server"

// plexProfile covers Plex Media Server (F155–F156).
//
// Two things make Plex unlike anything else in this registry.
//
// The first is that its identity is REGISTERED WITH A CLOUD SERVICE. The server
// identifier and the account token live in Preferences.xml, and restoring that
// file verbatim is exactly right for a real recovery — same identity, so every
// client that was already paired keeps working with nothing to re-link. It is
// exactly wrong for a rehearsal: a copy carrying the same identity announces
// itself as the server still running, and the two contend. Hence the clone
// redaction.
//
// The second is proportion. Its data directory is mostly artwork and metadata
// that took hours to build and must be kept, sitting beside half a gigabyte of
// caches, logs and downloaded codecs that are re-fetched on the next start. A
// backup that copies it wholesale is both far larger than it needs to be and,
// because the database underneath is being written to continuously, less
// trustworthy than it looks.
var plexProfile = AppProfile{
	Name: "Plex",
	// Never backed up: none of it survives a restart in any meaningful sense,
	// and together it is hundreds of megabytes per archive.
	NeverBackup: []NeverBackupPath{
		{Path: pmsDataDir + "/Cache", Why: "regenerated cache"},
		{Path: pmsDataDir + "/Logs", Why: "server logs"},
		{Path: pmsDataDir + "/Crash Reports", Why: "crash reports"},
		{Path: pmsDataDir + "/Codecs", Why: "codecs re-downloaded per version and architecture"},
		{Path: pmsDataDir + "/Drivers", Why: "hardware drivers re-downloaded on start"},
		{Path: pmsDataDir + "/Updates", Why: "downloaded installers"},
		{Path: pmsDataDir + "/Diagnostics", Why: "diagnostic bundles"},
		{Path: pmsDataDir + "/Plug-in Support/Caches", Why: "plug-in caches"},
	},
	// The media library itself. Kept OUT of the backup by the ordinary
	// large-bind default, and named here because a restore onto a host that does
	// not mount it produces a server whose every library reads as unavailable.
	PathEmbedding: "Plex stores each library's location as an absolute path inside the container, so the target must mount the same media at the same container paths",
	// Preferences.xml holds the account token; the databases hold every user,
	// share and viewing history.
	CredentialStore: "Plex's configuration holds the token that links this server to your Plex account, so a decrypted archive can act as that server and reach the account it belongs to. " +
		"If one ever leaks, sign the token out from your Plex account's authorised devices — that invalidates it without touching anything else",
	// The server migrates its database forward on start and an older build
	// cannot open a migrated one.
	OneWayMigration: "Plex migrates its database when it starts and an older server cannot open a migrated one",
	// A 400 MB database with a write-ahead log of the same size again: the one
	// place in this whole tree where a network filesystem does real damage.
	LocalOnly: []string{"/config"},
	// F155: what a rehearsal must not do.
	CloneRedactions: []CloneRedaction{{
		Path:     pmsDataDir + "/Preferences.xml",
		Attrs:    []string{"PlexOnlineToken", "PublishServerOnPlexOnlineKey"},
		Required: true,
		Why: "a copy of Plex carries the same server identity and account token as the original, and it needs nothing but outbound access to announce itself as that server — " +
			"so the copy would contend with the server you are still running. Cleared here, the copy can be inspected locally and cannot reach your Plex account",
	}},
	// F147: Preferences.xml is where the token lives.
	SecretFiles: []SecretFile{
		{Path: pmsDataDir + "/Preferences.xml", MaxMode: 0o640, What: "the file holding this server's Plex account token"},
	},
	// F142: an empty library after a restore is not "some rows missing" — it is a
	// server that came back as a fresh install.
	CriticalTables: []CriticalTable{
		{
			DB:    pmsDataDir + "/Plug-in Support/Databases/com.plexapp.plugins.library.db",
			Table: "library_sections",
			Means: "the restored database has no libraries at all, which is the state a brand-new server starts in",
		},
		{
			DB:    pmsDataDir + "/Plug-in Support/Databases/com.plexapp.plugins.library.db",
			Table: "metadata_items",
			Means: "the restored database has no items in any library — the artwork and matches that took hours to build are not there",
		},
	},
	ControlPlane: "Plex runs on the host's own network so it can be discovered on the LAN, and a restore reproduces that. " +
		"Its hardware-transcoding device is specific to the machine it was configured on: on a target without the same device Plex falls back to transcoding in software, which works but costs far more processor time — reconfigure it in Settings after the move. " +
		"Codecs and drivers are deliberately not in the backup; the restored server re-downloads them on first start, so give it internet access before judging it.",
	Address: []AddressBinding{
		{
			// Plex's identity is what clients follow, not an address — which is
			// the good news, and also the thing that makes a second copy
			// dangerous.
			Kind:    BindManual,
			Symptom: "clients connect to whichever server published the identity most recently, which on a bad day is the one you were trying to retire",
			Note: "Plex clients find this server by its identity rather than its address, so a move needs no change on any client and nothing needs re-pairing — the restored server publishes itself and they reconnect. " +
				"What that requires is that only ONE server carries the identity at a time: stop the old one before the restored one starts. " +
				"Restoring a copy for a rehearsal is safe — DockBack clears the account token from the copy so it cannot publish at all.",
		},
	},
}

// pangolinProfile covers the Pangolin stack (F146–F149).
//
// Two things make it unlike anything else in this registry.
//
// The first is that its state is split across CONTAINERS rather than across
// paths. Pangolin holds the database — organisations, sites, resources, users,
// and the per-site secrets its agents authenticate with. Gerbil holds a
// 44-byte WireGuard private key that IS the tunnel's identity. Traefik holds the
// certificate store. Restore the database without the key and every agent
// authenticates to an endpoint that is no longer the one it trusts; restore the
// key without the database and the endpoint no longer knows any of the sites
// that trust it. Neither failure says anything useful in a log.
//
// The second is what a decrypted archive is worth. It is simultaneously the
// identity of a VPN concentrator, the private keys for every certificate the
// proxy serves, and the session-signing secret of the control plane. There is
// no single archive in a typical deployment worth more.
var pangolinProfile = AppProfile{
	Name: "Pangolin",
	StackAtomic: &StackAtomicSet{
		Why: "Pangolin's state is split across the services of its stack: the application holds the database of sites, resources and per-site secrets, its tunnel service holds the WireGuard private key that is the endpoint's identity, and the proxy holds the certificate store. " +
			"They are one configuration in several containers, so DockBack captures and restores them as one unit",
		Symptom: "restoring part of the set leaves the tunnel identity and the site secrets disagreeing — every remote site fails to reconnect, and nothing in the logs says why",
		SoloRestore: "Restoring one service of this stack on its own is the specific way this breaks: the service comes back healthy, and the deployment does not work. " +
			"Restore the whole stack from one app-consistent snapshot instead. To inspect this backup without touching the live stack, restore it as an isolated copy — that is always allowed.",
	},
	// The WireGuard private key is the highest-value 44 bytes on the machine, and
	// it is routinely left world-readable by the stack's own setup.
	SecretFiles: []SecretFile{
		{Path: "/var/config/key", MaxMode: 0o600, What: "the WireGuard private key that is this tunnel endpoint's identity"},
		{Path: "/app/config/key", MaxMode: 0o600, What: "the WireGuard private key that is this tunnel endpoint's identity"},
		{Path: "/letsencrypt/acme.json", MaxMode: 0o600, What: "the certificate store, which holds the private key for every certificate served"},
	},
	CredentialStore: "A decrypted archive of this stack is the identity of your VPN endpoint, the private keys for every certificate it serves, and the signing secret of its control plane at once — enough to impersonate the tunnel every remote site connects to, serve valid TLS for your domains, and administer every exposed resource. " +
		"If one ever leaks, treat it as a full tunnel, certificate and administrative compromise: regenerate the WireGuard key and re-pair every site, reissue every certificate, and rotate the server secret and any bouncer keys",
	// Pangolin migrates its schema when it starts, forward only.
	OneWayMigration: "Pangolin migrates its database when it starts and provides no way to downgrade it",
	// A continuously-written SQLite database, and — unusually — one under real
	// write load, since the application logs traffic to it.
	LocalOnly: []string{"/app/config"},
	// The database is large and busy enough that a clean shutdown is worth a few
	// seconds of downtime — and unlike an ingress proxy, this one is the thing
	// being protected rather than the path to everything else. Only this service
	// stops: its siblings hold static files and are merely frozen for the copy.
	DefaultPause: PauseStop,
	DefaultPauseWhy: "its entire state is one busy SQLite database, and stopping it for the few seconds of the copy gives a pristine snapshot instead of one the database has to recover on next open. " +
		"Only this service stops — the rest of the stack holds static files and is only briefly frozen",
	// Pangolin writes its own periodic database copies inside the captured tree.
	// Real value as a portable extra, and they roughly double the archive, so
	// this is the operator's call rather than an always-exclusion.
	Regenerable: []RegenerablePath{{
		Path:  "/app/config/db/backups",
		Label: "Pangolin's own database copies",
		Cost:  "nothing is lost for a DockBack restore — the live database is captured directly and is what a restore uses. These are a portable extra you can open by hand; on a large deployment they roughly double the archive",
	}},
	ControlPlane: "Moving this stack changes nothing inside it: remote agents dial the endpoint by DOMAIN, and the tunnel identity and site secrets travel with the backup, so no agent needs reconfiguring and no site needs re-pairing. " +
		"What the new host must provide is reachability — inbound UDP for the WireGuard listener, and inbound TCP 80 and 443. Port 80 is not optional if certificates are issued by HTTP challenge: that is how the certificate authority proves the domain, and it is checked against whatever address the DNS record points at.",
	Address: []AddressBinding{
		{
			// The one that decides whether a move works at all, and the one
			// DockBack cannot reach: agents and clients find this stack through
			// DNS, which lives at a registrar.
			Kind:    BindManual,
			Symptom: "no remote site reconnects and nothing behind the proxy is reachable — which reads as a failed restore and is not one; the stack is correct and nothing is being sent to it",
			Note: "Remote agents reach this stack by domain name, so a move needs no change on any agent and no site needs re-pairing — but the DNS records still point at the old machine. " +
				"Update the record for the endpoint domain, and the records for every exposed subdomain (or the wildcard covering them), to the new address. " +
				"If a proxy service sits in front, it is the ORIGIN record that changes. " +
				"Certificates already issued keep working for the rest of their life, so there is no immediate outage — which is exactly the trap: if certificates are issued by HTTP challenge, every renewal fails silently until DNS points here. " +
				"Force one renewal after the move rather than finding out when the first certificate expires. " +
				"Only if the endpoint DOMAIN itself changes does every remote agent need its endpoint updated.",
		},
	},
}

// karakeepProfile is shared by the karakeep and hoarder image names.
//
// The whole app is one stateful service: db.db holds the bookmarks AND every
// piece of AI-generated data — which tags a model attached rather than a person,
// the generated summaries, the crawled descriptions. None of it is regenerated
// on restore, and none of it could be: re-running the models would produce
// different tags against a different model version.
var karakeepProfile = AppProfile{
	Name:            "Karakeep",
	OneWayMigration: "Karakeep migrates its database when it starts and provides no way to downgrade it",
	// F196: the reindex advice below names what the action does NOT do, because
	// an operator with AI-generated tags read "reindex" as "regenerate" and
	// nearly avoided a rebuild their search needed. Verified against the shipped
	// image rather than assumed: the admin panel's reindex mutation feeds only
	// the SearchIndexingQueue, while "Regenerate AI tags" and "Recrawl links"
	// are separate mutations (reRunInferenceOnAllBookmarks / recrawlLinks) with
	// their own queues. AI tags and summaries are rows in the database, so they
	// are captured and restored like every other row.
	DerivedServices: "Karakeep's Meilisearch index is built FROM its database, not alongside it — so it does not need backing up, and leaving it out avoids tying the archive to one Meilisearch version. " +
		"Rebuild it after a restore with Karakeep's \"Reindex all bookmarks\" action; until that finishes, search is incomplete while everything else is already correct. " +
		"Reindexing only rebuilds the search index from data already in the database — it runs no AI and refetches no pages, and your AI-generated tags and summaries are database rows that the restore has already brought back. " +
		"The expensive actions are the OTHER two admin buttons, \"Regenerate AI tags\" and \"Recrawl all links\" — a restore needs neither. " +
		"The Chrome container is stateless and needs nothing at all.",
}

// wikiJSProfile is shared by Wiki.js's two common image names.
//
// Wiki.js keeps essentially all of its state — pages, history, users, groups,
// permissions, settings and its own JWT signing certificate — in PostgreSQL, and
// applies pending schema migrations when it starts.
var wikiJSProfile = AppProfile{
	Name:            "Wiki.js",
	OneWayMigration: "Wiki.js migrates its database schema on start and ships no down-migrations",
	// F170: two things a restore genuinely cannot bring back, and neither is
	// visible from anywhere except the application's own configuration.
	//
	// With the defaults there is nothing to say: pages, history, users,
	// permissions and even the uploaded attachments all live in the database, so
	// importing it restores the whole wiki. These fire only for a deployment that
	// has moved part of itself somewhere else.
	//
	// Both are read as: one row per active thing, no rows when it does not apply.
	// Column names are double-quoted because the schema is camel-cased.
	PostRestoreNotes: []PostRestoreNote{
		{
			Engine: "postgres",
			SQL:    `SELECT 1 FROM "searchEngines" WHERE "isEnabled" = true AND key <> 'db'`,
			Note: "this wiki searches through an external search engine, and that engine's index is not part of the backup — the pages are all here, but searching will find nothing until the index is rebuilt. " +
				"Rebuild it from Administration → Search Engine. Everything else about the restore is complete.",
		},
		{
			Engine: "postgres",
			SQL:    `SELECT 1 FROM storage WHERE "isEnabled" = true AND key = 'git'`,
			Note: "this wiki mirrors its content to a Git repository, and that connection is about to resume using the credentials that were just restored. " +
				"Check the remote and its deploy key before it syncs — the database is the source of truth, so a mirror pointed somewhere unexpected is worth catching first.",
		},
	},
	// The cache is rebuilt from the database on demand, and on a large wiki it is
	// the bulk of what is on disk. Excluding it is the operator's call, because
	// what it costs is a slower first few page loads and nothing else.
	Regenerable: []RegenerablePath{{
		Path:  "/data/cache",
		Label: "Rendered page cache",
		Cost:  "nothing is lost — Wiki.js re-renders each page from the database the first time it is asked for after a restore",
	}},
	Address: []AddressBinding{
		{
			// Wiki.js page links are RELATIVE, so the wiki works fine at a new
			// address without touching anything. Only absolute URLs — the ones
			// in notification emails and auth redirects — use the stored host,
			// and it lives in a `settings` row with no CLI to set it.
			Kind:    BindManual,
			Symptom: "the wiki works, but links in notification emails and login redirects point at the old address",
			Note: "Wiki.js keeps this in its database with no command-line tool to change it. " +
				"Set it in the Wiki.js admin area under Administration → General → Site URL.",
		},
	},
}

// applyNextcloudTrustedDomain names the built-in occ procedure (see
// applyAddressBinding in restore.go).
const applyNextcloudTrustedDomain = "nextcloud-trusted-domains"

// commafeedMigration is shared by both CommaFeed variants: the application
// upgrades its own schema when it starts and ships no way back down, whichever
// database is behind it.
const commafeedMigration = "CommaFeed migrates its database schema on start and provides no way to downgrade it"

// commafeedProfile covers the external-Postgres build, where the database
// container is backed up by a logical dump and /commafeed/data holds nothing.
var commafeedProfile = AppProfile{
	Name:            "CommaFeed",
	OneWayMigration: commafeedMigration,
}

// commafeedEmbeddedProfile covers the default build, which keeps an EMBEDDED H2
// database inside its data directory.
//
// This is the case worth flagging. H2 is not SQLite, so the consistent-snapshot
// path does not apply to it and DockBack has no dump tool for it — the file is
// captured as a file. That is sound when the container is quiesced for the copy
// (the default) and a torn copy waiting to happen when it is not, and nothing
// previously said so. The distinction is invisible from the outside: same
// application, same UI, same container name, and only the image tag differs.
var commafeedEmbeddedProfile = AppProfile{
	Name:            "CommaFeed",
	OneWayMigration: commafeedMigration,
	// The H2 file lives here and must not be on a network share, for the same
	// locking reason an embedded database never should be.
	LocalOnly: []string{"/commafeed/data"},
	EmbeddedDBWarning: "CommaFeed's default image keeps all of its state — feeds, subscriptions, read and starred status, accounts and API keys — in an embedded H2 database inside /commafeed/data. " +
		"DockBack has no dump tool for H2, so that file is captured as a file: keep this container's \"Consistency during volume backup\" set to pause or stop, or the copy can be taken mid-write. " +
		"The :latest-postgresql image instead uses a separate Postgres container, which is dumped logically and needs none of this.",
}

// calibreLocalOnly is the reason both Calibre variants must keep their data off
// network storage.
//
// Calibre's own documentation is unusually blunt about this: a library on a
// network filesystem corrupts. metadata.db is WAL-mode SQLite and depends on
// POSIX advisory locking that CIFS and NFS do not honour, and the failure is
// silent for days before it surfaces as an unreadable library.
var calibreLocalOnly = []string{"/config", "/calibre-library"}

// calibreProfile covers desktop Calibre (the LinuxServer image), which manages
// the library itself.
var calibreProfile = AppProfile{
	Name: "Calibre",
	// The library's book folders are referenced from metadata.db by their
	// position under the library root, so the library must come back at the same
	// container destination or every book reads as missing.
	PathEmbedding:   "Calibre records each book's location relative to its library folder, so the library must be mounted at the same container path",
	LocalOnly:       calibreLocalOnly,
	OneWayMigration: "Calibre upgrades its library database (metadata.db) on start and provides no way to downgrade it",
}

// calibreWebProfile covers calibre-web and its calibre-web-automated fork, which
// READ a library another container manages while keeping their own users,
// permissions and settings in a separate database.
var calibreWebProfile = AppProfile{
	Name:            "calibre-web",
	PathEmbedding:   "calibre-web stores the location of the Calibre library it reads, so the library must be mounted at the same container path",
	LocalOnly:       calibreLocalOnly,
	OneWayMigration: "calibre-web migrates its application database (app.db) on start and provides no way to downgrade it",
}

// ProfileFor returns the restore profile for an image, or nil when the image is
// not one DockBack has preconditions for — which is the overwhelming majority,
// so an ordinary container's restore path is unchanged.
func ProfileFor(image string) *AppProfile {
	img := strings.ToLower(image)
	for _, p := range appProfiles {
		if strings.Contains(img, p.match) {
			pr := p.profile
			return &pr
		}
	}
	return nil
}

// LocalOnlyPath reports whether a container destination is one this profile
// requires to be on local disk. A path under a local-only root counts too:
// /config/metadata inherits /config's requirement.
func (p *AppProfile) LocalOnlyPath(dest string) bool {
	if p == nil || dest == "" {
		return false
	}
	for _, l := range p.LocalOnly {
		if dest == l || strings.HasPrefix(dest, strings.TrimSuffix(l, "/")+"/") {
			return true
		}
	}
	return false
}

// networkFilesystems are the backing filesystems that cannot safely host an
// embedded database. SQLite's locking depends on POSIX advisory locks behaving
// correctly, which none of these guarantee — the failure is silent corruption,
// not a refused write, which is exactly why it has to be caught up front.
var networkFilesystems = map[string]bool{
	"cifs": true, "smbfs": true, "smb2": true, "smb3": true,
	"nfs": true, "nfs4": true, "afs": true, "9p": true,
	"fuse.sshfs": true, "fuse.rclone": true, "fuse.glusterfs": true,
	"glusterfs": true, "ceph": true, "lustre": true,
}

// IsNetworkFilesystem reports whether a filesystem type as reported by `stat
// -f -c %T` is a network/shared filesystem. Unknown types return false: a guard
// that blocks on anything it does not recognise would refuse valid restores on
// perfectly ordinary filesystems.
func IsNetworkFilesystem(fstype string) bool {
	return networkFilesystems[strings.ToLower(strings.TrimSpace(fstype))]
}

// AppVersionVerdict is the outcome of the one-way-migration version gate (F108).
type AppVersionVerdict struct {
	// Warning is the human explanation, empty when the versions are equal or
	// either side is unreadable.
	Warning string
	// Blocking is true only for a DOWNGRADE into an app that cannot migrate
	// back down.
	Blocking bool
}

// AppVersionCompatibility judges restoring a backup of backupVersion into an
// image running targetVersion, for an app whose migrations are one-way (F108).
//
// Fail-open by design. An unreadable version on EITHER side yields no verdict at
// all: images that publish no version label are common, and a gate that guessed
// would block valid restores far more often than it caught a real downgrade.
//
// Deliberately NOT folded into RestoreCompatibility: that gate reads
// man.Databases and only ever fires for a database CONTAINER. An application
// with an embedded SQLite database has no Databases entry, so it reached the
// restore path with no version check whatsoever — the gap this closes.
func AppVersionCompatibility(p *AppProfile, backupVersion, targetVersion string) AppVersionVerdict {
	if p == nil || (p.OneWayMigration == "" && p.NoConfigMigration == "" && p.VersionLock == "") {
		return AppVersionVerdict{}
	}
	cmp, ok := compareVersions(backupVersion, targetVersion)
	if !ok || cmp == 0 {
		return AppVersionVerdict{}
	}
	// F164: neither direction has been shown to be safe, so neither is allowed.
	// Checked first, because it is the strictest verdict and an application that
	// declares it should not be softened by also declaring a weaker one.
	if p.VersionLock != "" {
		return AppVersionVerdict{
			Blocking: true,
			Warning: "This backup was taken from " + p.Name + " " + strings.TrimSpace(backupVersion) +
				", and the target runs " + strings.TrimSpace(targetVersion) + ". " + p.VersionLock +
				" — so DockBack will not restore across the difference in either direction. Pin the target to " +
				p.Name + " " + strings.TrimSpace(backupVersion) + " and restore again. " +
				"Restoring by the recorded image digest, which is the normal path, does this for you.",
		}
	}
	// F129: an app that migrates NOTHING is a risk in BOTH directions — a newer
	// image may have renamed keys the config still uses, an older one may not
	// understand keys it has since gained — and neither fails loudly. So it warns
	// either way and blocks nothing: the config is not wrong, it is just written
	// for a different version, and only the operator can judge that.
	if p.OneWayMigration == "" {
		return AppVersionVerdict{
			Warning: "This backup's configuration was written for " + p.Name + " " + strings.TrimSpace(backupVersion) +
				", but the target runs " + strings.TrimSpace(targetVersion) + ". " + p.NoConfigMigration +
				", so anything renamed or restructured between those versions will simply stop working — usually a widget or panel that quietly renders nothing rather than an error. " +
				"Check that version's release notes, or pin the target to " + strings.TrimSpace(backupVersion) + " for a faithful restore.",
		}
	}
	if cmp > 0 {
		return AppVersionVerdict{
			Blocking: true,
			Warning: "This backup was taken from " + p.Name + " " + strings.TrimSpace(backupVersion) +
				", but the target runs " + strings.TrimSpace(targetVersion) + ". " + p.OneWayMigration +
				", so an older version cannot open this database — it fails at start, after the data has been written. Restore into " +
				p.Name + " " + strings.TrimSpace(backupVersion) + " or newer, or upgrade the target first.",
		}
	}
	return AppVersionVerdict{
		Warning: "This backup was taken from " + p.Name + " " + strings.TrimSpace(backupVersion) +
			" and is being restored into " + strings.TrimSpace(targetVersion) +
			". " + p.Name + " will migrate the database forward on start, which is normal but ONE-WAY: once it has run you cannot go back to " +
			strings.TrimSpace(backupVersion) + " without restoring this backup again.",
	}
}

// compareVersions compares two dotted numeric version strings, returning -1, 0
// or 1, and ok=false when either side has no readable leading number.
//
// parseMajor is not enough here: 2.36.0 and 2.26.0 share a major, and the whole
// point of the gate is telling those apart. A leading "v" and any trailing
// pre-release/build suffix are tolerated, and a shorter version is compared as
// if zero-padded, so 2.36 == 2.36.0.
func compareVersions(a, b string) (int, bool) {
	av, aok := versionParts(a)
	bv, bok := versionParts(b)
	if !aok || !bok {
		return 0, false
	}
	n := len(av)
	if len(bv) > n {
		n = len(bv)
	}
	for i := 0; i < n; i++ {
		x, y := 0, 0
		if i < len(av) {
			x = av[i]
		}
		if i < len(bv) {
			y = bv[i]
		}
		if x != y {
			if x > y {
				return 1, true
			}
			return -1, true
		}
	}
	return 0, true
}

// versionParts splits a version string into its numeric components. Returns
// ok=false when the string has no leading numeric component at all (e.g.
// "latest", "nightly", ""), which is the signal for the caller to stay silent.
func versionParts(v string) ([]int, bool) {
	v = strings.TrimSpace(strings.ToLower(v))
	v = strings.TrimPrefix(v, "v")
	if v == "" {
		return nil, false
	}
	// Stop at the first separator that starts a pre-release/build suffix, so
	// "2.36.0-beta.1" and "2.36.0+build5" both compare as 2.36.0.
	if i := strings.IndexAny(v, "-+_ "); i >= 0 {
		v = v[:i]
	}
	var out []int
	for _, part := range strings.Split(v, ".") {
		// Tolerate a trailing non-numeric tail on a component ("36rc") by taking
		// its leading digits; a component with no digits at all ends the version.
		end := 0
		for end < len(part) && part[end] >= '0' && part[end] <= '9' {
			end++
		}
		if end == 0 {
			break
		}
		n, err := strconv.Atoi(part[:end])
		if err != nil {
			break
		}
		out = append(out, n)
		if end != len(part) {
			break
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// AppRestorePreconditions describes what a target host must provide for this
// backup's application to work there, for display BEFORE a restore (F110).
//
// Everything here is a statement about the deployment, not the archive: the
// backup can be perfect and the restore still produce a broken app because the
// new host mounts the data somewhere else. Empty for an image with no profile,
// so the ordinary restore dialog is unchanged.
type AppRestorePreconditions struct {
	App string `json:"app"`
	// DataPaths are the container destinations the target must reproduce,
	// captured or not — a skipped media bind matters most, because that is
	// exactly the mount a new host is most likely to place differently.
	DataPaths []string `json:"data_paths,omitempty"`
	// Notes are the human-readable preconditions, one per line.
	Notes []string `json:"notes,omitempty"`
	// Blocking is true when at least one precondition, left unmet, makes the
	// application UNREACHABLE rather than merely imperfect (F114). The dialog
	// styles those differently, because "you may want to check this" and "the
	// site will not load" deserve different amounts of the operator's attention.
	Blocking bool `json:"blocking,omitempty"`
	// AddressPrompt, when set, is the label for the optional new-address field
	// the dialog offers for this app. Empty for an app that records its address
	// nowhere, so the field is not shown at all.
	AddressPrompt string `json:"address_prompt,omitempty"`
	// UpstreamPrompt, when set, is the label for the optional field naming a new
	// address for a service this application DEPENDS ON (F160). Distinct from
	// AddressPrompt above, which is where the application itself is reached — the
	// two break at opposite moments and an operator needs to know which one they
	// are answering.
	UpstreamPrompt string `json:"upstream_prompt,omitempty"`
}

// AppPreconditionsFor builds the pre-restore preconditions for a manifest.
// Returns nil when the image has no profile or the profile has nothing to say,
// so callers can treat nil as "nothing to show".
func AppPreconditionsFor(man *Manifest) *AppRestorePreconditions {
	if man == nil {
		return nil
	}
	p := ProfileFor(man.Image)
	if p == nil {
		return nil
	}
	out := &AppRestorePreconditions{App: p.Name}

	if p.PathEmbedding != "" {
		// Every destination this container mounted — captured volumes AND
		// skipped ones. The skipped media bind is deliberately included: it is
		// not in the archive precisely because it is large, which is also why a
		// new host is most likely to mount it somewhere else.
		seen := map[string]bool{}
		add := func(d string) {
			if d != "" && !seen[d] && !p.LocalOnlyPath(d) {
				seen[d] = true
				out.DataPaths = append(out.DataPaths, d)
			}
		}
		for _, v := range man.Volumes {
			add(v.Destination)
		}
		for _, sk := range man.SkippedMounts {
			add(sk.Destination)
		}
		if len(out.DataPaths) > 0 {
			out.Notes = append(out.Notes, p.PathEmbedding+
				" — mount them at the paths listed below (the host path may differ). If they must differ, re-point each library folder in "+
				p.Name+" after the restore, or the items will show as missing.")
		}
	}

	// F146: said first of all, because it decides the SHAPE of the restore —
	// whether this dialog is even the right one to be in.
	if p.StackAtomic != nil {
		out.Notes = append(out.Notes, p.StackAtomic.Why+" — "+p.StackAtomic.Symptom+
			". Restore the whole stack from one app-consistent snapshot; a partial one is refused.")
	}

	// F141: said first, and said even when the archive is complete. The operator
	// is about to choose mount options and a target, and "these two are one
	// thing" is the fact that makes the rest of the dialog read correctly.
	if p.AtomicVolumes != nil {
		out.Notes = append(out.Notes, p.AtomicVolumes.Why+" — "+p.AtomicVolumes.Symptom+
			". DockBack refuses a restore that holds only part of the set rather than producing that outcome.")
	}

	// Said before the local-disk note, because it explains WHY that path matters:
	// there is a database in there that nothing else in DockBack will announce.
	if p.EmbeddedDBWarning != "" {
		out.Notes = append(out.Notes, p.EmbeddedDBWarning)
	}

	// F135: which sibling services need nothing, so a restore that brings them
	// back empty is understood as correct rather than as a gap.
	if p.DerivedServices != "" {
		out.Notes = append(out.Notes, p.DerivedServices)
	}

	// F123: what a move means for an app that manages other machines. Said at
	// restore time because that is when the operator is choosing the target, and
	// reachability to the managed hosts is a property of that choice.
	if p.ControlPlane != "" {
		out.Notes = append(out.Notes, p.ControlPlane)
	}

	for _, l := range p.LocalOnly {
		out.Notes = append(out.Notes, l+" must be on LOCAL disk on the target — an embedded database on CIFS/NFS loses the file locking it depends on and corrupts silently.")
	}

	// Stated as a standing property of the app rather than only when the version
	// gate fires, because the operator needs it BEFORE choosing a target image —
	// by the time the block appears they have already picked one. The gate
	// (AppVersionCompatibility) still refuses the specific unsafe pairing.
	if p.OneWayMigration != "" {
		out.Notes = append(out.Notes, p.OneWayMigration+
			" — the target image must be the same version as this backup or newer. Starting a newer version migrates the database forward and you cannot go back without restoring again.")
	}
	// F158: where the on-disk format's compatibility edge actually is. Said for
	// the apps that have no gate to fire — a format stable across a whole major
	// version has nothing to block, and "nothing to block" is not the same as
	// "nothing to know".
	if p.StorageCompat != "" {
		out.Notes = append(out.Notes, p.StorageCompat)
	}
	// F129: stated up front because the fix is to PIN the image, which has to
	// happen before the restore rather than after someone notices half the
	// dashboard is blank.
	if p.NoConfigMigration != "" {
		out.Notes = append(out.Notes, p.NoConfigMigration+
			" — pin the target image to the same version as this backup. A different version can silently stop parts of the configuration working, with no error to tell you which.")
	}

	// Address bindings (F114). Said BEFORE the restore, because the blocking ones
	// decide whether the operator can reach the app at all afterwards — and
	// because the opposite mistake is just as common: running a rewrite "to be
	// safe" on a move that did not change the address.
	if blocking := p.BlockingAddressSymptoms(); len(blocking) > 0 {
		out.Blocking = true
		out.Notes = append(out.Notes, "If you are moving "+p.Name+" to a DIFFERENT address, enter it below — otherwise "+
			strings.Join(blocking, ", and ")+". Keeping the same address (re-pointing DNS or your reverse proxy at the new host) needs nothing here.")
	}
	for _, bnd := range p.Address {
		if bnd.Blocking {
			continue // already covered by the combined sentence above
		}
		switch bnd.Kind {
		case BindContentRewrite:
			out.Notes = append(out.Notes, "A deliberate address change also needs a one-off content rewrite, or "+bnd.Symptom+
				". DockBack prints the exact command after the restore rather than running it — it cannot be undone.")
		case BindManual:
			out.Notes = append(out.Notes, "After a deliberate address change: "+bnd.Note+" Otherwise "+bnd.Symptom+".")
		case BindExternal:
			// Said whether or not an address change is planned, because this is
			// the one nothing here can check afterwards — and the symptom points
			// at the wrong culprit.
			out.Notes = append(out.Notes, bnd.Note+" DockBack cannot see or change this — it lives in another system. If it is missed, "+bnd.Symptom+".")
		}
	}

	if p.HasAddressBindings() {
		out.AddressPrompt = "New address for " + p.Name + " (leave blank if it is not changing)"
	}
	// F160: the other direction — the address of something this application
	// reaches out to. Said whether or not a move is planned, because the case it
	// covers is one where THIS application did not move at all.
	if p.Upstream != nil {
		out.Notes = append(out.Notes, p.Upstream.Why+
			". Moving "+p.Name+" therefore needs nothing here — but if "+p.Upstream.Service+
			" is what moved, give its new address below, or "+p.Upstream.Symptom+".")
		out.UpstreamPrompt = UpstreamPrompt(man.Image)
	}

	if len(out.Notes) == 0 {
		return nil
	}
	return out
}

// BlockingAddressSymptoms lists what the operator will SEE if each blocking
// binding is left stale — the raw symptoms, for composing into one sentence.
func (p *AppProfile) BlockingAddressSymptoms() []string {
	if p == nil {
		return nil
	}
	var out []string
	for _, b := range p.Address {
		if b.Blocking && b.Symptom != "" {
			out = append(out, b.Symptom)
		}
	}
	return out
}

// criticalTables returns the tables whose emptiness means a factory-fresh
// application (F142). Nil-safe: the overwhelming majority of images have no
// profile at all.
func (p *AppProfile) criticalTables() []CriticalTable {
	if p == nil {
		return nil
	}
	return p.CriticalTables
}

// HasAddressBindings reports whether this app records its address anywhere, so
// the restore dialog knows whether to offer the address field at all.
func (p *AppProfile) HasAddressBindings() bool { return p != nil && len(p.Address) > 0 }

// AddressCommandSteps renders the bindings DockBack does NOT run as copy-ready
// `docker exec` lines (F113/F114), with the current address filled in.
//
// newAddr may be empty, in which case an obvious placeholder is used — the
// operator is being shown what a future address change would take. A missing
// CURRENT address renders nothing at all: a half-filled command invites running
// it with a wrong "from" value, which rewrites nothing while looking like it
// worked.
func (p *AppProfile) AddressCommandSteps(container, currentAddr, newAddr string) []string {
	if p == nil || container == "" {
		return nil
	}
	currentAddr = strings.TrimSpace(currentAddr)
	if currentAddr == "" {
		return nil
	}
	if newAddr = strings.TrimSpace(newAddr); newAddr == "" {
		newAddr = "https://NEW-ADDRESS-HERE"
	}
	var out []string
	for _, b := range p.Address {
		if b.Kind != BindContentRewrite {
			continue
		}
		for _, c := range b.Commands {
			c = strings.ReplaceAll(c, "{{old}}", currentAddr)
			c = strings.ReplaceAll(c, "{{new}}", newAddr)
			out = append(out, "docker exec "+container+" "+c)
		}
	}
	return out
}

// currentAddressEnvKey returns the env key holding this app's single current
// address, used as the "from" value in rendered guidance. Empty when the app has
// no such key (a list-valued key is a set of accepted hosts, not one address).
func (p *AppProfile) currentAddressEnvKey() string {
	if p == nil {
		return ""
	}
	for _, b := range p.Address {
		if b.Kind == BindEnv && !b.List && len(b.Keys) > 0 {
			return b.Keys[0]
		}
	}
	return ""
}

// StorageCompatFor returns an image's on-disk format compatibility statement,
// or "" for the images that have none (F158).
//
// Exported so the restore dialog's image-drift note can carry it: that is the
// one moment an operator is looking at two different versions of the same
// application and deciding whether the difference matters.
func StorageCompatFor(image string) string {
	p := ProfileFor(image)
	if p == nil {
		return ""
	}
	return p.StorageCompat
}
