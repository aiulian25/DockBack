// "Add Storage Provider" modal — Synology NAS (SMB), Samba/SMB share, or
// Nextcloud (WebDAV). Real Test Connection (write/read/delete round-trip) before
// saving. Credentials are encrypted at rest server-side.
import { useEffect, useRef, useState } from "react";
import { Server, FolderTree, Cloud, Database, Info, ShieldCheck, Gauge, Lock, TerminalSquare, KeyRound } from "lucide-react";
import { api, fmtBytes, Destination, TestDestResult } from "../api";
import { Button, Input, Label, Modal } from "./ui";

// WormVerdict summarizes the Object-Lock (WORM) preflight for the test result box
// (PLAN §9.1): did the bucket actually confirm it enforces immutability?
type WormVerdict = { tone: "ok" | "warn" | "error"; msg: string };

function wormVerdict(r: TestDestResult): WormVerdict | undefined {
  if (r.object_lock_enforced) {
    return { tone: "ok", msg: r.object_lock_detail || "Object Lock verified: this bucket enforces immutability (WORM)." };
  }
  if (r.object_lock_warning) { // configured immutable but bucket doesn't enforce — the dangerous case
    return { tone: "error", msg: r.object_lock_warning + (r.object_lock_detail ? " " + r.object_lock_detail : "") };
  }
  if (r.object_lock_requested && r.object_lock_checked === false) {
    return { tone: "warn", msg: r.object_lock_detail || "Couldn't verify Object Lock (the key may lack read permission). WORM still applies on upload if the bucket was created with Object Lock." };
  }
  return undefined; // not an S3 immutable destination, or nothing worth surfacing
}

// Each provider maps to a backend type + its field set + a short setup guide.
const providers = [
  { v: "synology", type: "synology", label: "Synology NAS (SMB)", icon: Server,
    fields: ["host", "share", "path", "username", "password"], hints: { host: "10.168.1.50", share: "backups", path: "dockback (optional subfolder)" },
    guide: [
      "Control Panel → Shared Folder → Create a folder just for backups (e.g. \"backups\").",
      "Control Panel → User → create a dedicated user and give it Read/Write on ONLY that shared folder (no access to anything else).",
      "Control Panel → File Services → SMB → enable SMB.",
      "Host = NAS IP, Share = the shared folder name, Sub-path = optional subfolder inside it.",
    ] },
  { v: "samba", type: "smb", label: "Samba / SMB share", icon: FolderTree,
    fields: ["host", "share", "path", "username", "password", "domain"], hints: { host: "10.168.1.50[:445]", share: "backups", path: "optional subfolder", domain: "optional" },
    guide: [
      "Create an SMB share dedicated to backups on your server.",
      "Create a user that has write access to ONLY that share.",
      "Host = server IP (add :445 if non-default), Share = the share name.",
      "Domain is only needed for Active Directory / Windows domains — leave blank otherwise.",
    ] },
  { v: "nextcloud", type: "webdav", label: "Nextcloud (WebDAV)", icon: Cloud,
    fields: ["url", "path", "username", "password"], hints: { url: "https://cloud.example.com/remote.php/dav/files/USER/", path: "DockBack (optional subfolder)" },
    guide: [
      "Use an APP PASSWORD, not your login password: Nextcloud → Settings → Security → \"Devices & sessions\" → enter a name → Create new app password.",
      "App passwords work even when 2FA / TOTP is enabled (a login password is rejected with a 401 when 2FA is on).",
      "WebDAV URL = https://YOUR-NEXTCLOUD/remote.php/dav/files/USERNAME/ (include the trailing slash).",
      "Sub-path = a folder to keep backups in (created automatically). DockBack only ever touches this folder.",
      "For strict isolation, create a dedicated Nextcloud user whose only files are the backup folder, and use its app password.",
    ] },
  { v: "sftp", type: "sftp", label: "SSH (SFTP)", icon: TerminalSquare,
    fields: ["host", "port", "user", "dir"], hints: { host: "10.168.1.60", port: "22", user: "dockback", dir: "/srv/backups/dockback" },
    guide: [
      "Any Linux box, Pi or NAS reachable over SSH works — no extra software needed.",
      "Create a dedicated user (e.g. \"dockback\") on the target and give it write access to ONLY the backup directory.",
      "Auth with a private key (recommended — paste the key below) or a password.",
      "The server's SSH host key is PINNED on first connect; if it ever changes, uploads are refused until you reset the pin — the same protection your nodes get.",
    ] },
  { v: "s3", type: "s3", label: "S3 / Backblaze B2", icon: Database,
    fields: ["endpoint", "region", "bucket", "access_key", "secret_key", "path"],
    hints: { endpoint: "s3.amazonaws.com / s3.us-west-002.backblazeb2.com", region: "us-east-1 (optional)", bucket: "dockback-backups", path: "optional prefix" },
    guide: [
      "Create a bucket dedicated to backups.",
      "Create an access key / secret scoped to ONLY that bucket (AWS IAM policy, or a Backblaze B2 application key restricted to the bucket).",
      "Endpoint = your provider's S3 endpoint (AWS: s3.amazonaws.com; B2: s3.us-west-002.backblazeb2.com).",
      "Sub-path = an optional key prefix inside the bucket.",
      "Ransomware-proof (optional): create the bucket with Object Lock ENABLED, then turn on Object Lock below — backups become immutable (WORM) until they expire. Best paired with an append-only key (PutObject only, no DeleteObject).",
    ] },
] as const;

// F78: default for "seal manifests" — ON for off-prem/cloud transports (a third
// party holds the bytes), OFF for LAN shares (recovery-friendliness first).
// Only NEW destinations get the default; existing ones keep their stored choice
// (or, if unset, follow the global manifest-encryption setting).
const sealDefaultFor = (type: string) => (type === "s3" || type === "webdav" ? "true" : "false");

const fieldLabels: Record<string, string> = {
  host: "Host", share: "Share name", path: "Sub-path", username: "Username",
  password: "Password", domain: "Domain", url: "WebDAV URL",
  endpoint: "Endpoint", region: "Region", bucket: "Bucket",
  access_key: "Access key", secret_key: "Secret key",
  user: "Username", port: "Port", dir: "Remote directory", // F66 SFTP
};

export default function AddDestinationModal({ open, onClose, onAdded, purpose = "container", edit = null }: { open: boolean; onClose: () => void; onAdded: () => void; purpose?: "container" | "app"; edit?: Destination | null }) {
  const [provider, setProvider] = useState<string>("synology");
  const [name, setName] = useState("");
  const [cfg, setCfg] = useState<Record<string, string>>({});
  const [test, setTest] = useState<{ ok: boolean; msg: string; worm?: WormVerdict } | null>(null);
  const [busy, setBusy] = useState(false);
  const isEdit = !!edit;
  // The test/error result renders at the bottom of the scrollable body; after
  // the user clicks Test Connection (in the pinned footer) it can land below the
  // fold, so bring it into view whenever a result appears — no missed errors.
  const resultRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (test) resultRef.current?.scrollIntoView({ behavior: "smooth", block: "nearest" });
  }, [test]);

  useEffect(() => {
    if (!open) return;
    setTest(null);
    if (edit) {
      // Edit: lock the provider to the stored type and pre-fill NON-secret fields.
      // Secrets come back blank (kept server-side) — leave blank to keep them.
      setProvider(providers.find((x) => x.type === edit.type)?.v || "synology");
      setName(edit.name);
      setCfg({});
      api.getDestinationConfig(edit.id).then((r) => setCfg(r.config || {})).catch(() => {});
    } else {
      setProvider("synology"); setName(""); setCfg({ seal_manifests: sealDefaultFor("synology") });
    }
  }, [open, edit]);

  const p = providers.find((x) => x.v === provider)!;
  const set = (k: string, v: string) => setCfg((c) => ({ ...c, [k]: v }));
  // Fields the server never sends back: on edit they render blank with a
  // "leave blank to keep current" placeholder, and a blank one keeps the stored
  // value. access_key is here because it is half of a credential pair and the
  // config endpoint no longer returns it.
  const secretField = (f: string) =>
    f === "password" || f === "secret_key" || f === "private_key" || f === "key_passphrase" || f === "access_key";
  // F66 SFTP: which credential the auth block shows (a blank secret on edit keeps the stored one).
  const [sftpAuth, setSftpAuth] = useState<"key" | "password">("key");

  const doTest = async () => {
    setBusy(true); setTest(null);
    try {
      const r = isEdit
        ? await api.testDestinationByID(edit!.id, { config: cfg })
        : await api.testDestination({ type: p.type, config: cfg });
      const cap = r.total_bytes ? ` · ${fmtBytes(r.free_bytes || 0)} free of ${fmtBytes(r.total_bytes)}` : "";
      const pin = r.host_key_fp ? ` · host key pinned ${r.host_key_fp}` : ""; // F66
      setTest(r.ok ? { ok: true, msg: "Reachable, read/write OK" + cap + pin, worm: wormVerdict(r) } : { ok: false, msg: r.error || "unreachable" });
    } catch (e) { setTest({ ok: false, msg: (e as Error).message }); }
    finally { setBusy(false); }
  };

  const save = async () => {
    setBusy(true);
    try {
      // host_key_fp is display-only (derived server-side) — never save it back.
      const { host_key_fp: _hkfp, ...sendCfg } = cfg;
      if (isEdit) await api.updateDestination(edit!.id, { name, type: p.type, config: sendCfg });
      else if (purpose === "app") await api.addAppDestination({ name, type: p.type, config: sendCfg });
      else await api.addDestination({ name, type: p.type, config: sendCfg });
      onAdded(); onClose();
    }
    catch (e) { setTest({ ok: false, msg: (e as Error).message }); }
    finally { setBusy(false); }
  };

  return (
    <Modal
      open={open} onClose={onClose} title={isEdit ? "Edit destination" : "Add Storage Provider"}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>Cancel</Button>
          <Button variant="secondary" onClick={doTest} disabled={busy}>Test Connection</Button>
          <Button variant="primary" onClick={save} disabled={busy || !name}>{isEdit ? "Save changes" : "Add Destination"}</Button>
        </>
      }
    >
      <div className="space-y-4">
        <div>
          <Label>Provider{isEdit && " (fixed — remove & re-add to change type)"}</Label>
          <div className="grid grid-cols-2 gap-2">
            {providers.map((pr) => {
              const Icon = pr.icon;
              const sel = provider === pr.v;
              if (isEdit && !sel) return null; // lock provider when editing
              return (
                <button key={pr.v} disabled={isEdit} onClick={() => { if (isEdit) return; setProvider(pr.v); setCfg({ seal_manifests: sealDefaultFor(pr.type) }); setTest(null); }}
                  className={`flex flex-col items-center gap-2 rounded border px-2 py-3 text-xs ${sel ? "border-docker-blue bg-docker-blue/10 text-primary" : "border-outline-variant text-on-surface-variant hover:bg-surface-high"} ${isEdit ? "cursor-default" : ""}`}>
                  <Icon size={20} /> {pr.label.split(" (")[0]}
                </button>
              );
            })}
          </div>
        </div>

        <div className="rounded border border-outline-variant bg-surface-high/50 px-3 py-2.5 text-xs text-on-surface-variant">
          <div className="mb-1.5 flex items-center gap-1.5 font-medium text-primary">
            <Info size={14} /> How to set up {p.label.split(" (")[0]}
          </div>
          <ol className="list-decimal space-y-1 pl-4">
            {p.guide.map((step, i) => <li key={i}>{step}</li>)}
          </ol>
        </div>

        <div><Label>Display name</Label><Input value={name} onChange={(e) => setName(e.target.value)} placeholder="Offsite NAS" /></div>

        {p.fields.map((f) => (
          <div key={f}>
            <Label>{fieldLabels[f]}{["host", "share", "url", "endpoint", "bucket"].includes(f) ? " *" : ""}</Label>
            <Input
              type={f === "password" ? "password" : "text"}
              value={cfg[f] || ""} onChange={(e) => set(f, e.target.value)}
              placeholder={isEdit && secretField(f) ? "•••••• (leave blank to keep current)" : (p.hints as Record<string, string>)[f] || ""}
            />
          </div>
        ))}

        {/* F66 SFTP: credential block (key or password) + pinned host key row. */}
        {p.type === "sftp" && (
          <div className="rounded border border-outline-variant bg-surface-lowest p-3">
            <div className="mb-1 flex items-center justify-between">
              <div className="flex items-center gap-1.5 text-sm font-medium"><KeyRound size={15} className="text-primary" /> Authentication</div>
              <button type="button" onClick={() => setSftpAuth(sftpAuth === "key" ? "password" : "key")} className="text-xs text-primary hover:underline">
                {sftpAuth === "key" ? "Use password instead" : "Use a private key instead"}
              </button>
            </div>
            {sftpAuth === "key" ? (
              <>
                <Label>Private key</Label>
                <textarea
                  value={cfg.private_key || ""}
                  onChange={(e) => set("private_key", e.target.value)}
                  placeholder={isEdit ? "•••••• (leave blank to keep current)" : "-----BEGIN OPENSSH PRIVATE KEY-----"}
                  rows={4} spellCheck={false}
                  className="w-full rounded border border-outline-variant bg-surface px-3 py-1.5 font-mono text-xs text-on-surface outline-none focus:border-docker-blue"
                />
                <div className="mt-2">
                  <Label>Key passphrase (if the key is encrypted)</Label>
                  <Input type="password" value={cfg.key_passphrase || ""} onChange={(e) => set("key_passphrase", e.target.value)}
                    placeholder={isEdit ? "•••••• (leave blank to keep current)" : "optional"} />
                </div>
              </>
            ) : (
              <>
                <Label>Password</Label>
                <Input type="password" value={cfg.password || ""} onChange={(e) => set("password", e.target.value)}
                  placeholder={isEdit ? "•••••• (leave blank to keep current)" : ""} />
              </>
            )}
            {isEdit && cfg.host_key_fp && (
              <div className="mt-3 flex flex-wrap items-center justify-between gap-2 border-t border-outline-variant/50 pt-2.5">
                <span className="flex items-center gap-1.5 text-xs text-on-surface-variant">
                  <ShieldCheck size={13} className="text-success" /> Host key pinned <span className="font-mono">{cfg.host_key_fp}</span>
                </span>
                <button
                  type="button"
                  onClick={async () => {
                    if (!confirm("Reset the pinned SSH host key?\n\nOnly do this after a legitimate change on the target (e.g. you re-installed it). The fresh key is pinned immediately when the host is reachable.")) return;
                    try {
                      const r = await api.resetDestHostKey(edit!.id);
                      setCfg((c) => ({ ...c, host_key_fp: r.host_key_fp || "" }));
                      setTest({ ok: true, msg: r.repinned ? `New host key pinned: ${r.host_key_fp}` : "Pin cleared — run Test Connection to pin the new key" });
                    } catch (e) { setTest({ ok: false, msg: (e as Error).message }); }
                  }}
                  className="text-xs text-error hover:underline"
                >
                  Reset pinned key
                </button>
              </div>
            )}
            <p className="mt-2 text-xs text-on-surface-variant">The server's SSH host key is pinned on first connect — a changed key refuses uploads until you reset it here, exactly like your nodes.</p>
          </div>
        )}

        {/* F78: per-destination sealed manifest sidecars — metadata privacy where
            a third party holds the bytes. The local archive is unaffected. */}
        <label className="flex cursor-pointer items-start gap-2 rounded border border-outline-variant bg-surface-lowest p-3 text-sm">
          <input
            type="checkbox"
            className="mt-0.5"
            checked={cfg.seal_manifests === "true"}
            onChange={(e) => set("seal_manifests", e.target.checked ? "true" : "false")}
          />
          <span>
            <span className="flex items-center gap-1.5 font-medium"><Lock size={14} className="text-primary" /> Seal manifests on this destination (hides names/paths from the storage provider)</span>
            <span className="mt-0.5 block text-xs text-on-surface-variant">
              Each backup ships with a small manifest sidecar describing it (container name, image, stack, volume paths, database engine).
              Sealed, it is encrypted with your master key so the storage operator sees only opaque data; unsealed, it stays readable and signed for the easiest hand-recovery.
              Your local copies are unaffected, and the offline recovery script opens sealed manifests with your key.
            </span>
          </span>
        </label>

        {/* F97: filesystem retention lock — for the destinations S3 Object Lock
            can't reach. WebDAV is excluded because it has no permission model to
            apply one, and a destination must never claim a protection it can't
            deliver. The wording is deliberately weaker than the S3 block's. */}
        {(p.type === "smb" || p.type === "synology" || p.type === "sftp") && (
          <div className="rounded border border-outline-variant bg-surface-lowest p-3">
            <div className="mb-1 flex items-center gap-1.5 text-sm font-medium"><Lock size={15} className="text-warning" /> Retention lock</div>
            <p className="mb-2 text-xs text-on-surface-variant">
              Makes uploaded backups read-only for this long, and stops DockBack's own retention from pruning them until the period ends.
            </p>
            <div className="max-w-[16rem]">
              <Label>Retention lock (days, 0 = off)</Label>
              <Input type="number" min={0} max={3650} value={cfg.retention_lock_days || ""}
                onChange={(e) => set("retention_lock_days", e.target.value)} placeholder="0" />
            </div>
            {Number(cfg.retention_lock_days) > 0 && (
              <p className="mt-2 text-xs text-warning">
                A speed bump, not S3 Object Lock. It blocks overwrite-in-place and DockBack's own prune for {cfg.retention_lock_days} day(s),
                but anyone with write access to the folder — and root on the target — can still remove these files.
                They also stay on disk for the full period, so pick one you can afford to store.
              </p>
            )}
          </div>
        )}

        {/* Object Lock (WORM) — S3/B2 only (PLAN §9.1) */}
        {p.type === "s3" && (
          <div className="rounded border border-outline-variant bg-surface-lowest p-3">
            <div className="mb-1 flex items-center gap-1.5 text-sm font-medium"><ShieldCheck size={15} className="text-success" /> Object Lock (immutable / WORM)</div>
            <p className="mb-2 text-xs text-on-surface-variant">Makes uploaded backups undeletable until they expire — even with these credentials — so ransomware or a stray prune can't destroy them. The bucket must have Object Lock enabled at creation.</p>
            <div className="grid grid-cols-2 gap-2">
              <div>
                <Label>Mode</Label>
                <select
                  value={cfg.object_lock || ""} onChange={(e) => set("object_lock", e.target.value)}
                  className="w-full rounded border border-outline-variant bg-surface-lowest px-3 py-1.5 text-sm text-on-surface outline-none focus:border-docker-blue"
                >
                  <option value="">Off</option>
                  <option value="governance">Governance (privileged users can override)</option>
                  <option value="compliance">Compliance (no one can delete early)</option>
                </select>
              </div>
              <div>
                <Label>Retention (days)</Label>
                <Input type="number" value={cfg.lock_days || ""} onChange={(e) => set("lock_days", e.target.value)} placeholder="30" />
              </div>
            </div>
            {cfg.object_lock && cfg.object_lock !== "" && (
              <p className="mt-2 text-xs text-warning">Backups stay locked (and are kept by retention) for {cfg.lock_days || "0"} day(s). Set a period you can afford to store. Pair with an append-only key for full protection.</p>
            )}
          </div>
        )}

        <div ref={resultRef} className="scroll-mt-2 space-y-4 empty:hidden">
          {test && (
            <div className={`rounded px-3 py-2 text-sm ${test.ok ? "bg-success/10 text-success" : "bg-error/10 text-error"}`}>
              {test.ok ? "✓ " : "✗ "}{test.msg}
              {!test.ok && p.type === "webdav" && /401|unauthor|authoriz/i.test(test.msg) && (
                <div className="mt-1 text-xs opacity-90">
                  401 = authentication failed. If 2FA / TOTP is enabled, create a Nextcloud app password (Settings → Security → Devices &amp; sessions) and use it here instead of your login password.
                </div>
              )}
            </div>
          )}

          {/* Object Lock (WORM) preflight verdict — a real "is this bucket actually immutable?" check (PLAN §9.1) */}
          {test?.worm && (
            <div className={`flex items-start gap-1.5 rounded px-3 py-2 text-sm ${
              test.worm.tone === "ok" ? "bg-success/10 text-success"
              : test.worm.tone === "error" ? "bg-error/10 text-error"
              : "bg-warning/10 text-warning"}`}>
              <ShieldCheck size={15} className="mt-0.5 shrink-0" />
              <span>{test.worm.msg}</span>
            </div>
          )}
        </div>

        {/* Bandwidth & upload window — optional per-destination throttle/schedule (F14) */}
        <div className="rounded border border-outline-variant bg-surface-lowest p-3">
          <div className="mb-1 flex items-center gap-1.5 text-sm font-medium"><Gauge size={15} className="text-primary" /> Bandwidth &amp; upload window (advanced)</div>
          <p className="mb-2 text-xs text-on-surface-variant">Optional. Keep a metered or slow offsite target from saturating your uplink: cap its rate and/or only upload during a quiet window. A fast LAN NAS can be left unlimited. Outside the window a backup still completes locally; the offsite copy shows <b>Deferred</b> and is sent when the window opens (or via <b>Send offsite now</b>).</p>
          <div>
            <Label>Max upload rate (Mbit/s, 0 = unlimited)</Label>
            <Input type="number" min={0} value={cfg.max_upload_mbps || ""} onChange={(e) => set("max_upload_mbps", e.target.value)} placeholder="0" />
          </div>
          <div className="mt-2">
            <Label>Upload window (optional)</Label>
            <div className="flex items-center gap-2">
              <span className="text-xs text-on-surface-variant">from</span>
              <Input type="time" className="!w-32" value={cfg.upload_window_start || ""} onChange={(e) => set("upload_window_start", e.target.value)} />
              <span className="text-xs text-on-surface-variant">to</span>
              <Input type="time" className="!w-32" value={cfg.upload_window_end || ""} onChange={(e) => set("upload_window_end", e.target.value)} />
            </div>
          </div>
        </div>

        <div className="flex items-start gap-1.5 rounded border border-outline-variant bg-surface-high/50 px-3 py-2 text-xs text-on-surface-variant">
          <ShieldCheck size={14} className="mt-0.5 shrink-0 text-success" />
          <span>DockBack only ever reads, writes and deletes inside the sub-path you configure above — it never touches any other files on this destination.</span>
        </div>
      </div>
    </Modal>
  );
}
