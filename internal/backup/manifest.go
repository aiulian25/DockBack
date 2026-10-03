// Package backup implements DockBack's backup, always-on verification, and
// restore engine (PLAN §4, §9). A backup captures the *irreplaceable state* —
// compose/config, database dumps, and volume data — encrypted and verified.
// Container images are NOT stored by default; the manifest records the image
// digest so restore re-pulls the identical image (PLAN §0.3).
package backup

import "time"

// ManifestVersion is bumped on incompatible format changes.
const ManifestVersion = 1

// Manifest is the self-describing contract written both inside the archive and
// beside it in storage, so a backup is restorable even without DockBack
// (PLAN §9.3). It is the single source of truth a restore is driven from.
type Manifest struct {
	Version   int      `json:"version"`
	BackupID  string   `json:"backup_id"`
	CreatedAt string   `json:"created_at"`
	NodeID    string   `json:"node_id"`
	NodeName  string   `json:"node_name"`
	Stack     string   `json:"stack,omitempty"`
	Service   string   `json:"service,omitempty"`    // compose service name
	DependsOn []string `json:"depends_on,omitempty"` // compose depends_on (for stack restore order)

	// StackWorkingDir + ComposeFile record the container's ON-HOST compose project
	// layout (from com.docker.compose.project.working_dir / .config_files), so a
	// disaster-recovery restore can rebuild the organized <base>/<stack>/ directory
	// and drop the reconstructed compose file back where it lived — not just
	// recreate the container. Empty for containers started without compose.
	StackWorkingDir string `json:"stack_working_dir,omitempty"`
	ComposeFile     string `json:"compose_file,omitempty"`

	// ConsistencyGroup ties every per-service backup captured in one
	// app-consistent stack snapshot (F33) together: they share this id and were
	// all quiesced/dumped inside a single pause window at ConsistencyAt (unix
	// seconds). Empty for ordinary per-container backups.
	ConsistencyGroup string `json:"consistency_group,omitempty"`
	ConsistencyAt    int64  `json:"consistency_at,omitempty"`

	TargetName  string `json:"target_name"`
	ContainerID string `json:"container_id"`

	// Image identity for restore-by-digest (PLAN §0.3).
	Image       string `json:"image"`
	ImageDigest string `json:"image_digest,omitempty"`

	// ImageConfig is what the backed-up IMAGE declared — distinct from the
	// container's resolved values in inspect.json. Recorded so a restore can diff
	// it against the image that will ACTUALLY run and report what the newer
	// version now expects (F95).
	//
	// ContainerEnvKeys are the KEYS the container itself set, so a new image
	// default the container already provides is not reported as a new
	// requirement. KEYS ONLY — a container environment routinely holds passwords
	// and API tokens, and the manifest sidecar is readable by default, so values
	// are dropped at the boundary rather than filtered later.
	ImageConfig      *ImageConfig `json:"image_config,omitempty"`
	ContainerEnvKeys []string     `json:"container_env_keys,omitempty"`

	// Format describes how to decrypt/decompress without DockBack (PLAN §9.3).
	Format Format `json:"format"`

	// Incremental volume backups (F61). When Incremental is true this backup's
	// volume payload is a CHANGED-FILES-ONLY delta (`volumes-delta.tar`) against
	// Parent, not a self-contained `volumes.tar`; a restore must walk Parent up to
	// the full baseline and apply the full + each delta in order, then remove the
	// paths in Deleted. All fields are omitempty so a container with the feature OFF
	// produces a byte-identical manifest to before this feature existed.
	//
	//   Parent             — backup id of the immediate chain parent ("" = a full).
	//   ParentCipherSHA256 — the parent's recorded CipherSHA256, pinning the chain
	//                        cryptographically so a substituted/corrupt parent fails.
	//   Incremental        — true = volumes-delta.tar (delta); false = volumes.tar (full).
	//   ChainDepth         — 0 for the full baseline; N deltas since the last full.
	//   Deleted            — volume-relative paths that vanished since Parent (removed
	//                        on restore after the delta is applied).
	//   VolIndex           — archive member holding the COMPLETE post-backup file
	//                        index (volumes-index.json.zst), diffed by the next delta.
	Parent             string   `json:"parent,omitempty"`
	ParentCipherSHA256 string   `json:"parent_cipher_sha256,omitempty"`
	Incremental        bool     `json:"incremental,omitempty"`
	ChainDepth         int      `json:"chain_depth,omitempty"`
	Deleted            []string `json:"deleted,omitempty"`
	VolIndex           string   `json:"vol_index,omitempty"`
	// Suspect (F69 ransomware tripwire): the human reason this delta touched an
	// abnormal share of the volume ("" = clean). The backup still succeeds — the
	// flag drives the critical alert + retention hold, never a failure.
	Suspect string `json:"suspect,omitempty"`

	// Config-drift fingerprints (F73): cheap hashes of the captured container
	// config so a page load can answer "has the live config changed since this
	// backup?" without opening the encrypted archive. ConfigFP covers the full
	// comparison tuple (image/env/mounts/ports/restart/command); ConfigFPLite
	// covers only the fields the fleet inventory cache carries (image + mounts),
	// for the runbook's zero-probe check. Empty on legacy backups (never drift).
	ConfigFP     string `json:"config_fp,omitempty"`
	ConfigFPLite string `json:"config_fp_lite,omitempty"`

	// Components captured in this backup.
	Volumes []VolumeRef `json:"volumes"`
	// MountedVolumes is every NAMED volume the container mounts — captured or not
	// — recorded for its IDENTITY alone: name, driver, driver options, labels
	// (F226). It is not a claim that anything in them was backed up; `Volumes`
	// above remains the captured set, and every reader of "what is in this
	// archive" keeps reading that one.
	//
	// It exists because a restore has to CREATE a volume the container mounts
	// even when its contents came from somewhere else — a database captured by
	// its dump, a folder captured once via another container, a regenerable cache
	// left out on purpose, anything the operator unticked. Without this the
	// restore left those to Docker, which auto-creates them unlabelled, and a
	// volume with no com.docker.compose.* labels is one `docker compose up`
	// refuses to adopt afterwards.
	//
	// Empty on backups taken before this existed; the restore falls back to
	// `Volumes` for those, which is what it always did.
	MountedVolumes []VolumeRef `json:"mounted_volumes,omitempty"`
	// MountedBinds is every HOST BIND the container mounts — captured or not —
	// recorded for what each one IS: the host source, the container path it feeds,
	// and the kind, ownership and mode of its root at capture time.
	//
	// The bind counterpart of MountedVolumes above, and it exists for exactly the
	// same reason. "Not captured" is ordinary and says nothing about whether the
	// path has to exist on the target: a bundled database's data directory is
	// superseded by its dump and deliberately not archived, a large media bind is
	// left out by the size default, a read-only reference set is skipped, the
	// operator unticked something. In every one of those cases the container still
	// binds that path, so a restore onto a fresh machine still has to create it —
	// as the right kind of thing, owned by the right ids.
	//
	// Left to Docker, an absent bind source becomes a root-owned DIRECTORY, even
	// where the container expects a file. That is the failure this record exists
	// to prevent, and it is why the set is wider than `Volumes`: the paths most
	// likely to be missing on a new host are precisely the ones nothing captured.
	//
	// System paths that exist on every host and are never ours to create
	// (/etc/localtime, the Docker socket, /proc…) are excluded — see
	// backupableBind.
	MountedBinds []VolumeRef `json:"mounted_binds,omitempty"`
	// Requires records what the HOST must provide for this container to start —
	// devices, GPU reservations, sysctls, extra capabilities, a non-default log
	// driver, privileged mode (F94).
	//
	// The cross-host restore path is first class, but nothing checked whether the
	// target could honor any of this: a hardware-transcoding container restored
	// onto a host with no GPU was created and then failed at start with a raw
	// Docker error. Recorded so the pre-restore dialog can say so first.
	//
	// Omitted entirely for an ordinary container, so its manifest is unchanged.
	Requires *HostRequirements `json:"requires,omitempty"`

	// Networks records each user-defined network this container was attached to —
	// its full definition (driver, IPAM, internal/attachable flags, options,
	// labels) and this container's endpoint on it (F89).
	//
	// Without it a disaster-recovery restore recreated every missing network as a
	// bare bridge: the subnet, the container's static address, and the `internal`
	// isolation flag were all silently dropped. Absent on pre-F89 backups, which
	// restore exactly as they did before.
	Networks      []NetworkRef `json:"networks,omitempty"`
	VolumesSHA256 string       `json:"volumes_sha256,omitempty"` // checksum of the plaintext volume payload (PLAN §4.12)
	Databases     []DBDump     `json:"databases"`

	// DBFallback records that this container was DETECTED as a database engine but
	// its native dump tools were missing, so its data was captured as raw FILES
	// instead (F103).
	//
	// This matters because the resulting manifest has an empty Databases array and
	// is otherwise indistinguishable from an ordinary application backup — while
	// what actually happened is the exact thing the dump feature exists to prevent:
	// a live database's files copied without quiescing, which can be torn. Without
	// this field such a backup could be graded A with no reasons.
	//
	// Empty for every other backup, so a manifest that never hit this path is
	// byte-identical to before.
	DBFallback string `json:"db_fallback,omitempty"`
	HasConfig  bool   `json:"has_config"`  // config/inspect.json present
	HasCompose bool   `json:"has_compose"` // config/docker-compose.yml (reconstructed) present
	// HasOriginalCompose is set when the GENUINE host compose file(s) were captured
	// over SSH into config/original-compose/ (F57) — the hand-tuned, commented
	// original, preferred over the reconstruction when rebuilding by hand. Only ever
	// set for SSH-transport nodes; the socket-proxy transports can't read host files.
	HasOriginalCompose bool `json:"has_original_compose,omitempty"`

	// SQLiteDumps records SQLite database files found under the captured volumes
	// that were snapshotted CONSISTENTLY (sqlite3 online backup) into a sidecar
	// `.dbk` instead of trusting the live file, so a mid-write WAL/journal can't
	// yield a torn "backup" (F22). On restore the consistent copy is laid back over
	// the raw file. Empty when no SQLite files were found or the sidecar lacked
	// sqlite3 (then the raw file — plus its WAL — is still captured as a fallback).
	SQLiteDumps []SQLiteRef `json:"sqlite_dumps,omitempty"`

	// SQLiteFallback records that SQLite databases were FOUND under the captured
	// mounts but none could be snapshotted consistently, so what shipped is a raw
	// copy of live files (F154).
	//
	// This is the SQLite twin of DBFallback above, and it had the same blind
	// spot: with no snapshot recorded, the manifest is indistinguishable from one
	// for an application that has no database at all. The commonest cause is the
	// most invisible — the volume sidecar image has no `sqlite3` binary, which is
	// true of the shipped default — so a whole fleet of embedded-database apps
	// can be backed up for months with every archive holding a file that was
	// copied mid-write.
	//
	// It matters most exactly where it is least visible: a database with a large
	// write-ahead log is being actively written, and a raw copy of the file plus
	// a separately-timed WAL is the definition of a torn copy.
	//
	// Empty for every backup where the snapshot worked, or where there was no
	// SQLite to snapshot.
	SQLiteFallback string `json:"sqlite_fallback,omitempty"`
	// SQLiteFallbackCount is how many databases that covers, so the finding can
	// say how much is at stake rather than that something is.
	SQLiteFallbackCount int `json:"sqlite_fallback_count,omitempty"`

	// CorruptDatabases records SQLite files that FAILED `PRAGMA integrity_check`
	// at capture (F116) — damaged at the source, so the backup preserves the
	// damage rather than repairing it.
	//
	// By default such a backup is refused outright. This field exists for the
	// case where the operator deliberately turned that gate off: the archive then
	// still declares the damage on its own face instead of being indistinguishable
	// from a clean one. Empty for every healthy backup.
	CorruptDatabases []CorruptDB `json:"corrupt_databases,omitempty"`

	// ExcludedRegenerable records directories the operator deliberately left out
	// because the application rebuilds them (F132) — Jellyfin's trickplay
	// previews being the motivating case, at 11 GB of a 12 GB archive.
	//
	// Recorded, but NOT counted as a partial capture. That distinction is the
	// point: a backup missing thumbnails the server will redraw is not the same
	// as one missing the photographs themselves, and grading them alike would
	// blunt the warning that catches the second. So the archive says what was
	// dropped and why, and nothing is graded down for a choice made on purpose.
	ExcludedRegenerable []ExcludedPath `json:"excluded_regenerable,omitempty"`

	// AtomicVolumes records container paths this archive was REQUIRED to contain
	// together, because the application's state is split across them and half of
	// it restores into a broken or factory-fresh deployment (F141).
	//
	// Recorded rather than only derived, so the archive states its own contract:
	// a restore years from now judges what THIS backup was required to hold, not
	// what the registry happens to say by then.
	//
	// Only members the source container actually mounted are listed — a path
	// that was never a mount could not have been captured, and requiring it
	// would make the archive permanently unrestorable over something the backup
	// never had.
	AtomicVolumes []string `json:"atomic_volumes,omitempty"`

	// StackAtomic records that this backup is ONE MEMBER of a set of services
	// that are only meaningful together (F146), and names the rest of the set as
	// it stood when the backup was taken.
	//
	// Recorded rather than derived, for a reason specific to this case: whether
	// two containers belong to one unit is a fact about the DEPLOYMENT at capture
	// time, not about the archive. A restore years later cannot re-derive it —
	// the stack may not exist any more — so the archive has to carry it.
	//
	// Absent for the overwhelming majority of backups, which belong to no set.
	StackAtomic *StackAtomicRef `json:"stack_atomic,omitempty"`

	// StaleStaging records interrupted-write leftovers found inside the captured
	// data (F157) — directories the application stages a write in and renames
	// away on success, still sitting there long afterwards.
	//
	// A finding about the SOURCE, not the backup: the archive is a faithful copy
	// of a tree that has debris in it. Recorded so the finding survives into the
	// backup's details and its runbook, where somebody reading about a recovery
	// will see what the tree actually contains.
	StaleStaging []StaleStagingDir `json:"stale_staging,omitempty"`

	// SecretModes records the permissions of files whose exposure matters
	// (F147) — a WireGuard private key, a certificate store.
	//
	// Modes only. Never a byte of any of them, and never a hash: a hash of a
	// 44-byte key with a known format is not the protection it looks like.
	SecretModes []SecretFileMode `json:"secret_modes,omitempty"`

	// Certificates records the TLS certificates found inside the captured data
	// (F143) — what they are, when they expire, and whether their private key
	// travelled with them.
	//
	// PUBLIC HALF ONLY, and less of it than is available. The certificate's own
	// bytes are read to parse validity dates; the private key is never read, only
	// tested for existence. The DNS names a certificate covers are counted, not
	// recorded — the manifest sidecar travels to every destination in plaintext,
	// and an operator's internal hostnames are not something a backup should
	// publish in order to describe itself.
	Certificates []CertRef `json:"certificates,omitempty"`

	// PublishedPorts records the HOST ports this container published (F144), so
	// a restore onto another machine can say which of them are already taken
	// there before it creates a container that cannot bind.
	PublishedPorts []PublishedPort `json:"published_ports,omitempty"`

	// RunAsIDs records the uid/gid this container's application runs as (F117),
	// so a restore onto a host configured with different ids can align the
	// restored files instead of leaving the app unable to write its own data.
	RunAsIDs *RunAs `json:"run_as,omitempty"`

	// SkippedMounts records data mounts that were NOT captured (e.g. a large
	// media bind excluded by default), so the backup is honestly reported as
	// PARTIAL instead of silently looking complete when its only real data lives
	// in an excluded mount.
	SkippedMounts []SkippedMount `json:"skipped_mounts,omitempty"`

	// Findings are things discovered about the SOURCE during capture that the
	// operator should see: a config file that is present but inert, one host path
	// bound at two destinations, two containers sharing a mount on different
	// image versions, a healthcheck whose success condition is satisfied by the
	// failure state (PLAYBOOK §10.3).
	//
	// Distinct from SkippedMounts and SecretModes above, which describe THIS
	// BACKUP — what it does not contain, and what permissions its files carry.
	// These describe the deployment the backup was taken from, and every one of
	// them is reported and never acted on: a restore reproduces the source, and
	// several of these are defects where "helpfully" fixing them is the dangerous
	// move rather than the safe one.
	//
	// Never a value, only ever a subject and a sentence — the manifest sidecar
	// travels to every destination in plaintext, so a finding about a credential
	// names the key, not what is in it.
	Findings []Finding `json:"findings,omitempty"`

	// AppExport, when set, means this is a portable application-native export
	// (e.g. Paperless document_exporter) restored via the app's own importer
	// rather than from raw volumes/DB (PLAN §9.4 / Phase 4).
	AppExport *AppExportRef `json:"app_export,omitempty"`

	// ImageTar, when set, means a `docker save` of the container's image is
	// bundled (image.tar) for fully air-gapped restore where re-pull is
	// impossible (PLAN §0.3 / §8.4). Optional, off by default.
	ImageTar *ImageTarRef `json:"image_tar,omitempty"`

	// DockerVersion is the engine this backup was taken on (#7).
	//
	// Recorded because some restore rules are ENVIRONMENTAL: they apply because
	// of a measured difference between the two machines, not because a restore is
	// happening. R1 §Issue 7's healthcheck broke purely on an engine gap — Docker
	// 29 resolves `localhost` to ::1 first where 24 resolved it to 127.0.0.1 —
	// and R5 §2's matched-platform control confirmed the rule does not fire
	// without the gap. Without this field the gap cannot be measured, and a rule
	// applied on a guess is the "gratuitous churn" §2.1 warns about.
	//
	// Empty on a backup taken before this was recorded, which reads as unknown
	// and never as "the same".
	DockerVersion string `json:"docker_version,omitempty"`

	// SelectionBytes is how much data this backup's selection occupied on disk
	// at capture, uncompressed (#37).
	//
	// The backup side has always measured this to guard its own free space; it
	// was then thrown away. A restore needs the same number to answer the only
	// question that matters before it starts writing — will this fit where it is
	// going — and R5 §3 is what happens when nobody asks: 58 GB onto a filesystem
	// with 74 GB free, leaving ~16 GB for seventeen other containers.
	//
	// Zero on an archive taken before this was recorded, which makes the restore
	// fall back to estimating from the stored size and say that it did.
	SelectionBytes int64 `json:"selection_bytes,omitempty"`

	// Integrity & size of the encrypted archive (PLAN §3.4).
	CipherSHA256   string `json:"cipher_sha256"`
	CipherSize     int64  `json:"cipher_size"`
	ArchiveKey     string `json:"archive_key"`           // storage object key (path), not an encryption key
	WrappedKey     string `json:"wrapped_key,omitempty"` // per-backup DEK wrapped by the master key (PLAN §3.3 envelope); empty = legacy direct-master-key archive OR write-only (see below)
	KeyFingerprint string `json:"key_fingerprint"`       // which master key wrapped the DEK (PLAN §3.3)

	// Write-only envelope (F86). When set, the DEK is sealed to an X25519 PUBLIC
	// key whose private half exists only offline, and WrappedKey is empty — so the
	// running instance cannot decrypt this archive at all, even holding the master
	// key. BackupPubFP identifies which keypair, so a restore can tell "wrong key"
	// from "corrupt archive" before decrypting.
	//
	// Both empty = today's symmetric envelope; nothing about a pre-F86 backup
	// changes, and the two modes coexist in one catalog.
	WrappedKeyPub string `json:"wrapped_key_pub,omitempty"`
	BackupPubFP   string `json:"backup_pub_fp,omitempty"`

	// Verification is the always-on test-restore report, embedded so the manifest
	// is the full contract (PLAN §4.12). Populated after verification on the
	// manifest-of-record; the immutable signed sidecar (§3.5) is written earlier.
	Verification *VerificationReport `json:"verification,omitempty"`
}

// Format documents the encryption/compression so external tooling can recover.
type Format struct {
	Encryption  string `json:"encryption"`          // "AES-256-GCM (DBACKv1 chunked)"
	Compression string `json:"compression"`         // human label, e.g. "zstd-balanced" | "gzip" | "xz-max"
	Algorithm   string `json:"algorithm,omitempty"` // machine-readable: zstd|gzip|xz (empty = zstd, pre-§4.15)
	Archive     string `json:"archive"`             // "tar"
	Layout      string `json:"layout"`              // human description of tar layout
}

// SQLiteRef records a SQLite database captured as a consistent snapshot (F22).
// Source is the file's absolute path inside the container's volumes; Archive is
// the snapshot's path inside the backup archive (e.g. "sqlite/1.dbk"). On restore
// the Archive copy is written back over Source and its stale WAL/shm removed.
type SQLiteRef struct {
	Source  string `json:"source"`
	Archive string `json:"archive"`
	Bytes   int64  `json:"bytes,omitempty"`

	// Snapshot integrity contract (F109) — the SQLite counterpart of the dump
	// contract the other engines already carry (F87/F99).
	//
	// Until this existed, a consistent snapshot was recorded and then never
	// looked at again: Verify walked man.Databases and skipped man.SQLiteDumps
	// entirely, so a .dbk that rotted in storage, or restored short, passed every
	// check. For an application whose ENTIRE state is one SQLite file — which is
	// most of the self-hosted fleet — that meant the most important object in the
	// archive was the least verified.
	//
	//   SHA256 — of the .dbk as written, so a scrub proves the stored snapshot is
	//            still byte-identical to what capture produced.
	//   Tables — user tables (sqlite_% excluded) present at capture.
	//   Rows   — total rows across those tables.
	//
	// Zero means "no expectation", covering both a database that was genuinely
	// empty and one too large to scan cheaply. The restore check treats zero the
	// same either way — it compares nothing rather than asserting emptiness —
	// because an empty database has no content to come back short of.
	//
	// All omitempty, so a backup taken before this feature is byte-identical and
	// simply carries no contract to check.
	SHA256 string `json:"sha256,omitempty"`
	Tables int    `json:"tables,omitempty"`
	Rows   int64  `json:"rows,omitempty"`

	// TableRows is the per-table row count at capture (F124).
	//
	// The aggregate above hides the failure that matters most. Gotify's ~115,000
	// rows are almost entirely message history, so losing every one of its 32
	// `clients` rows — every device that receives notifications — is a 0.03%
	// shortfall, and nets out to nothing at all if another table grew meanwhile.
	// Per table, the same loss is unmissable: "clients came back with 0 of 32".
	//
	// Empty when the database was unreadable, too large to scan, or has more
	// tables than the recording cap; the aggregate check still applies then, so
	// the contract degrades rather than vanishing. Holds table NAMES (schema
	// shape) and counts — never a value from any row.
	TableRows map[string]int64 `json:"table_rows,omitempty"`

	// TableHashes is a per-table CONTENT hash of this snapshot (#18), the SQLite
	// half of the same proof. Keys are table names; values are the sqlite3
	// shell's own SHA3 content hash, which is independent of insertion order and
	// unchanged across the backup-API copy this snapshot is made with.
	TableHashes map[string]string `json:"table_hashes,omitempty"`
}

// RunAs records the uid/gid the container's application runs as, taken from the
// PUID/PGID-style environment variables the image declares (F117).
//
// Recorded so a restore can tell "these files are owned by the ids this app will
// run as" from "these files are owned by the ids the OLD machine's app ran as" —
// a distinction the numeric restore alone cannot make, and the cause of the
// commonest broken restore there is: an app that starts fine and then cannot
// write its own database.
//
// Absent for the majority of images, which declare no such variable.
type RunAs struct {
	UID int    `json:"uid"`
	GID int    `json:"gid"`
	Key string `json:"key,omitempty"` // which convention it came from, e.g. "PUID/PGID"
}

// ExcludedPath is one regenerable directory left out on purpose (F132).
type ExcludedPath struct {
	Path  string `json:"path"`
	Label string `json:"label,omitempty"`
	Cost  string `json:"cost,omitempty"` // what regenerating it will take
}

// StackAtomicRef is the set this backup is one member of (F146).
type StackAtomicRef struct {
	// Why and Symptom are carried in the archive rather than looked up, so the
	// refusal a restore prints is the one that was true when the backup was
	// taken — and still reads correctly for an image DockBack no longer knows.
	Why     string `json:"why"`
	Symptom string `json:"symptom,omitempty"`
	// SoloRestore is the extra guidance for the case that looks most reasonable
	// and is most damaging: restoring one member on its own.
	SoloRestore string `json:"solo_restore,omitempty"`
	// GroupID is the app-consistent snapshot every member shares, empty when this
	// member was captured on its own — which is itself the finding.
	GroupID string `json:"group_id,omitempty"`
	// Members is the whole set as it stood at capture, this backup included.
	Members []StackMember `json:"members"`
}

// StackMember is one service of an atomic set (F146). Identity only — the image
// reference makes the recorded set a VERSION set too, so a restore can say what
// the tested combination was.
type StackMember struct {
	Service   string `json:"service"`
	Container string `json:"container,omitempty"`
	Image     string `json:"image,omitempty"`
}

// StaleStagingDir is one interrupted-write leftover captured in a backup (F157).
type StaleStagingDir struct {
	// Path inside the container.
	Path string `json:"path"`
	// ModifiedAt is when it was last written, RFC3339. An absolute time rather
	// than an age, because an age recorded in an archive is wrong the moment the
	// archive is a day old.
	ModifiedAt string `json:"modified_at,omitempty"`
	// Bytes is what it is costing in every backup that carries it.
	Bytes int64 `json:"bytes,omitempty"`
	// What names it in the operator's terms.
	What string `json:"what,omitempty"`
}

// SecretFileMode is one audited file's permissions at a point in time (F147).
type SecretFileMode struct {
	Path string `json:"path"`
	// Mode is the permission bits as an octal string ("600", "644"), the form an
	// operator will type into chmod.
	Mode string `json:"mode"`
	// Max is the widest mode that is acceptable for this file, same notation.
	Max string `json:"max,omitempty"`
	// What names what the file is, so a finding says what is exposed.
	What string `json:"what,omitempty"`
}

// TooOpen reports whether this file's permissions are wider than its maximum.
// An unreadable or unrecorded mode is never "too open" — a check that could not
// run must not read as one that failed.
func (s SecretFileMode) TooOpen() bool {
	m, mok := parseOctalMode(s.Mode)
	x, xok := parseOctalMode(s.Max)
	return mok && xok && m&^x != 0
}

// CertRef is one TLS certificate captured inside a backup (F143).
//
// Enough to answer the two questions that matter after a restore — is it still
// here, and is it still valid — and deliberately nothing more. No subject, no
// SAN list, no serial: those name the operator's own services, and this record
// travels in plaintext to every destination the archive is copied to.
type CertRef struct {
	// Path is the certificate file inside the container.
	Path string `json:"path"`
	// Issuer is the issuing authority's common name ("R11", "ISRG Root X1"), or
	// "self-signed". Names an authority, never a subscriber.
	Issuer string `json:"issuer,omitempty"`
	// NotBefore/NotAfter are RFC3339 validity bounds.
	NotBefore string `json:"not_before,omitempty"`
	NotAfter  string `json:"not_after,omitempty"`
	// Names counts the DNS names this certificate covers, without listing them.
	// A count is enough to notice a certificate came back as a different one.
	Names int `json:"names,omitempty"`
	// HasKey records that a private key sits beside the certificate and was
	// therefore captured with it. Determined by testing that the file exists —
	// its contents are never read.
	HasKey bool `json:"has_key,omitempty"`
}

// PublishedPort is one host port a container published (F144).
type PublishedPort struct {
	HostIP   string `json:"host_ip,omitempty"`
	HostPort int    `json:"host_port"`
	Proto    string `json:"proto,omitempty"` // tcp | udp
}

// CorruptDB is one database that was damaged when the backup was taken (F116).
type CorruptDB struct {
	Path   string `json:"path"`             // absolute path inside the container's volumes
	Detail string `json:"detail,omitempty"` // sqlite's own integrity_check verdict
}

// VolumeRef records a backed-up mount (named volume or host bind).
type VolumeRef struct {
	Name        string `json:"name,omitempty"`
	Destination string `json:"destination"`
	Driver      string `json:"driver,omitempty"`
	Type        string `json:"type,omitempty"`   // "volume" | "bind"
	Source      string `json:"source,omitempty"` // host path (binds) — recreated on DR

	// What the bind's ROOT was on the source machine. Binds only; a named volume
	// is Docker's to create and its identity is Options/Labels above.
	//
	// A restore onto a machine that has none of these paths has to create them,
	// and cannot do that safely without knowing what each one IS. Docker's own
	// answer when a bind source is absent is to invent a root-owned DIRECTORY —
	// including where the container expects a FILE, which is how a mounted secret
	// becomes a directory and the application starts up broken rather than not at
	// all. Recording the kind, the ids and the mode is what lets a restore
	// reproduce the path instead of letting the daemon guess at it.
	//
	// Empty on a pre-F81 backup and whenever the probe could not read the value.
	// Empty means "not known", never a default: a consumer must decline to create
	// the path rather than pick a kind.
	Kind  string `json:"kind,omitempty"`  // dockercli.MountKindDir | MountKindFile
	Owner string `json:"owner,omitempty"` // "uid:gid" of the mount root at capture
	Mode  string `json:"mode,omitempty"`  // octal permission bits, e.g. "700"
	// ReadOnly records that the container mounted this bind read-only.
	//
	// It is what separates the mounts an application must be able to WRITE from
	// the ones it only reads, and that distinction identifies the ids it runs as:
	// whoever owns the writable data is the application, whereas a read-only
	// reference set is frequently owned by whoever put it there. Without it, a
	// read-only directory owned by the host's operator makes the answer ambiguous.
	ReadOnly bool `json:"read_only,omitempty"`

	// Archive names the member holding this mount's contents when they are NOT
	// in volumes.tar — set only for a bind whose root is a file (F81).
	//
	// Those cannot ride in the volume archive at all: it is extracted in a
	// sidecar where every bind is a live mount, and replacing a bind-mounted file
	// requires unlinking it, which returns EBUSY and fails the whole extraction.
	// So the bytes travel as their own small member and the restore writes them
	// to the host before the container exists. Same convention as SQLiteDump.Archive.
	Archive string `json:"archive,omitempty"`

	// NestedIn names the captured mount whose archive member already holds this
	// one's bytes, when this mount sits INSIDE another selected mount (#20).
	//
	// Nextcloud mounts four binds under /var/www/html. The volume tar's member
	// list was the selected destinations verbatim, so the sidecar walked the
	// parent (crossing into each child, as tar does) and then walked every child
	// again: 22 GB of data became 44 GB on the wire and in the archive.
	//
	// The child is no longer listed as its own member — the parent's walk already
	// carries it, under exactly the same member name — so this field is how the
	// manifest still describes the real mount topology to a restore and to the UI
	// after the member list stopped describing it.
	//
	// Empty means the mount stands on its own, which is the common case.
	NestedIn string `json:"nested_in,omitempty"`

	// Driver options and labels for a NAMED volume (F90). Without these, a volume
	// backed by NFS or CIFS was recreated on restore as a plain local one and the
	// restored data went to local disk instead of the NAS — silently.
	//
	// CREDENTIAL VALUES ARE NOT STORED. The manifest sidecar is readable by
	// default and travels to every destination, so a CIFS password recorded here
	// would leak on each of them. Secret-looking values are replaced with a
	// marker and listed in OptionsRedacted; the restore then refuses to
	// half-create the volume rather than silently producing a broken one.
	Options         map[string]string `json:"options,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	OptionsRedacted []string          `json:"options_redacted,omitempty"`
}

// AppExportRef records how a portable app-native export was produced and how to
// re-import it on restore.
type AppExportRef struct {
	Tool      string   `json:"tool"`       // e.g. "paperless"
	Dir       string   `json:"dir"`        // export directory inside the container
	ImportCmd []string `json:"import_cmd"` // command to import the export on restore
	Bytes     int64    `json:"bytes"`      // captured export size
	// VerifyCmd runs after a successful import and FAILS the restore on a
	// non-zero exit (F152). Recorded here rather than looked up at restore time
	// for the same reason ImportCmd is: the archive must describe how to restore
	// itself, without depending on settings that may have changed — or on a
	// DockBack instance existing at all.
	VerifyCmd []string `json:"verify_cmd,omitempty"`
	// Cleaned records that the export directory was emptied after this capture
	// (F151), so the archive states that the plaintext copy did not linger.
	Cleaned bool `json:"cleaned,omitempty"`
}

// ImageTarRef records a bundled `docker save` image tarball for air-gapped
// restore (PLAN §0.3 / §8.4).
type ImageTarRef struct {
	Ref   string `json:"ref"`   // image reference saved (repo:tag)
	Bytes int64  `json:"bytes"` // tarball size
}

// DBDump records a captured database dump.
// DBLocale is one database's character-set identity: the three properties that
// are chosen at CREATE DATABASE and cannot be altered afterwards.
//
// Collation is the one that matters most and looks like it matters least. It
// decides string sort order, so it decides text index order — a cluster
// re-initialised under a different collation builds indexes in a different
// order, and queries then miss rows that exist. It surfaces weeks later and
// reads as data corruption.
type DBLocale struct {
	Name     string `json:"name"`
	Encoding string `json:"encoding"`          // e.g. "UTF8"
	Collate  string `json:"collate,omitempty"` // e.g. "en_US.utf8"
	Ctype    string `json:"ctype,omitempty"`
}

// ClusterConfig is what a PostgreSQL CLUSTER is configured as — which a dump of
// its databases does not contain.
//
// pg_dump dumps databases; server settings live in postgresql.conf inside the
// data directory, and a restore here wipes that directory and re-initialises, so
// without this record the new cluster takes every setting from the TARGET's
// environment and defaults. Measured consequence: a source on Europe/London
// restored onto a target on Etc/UTC made 19 of 66 tables appear to differ, with
// byte-identical data — indistinguishable from corruption at a glance. Encoding
// and collation are worse: immutable after initdb, so getting them wrong is not
// fixable by editing configuration afterwards.
//
// Only NON-DEFAULT settings are recorded. Writing back a value the source never
// set is the same error in the opposite direction — a restore reproduces its
// source, it does not add to it.
type ClusterConfig struct {
	// Settings are the non-default server settings, name to value.
	Settings map[string]string `json:"settings,omitempty"`
	// Databases is the per-database encoding/collation identity.
	Databases []DBLocale `json:"databases,omitempty"`
	// Redacted names the settings whose values were WITHHELD because the name
	// says the value is a credential — primary_conninfo carries a replication
	// password, and the *_command settings are shell commands that routinely
	// embed one. The manifest sidecar travels to every destination in plaintext,
	// so those values are dropped at the boundary rather than filtered later.
	//
	// The names are kept because their presence is worth knowing and is not
	// itself a secret; none of them describe cluster semantics, so nothing a
	// restore needs is lost by withholding them.
	Redacted []string `json:"redacted,omitempty"`
}

type DBDump struct {
	Service string `json:"service"`
	Engine  string `json:"engine"`            // postgres|mysql|mongodb
	Version string `json:"version,omitempty"` // engine version string, for restore compatibility (PLAN §4.12)
	Path    string `json:"path"`              // path inside the archive
	Bytes   int64  `json:"bytes"`
	// Extensions lists installed Postgres extensions as "name version" (e.g.
	// "vectors 0.2.0"), so a restore can flag an extension/vector mismatch on the
	// target — the exact class of break behind the Immich pgvecto.rs -> VectorChord
	// upgrade failure. Empty for non-Postgres engines or when unavailable.
	Extensions []string `json:"extensions,omitempty"`

	// Dump integrity contract (F87). Recorded AT CAPTURE by streaming the dump
	// through the same tally the restore path uses, so what the dump declared when
	// it was written is a fact on record rather than something re-derived (and
	// possibly re-derived differently) years later.
	//
	// This is the capture-side half of a real incident: a verified-good dump that
	// restored as 27 of 72 primary keys and reported success. Restore-side
	// verification (F82) catches a bad import; these fields additionally prove the
	// STORED dump was complete when written, and let a scrub catch it rotting.
	//
	// Absent on pre-F87 backups, which fall back to the streamed tally.
	DumpPrimaryKeys int    `json:"dump_primary_keys,omitempty"`
	DumpForeignKeys int    `json:"dump_foreign_keys,omitempty"`
	DumpComplete    bool   `json:"dump_complete,omitempty"`
	DumpSHA256      string `json:"dump_sha256,omitempty"`

	// The same contract for the other three engines (F99), each using the cheap
	// invariant its own dump format actually offers. MySQL's comes from the dump
	// text as it streams; MongoDB's and Redis's are queried from the live source
	// at capture, because their dumps are binary and declare nothing scannable.
	//
	// DumpRows counts INSERT STATEMENTS, not rows — mysqldump writes extended
	// inserts. It is recorded for diagnosis and never used as a pass/fail gate.
	DumpTables int `json:"dump_tables,omitempty"`

	// DumpScope is the privilege level this dump was taken with (#13).
	//
	// "server" when a privileged identity dumped the whole server;
	// "schema:<names>" when an application identity dumped only what it owns.
	// The difference is not cosmetic: R2 §Issue 13's dump was taken as the app
	// user after a declared root password turned out never to have applied, and
	// such a dump covers its schemas completely while containing none of the
	// server's accounts or grants — "a real gap on a full-server restore".
	//
	// Empty on a backup taken before this was recorded, and whenever the probe
	// could not run; absent means unknown, never "server".
	DumpScope string `json:"dump_scope,omitempty"`

	// TableHashes is a per-table CONTENT hash taken at capture (#18).
	//
	// TableRows above proves a table came back with the same number of rows. It
	// says nothing about whether they are the same rows: a restore that returns
	// every row with a changed value passes it exactly. This closes that, and it
	// is computed the one way that does not manufacture its own failures.
	//
	// R4 §Issue 31 measured what the obvious implementation costs. Hashing
	// `t::text` reported SEVEN false table mismatches out of eight on a
	// byte-perfect restore — `documents_document` "differing" on 169 of 203 rows
	// — because rendering a timestamptz as text follows the session TimeZone, and
	// the clone had one the source did not. So every temporal column is hashed as
	// `extract(epoch from …)` and the aggregate is ordered under `COLLATE "C"`.
	// See dbhash.go.
	//
	// Keys are `<database>.<schema>.<table>` for a server engine and the table
	// name for SQLite. Holds table NAMES and hashes — never a value from any row,
	// and a hash of a table is not reversible into one.
	//
	// Empty for an engine with no table concept (Mongo, Redis), for a schema
	// larger than the recording cap, and whenever the pass could not run — the
	// row-count contract still applies, so this degrades rather than vanishing.
	TableHashes     map[string]string `json:"table_hashes,omitempty"`
	DumpRows        int64             `json:"dump_rows,omitempty"`
	DumpCollections int               `json:"dump_collections,omitempty"`
	DumpKeys        int64             `json:"dump_keys,omitempty"`
	// DumpTableRows is the row count of the APPLICATION'S OWN key tables at
	// capture (F168), for an embedded database whose profile names them.
	//
	// DumpTables above counts how many tables the dump declares, which catches a
	// dump that was cut short. It says nothing about an import that created every
	// table and filled almost none of them — and for an application whose value
	// IS the rows, that is the failure worth catching. Absent for every other
	// dump, which has nothing declared to count.
	DumpTableRows map[string]int64 `json:"dump_table_rows,omitempty"`

	// ClusterConfig records what the SERVER was configured as, for Postgres
	// (#39). Absent for other engines, for a capture that could not run, and on
	// every backup taken before this existed — all of which a restore reads the
	// same way: nothing to replay, so the target's defaults stand, exactly as
	// they did before.
	ClusterConfig *ClusterConfig `json:"cluster_config,omitempty"`

	// DumpVolatileKeys is how many of DumpKeys carried a TTL at capture (F150).
	//
	// Redis deletes already-expired keys as it LOADS an RDB, so a snapshot
	// restored later legitimately holds fewer keys than were captured. This is
	// what lets a restore tell that apart from a snapshot that did not load:
	// anything below DumpKeys-DumpVolatileKeys is real loss, anything between is
	// a time-to-live doing its job. Absent on pre-F150 backups, which are judged
	// by the weaker rule that only an entirely empty Redis is a failure.
	DumpVolatileKeys int64 `json:"dump_volatile_keys,omitempty"`

	// Databases, when non-empty, records that this is a PER-DATABASE subset dump
	// (F8) of just these databases rather than the whole cluster — so restore
	// re-imports only them, into the RUNNING engine, without wiping the data
	// directory (which would destroy the sibling databases). Empty = full cluster.
	Databases []string `json:"databases,omitempty"`
}

// hasDumpContract reports whether capture recorded a completeness expectation
// for this dump (F87 for Postgres, F99 for the rest). Backups written before
// those features have none, and fall back to the tally taken from the stream.
func (d DBDump) hasDumpContract() bool {
	return d.DumpPrimaryKeys > 0 || d.DumpForeignKeys > 0 ||
		d.DumpTables > 0 || d.DumpCollections > 0 || d.DumpKeys > 0
}

// HostRequirements is what the host must provide for a container to start (F94).
type HostRequirements struct {
	// Devices are host device paths the container is given (/dev/dri/renderD128).
	Devices []string `json:"devices,omitempty"`
	// GPUs counts DeviceRequests reservations (`--gpus`), which need the NVIDIA
	// container runtime on the target, not merely a card.
	GPUs int `json:"gpus,omitempty"`
	// Sysctls, CapAdd and Privileged are RECORDED but not probed: whether a host
	// permits them depends on its kernel and daemon policy, and the only reliable
	// test is trying. They are reported so the operator can judge.
	Sysctls    map[string]string `json:"sysctls,omitempty"`
	CapAdd     []string          `json:"cap_add,omitempty"`
	Privileged bool              `json:"privileged,omitempty"`
	// LogDriver is compared against the target daemon's default.
	LogDriver string `json:"log_driver,omitempty"`

	// DockerSocket records that the container is given the host's Docker socket,
	// and in which mode — "rw" or "ro" (F128).
	//
	// It belongs here for the same reason Privileged does: it is not data, it is
	// AUTHORITY. A read-write socket is full control of the host's Docker daemon,
	// which is root-equivalent, and moving such a container to another machine
	// hands that authority to the new host — a decision worth stating out loud
	// rather than replaying silently.
	//
	// Recorded so a restore can also PROVE it reproduced the mount as configured
	// and never widened it. Empty for the overwhelming majority of containers.
	DockerSocket string `json:"docker_socket,omitempty"`
}

// Empty reports a container that asks nothing special of its host, so the field
// can be omitted from the manifest entirely.
func (h HostRequirements) Empty() bool {
	return len(h.Devices) == 0 && h.GPUs == 0 && len(h.Sysctls) == 0 &&
		len(h.CapAdd) == 0 && !h.Privileged && h.LogDriver == "" && h.DockerSocket == ""
}

// NetworkPoolRef is one IPAM pool of a network.
type NetworkPoolRef struct {
	Subnet  string `json:"subnet,omitempty"`
	Gateway string `json:"gateway,omitempty"`
	IPRange string `json:"ip_range,omitempty"`
}

// NetworkRef is one attached network's definition plus this container's endpoint.
// Mirrors dockercli.NetworkSpec; kept as its own type so the manifest — the
// documented offline contract — stays free of runtime dependencies.
type NetworkRef struct {
	Name       string `json:"name"`
	Driver     string `json:"driver,omitempty"`
	Scope      string `json:"scope,omitempty"`
	Internal   bool   `json:"internal,omitempty"`
	Attachable bool   `json:"attachable,omitempty"`
	EnableIPv6 bool   `json:"enable_ipv6,omitempty"`
	// Primary pool, flattened for the common single-pool case.
	Subnet  string `json:"subnet,omitempty"`
	Gateway string `json:"gateway,omitempty"`
	IPRange string `json:"ip_range,omitempty"`
	// Pools carries EVERY pool when there is more than one (dual-stack), so a
	// multi-pool network loses nothing while a normal one records no duplication.
	Pools   []NetworkPoolRef  `json:"pools,omitempty"`
	Options map[string]string `json:"options,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
	// This container's endpoint: the address it was PINNED to (not a lease the
	// daemon happened to hand out), and its DNS aliases.
	IPv4    string   `json:"ipv4,omitempty"`
	IPv6    string   `json:"ipv6,omitempty"`
	Aliases []string `json:"aliases,omitempty"`
}

// SkippedMount records a mount that was NOT captured in this backup and why, so
// a near-empty backup (e.g. a container whose only real data is a large media
// bind mount excluded by default) is honestly reported as PARTIAL rather than
// silently looking like a complete success.
type SkippedMount struct {
	Destination string `json:"destination"`
	Source      string `json:"source,omitempty"` // host path for binds
	Type        string `json:"type"`             // "volume" | "bind"
	Reason      string `json:"reason"`
	Bytes       int64  `json:"bytes,omitempty"` // measured size when known
	// CoveredBy is set when another container on the same node captures this
	// same host Source in its own backups (F83) — an intentional single-capture
	// of a shared bind, not data loss. A skip with CoveredBy set is NOT counted
	// as PARTIAL by the grade/runbook/digest, though it stays listed.
	CoveredBy string `json:"covered_by,omitempty"`
}

// Finding severities. Three, deliberately: the register's P0–P3 levels rank
// implementation work, not what an operator is looking at, and a fourth level
// would be a distinction nobody could apply consistently at the call site.
const (
	// findingDuplicateMountTarget — one host path mounted at several destinations
	// in one container (#21). Shared here because the restore's ownership guard
	// keys off the same shape the capture reports.
	findingDuplicateMountTarget = "duplicate-mount-target"

	// findingStatefulUndetectedEngine — a mount holds a database's own files but
	// the image is not one engine detection recognises (#3), so the capture is a
	// file copy rather than a dump.
	findingStatefulUndetectedEngine = "stateful-volume-undetected-engine"

	// findingIrreplaceableSecret — this container's environment holds values that
	// cannot be re-created, so the archive is their only other copy (#11).
	findingIrreplaceableSecret = "irreplaceable-secret"

	// findingChangedDuringCopy — a file moved while the archive was being written
	// (#25), so the archive holds a smear across the copy window rather than one
	// instant.
	findingChangedDuringCopy = "changed-during-copy"

	// findingOffHostRedirect — the restored application sends visitors to another
	// host, so testing it tests the original (#10).
	findingOffHostRedirect = "off-host-redirect"
	// findingOriginRejected — it renders but refuses writes from this host (#33).
	findingOriginRejected = "origin-rejected"

	// findingDeclaredCredentialInert — the environment declares a credential that
	// does not work, so the declared configuration does not describe the running
	// system (#13).
	findingDeclaredCredentialInert = "declared-credential-inert"

	// findingRestartPolicyNotBootSafe — this container will not come back when
	// the host reboots (#8).
	findingRestartPolicyNotBootSafe = "restart-policy-not-boot-safe"

	// findingHealthcheckCannotFail — this container's health probe passes when
	// the thing it checks is broken (#35).
	findingHealthcheckCannotFail = "healthcheck-cannot-fail"

	// findingHealthcheckMissing — this database reports nothing better than
	// "running", so everything that waits on it races it (#16).
	findingHealthcheckMissing = "healthcheck-missing"

	// findingSharedMountVersionSkew — two containers running different builds of
	// the same image share one directory (#23).
	findingSharedMountVersionSkew = "shared-mount-version-skew"

	// findingComposeUserHostSpecific — a `user:` override in the stack file pins
	// this container to a raw id that means nothing on another machine (#24).
	findingComposeUserHostSpecific = "compose-user-host-specific"

	// findingLocalTagDrifted — this tag resolves to a different image in its
	// registry than the one running here, so the next pull is an unplanned
	// upgrade (#29).
	findingLocalTagDrifted = "local-tag-drifted"
	// findingLocalTagRemoved — the container's own reference is no longer a tag
	// on this host. Not a defect: the restore pins the digest (#29).
	findingLocalTagRemoved = "local-tag-removed"

	// findingRunAsConventionForeign — the ids this container asks its image to
	// run as come from a NAS's numbering scheme, not this host's (#5).
	findingRunAsConventionForeign = "run-as-convention-foreign"

	// FindingInfo — worth knowing, nothing is wrong.
	FindingInfo = "info"
	// FindingWarn — a real defect in the source that a restore will reproduce.
	FindingWarn = "warn"
	// FindingDanger — a security consequence, or a defect where the obvious fix
	// makes it worse. Nextcloud's world-writable my.cnf is the case that named
	// this: it is inert only because a second misconfiguration rejects it, and
	// chmod 644, chown root and adding :ro each activate skip_grant_tables.
	FindingDanger = "danger"
)

// Finding is one thing discovered about the SOURCE during capture that the
// operator should see — reported, never silently reproduced or fixed.
type Finding struct {
	// Code is a stable slug ("duplicate-mount-target"), so the UI and any future
	// filtering key off something that does not change when the wording does.
	Code string `json:"code"`
	// Severity is one of FindingInfo, FindingWarn, FindingDanger.
	Severity string `json:"severity"`
	// Message is the operator-facing sentence, and it names the fix. A finding
	// that says what is wrong without saying what to do about it is a puzzle.
	Message string `json:"message"`
	// Subject is what the finding is about — a path, an environment KEY (never
	// its value), an image reference. Empty when the finding is about the
	// container as a whole.
	Subject string `json:"subject,omitempty"`
}

// HasUncoveredSkip reports whether any skipped mount represents data genuinely
// absent from every backup on the node (F83) — the condition that makes a
// backup PARTIAL. A skip covered by another container's backups doesn't count.
func (m *Manifest) HasUncoveredSkip() bool {
	if m == nil {
		return false
	}
	for _, sk := range m.SkippedMounts {
		if sk.CoveredBy == "" {
			return true
		}
	}
	return false
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
