// KeyEscrowModal — the one-time "save your encryption key" recovery flow
// (PLAN §9.2). Reveals the master key on an explicit, CSRF-protected, audited
// action, lets the admin download a formatted recovery sheet, and records an
// acknowledgement so the persistent reminder can clear. Losing the key makes
// every backup unrecoverable, so this is treated as a first-class step.
import { useEffect, useState } from "react";
import { KeyRound, Download, Copy, ShieldAlert, Loader2, CheckCircle2, AlertTriangle, Lock } from "lucide-react";
import { api, StepUpError } from "../api";
import { Button, Modal } from "./ui";
import StepUpPrompt from "./StepUpPrompt";

type RecoverInfo = { filename: string; sha256: string; release_url: string; available: boolean } | null;

function recoverySheet(keyHex: string, fp: string, ephemeral: boolean, rec: RecoverInfo): string {
  return [
    "DockBack — Encryption Key Recovery Sheet",
    "Generated: " + new Date().toISOString(),
    "==================================================",
    "",
    "KEEP THIS SECRET AND OFFLINE.",
    "Anyone with this key can decrypt your backups.",
    "WITHOUT THIS KEY, EVERY BACKUP IS PERMANENTLY UNRECOVERABLE.",
    ...(ephemeral
      ? ["", "WARNING: this is an EPHEMERAL key — it is regenerated on restart", "unless you set DOCKBACK_ENCRYPTION_KEY to this value. Do that now."]
      : []),
    "",
    "Master key (DOCKBACK_ENCRYPTION_KEY):",
    "  " + keyHex,
    "",
    "Key fingerprint: " + fp,
    "",
    "To restore on a fresh install, set this in the environment before starting:",
    "  DOCKBACK_ENCRYPTION_KEY=" + keyHex,
    "(or DOCKBACK_ENCRYPTION_KEY_FILE pointing to a file containing this value)",
    "",
    "Store this offline — a password manager, or printed in a safe.",
    "Do NOT store it next to your backups.",
    ...(rec && rec.available && rec.sha256
      ? [
          "",
          "==================================================",
          "RECOVER WITHOUT DOCKBACK",
          "You do not need DockBack — or Docker — to restore. With the key above, one",
          "script decrypts any .dback backup on any machine with Python 3.",
          "",
          "Download the recovery script:",
          "  " + rec.release_url,
          "  (your running DockBack serves the identical file under Settings)",
          "",
          "Verify the download is authentic (its SHA-256 must equal):",
          "  " + rec.sha256,
          "  check with:  sha256sum " + rec.filename,
          "",
          "Run:  python3 " + rec.filename + " --key <the master key above> --in <backup>.dback --out backup.tar",
        ]
      : []),
    "",
  ].join("\n");
}

export default function KeyEscrowModal({ open, onClose, onAck }: { open: boolean; onClose: () => void; onAck?: () => void }) {
  const [ephemeral, setEphemeral] = useState(false);
  const [fp, setFp] = useState("");
  const [keyHex, setKeyHex] = useState("");
  const [revealing, setRevealing] = useState(false);
  const [confirmed, setConfirmed] = useState(false);
  const [downloaded, setDownloaded] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const [kfPass, setKfPass] = useState("");
  // Served by the same status call the modal already makes, so the label and the
  // button agree with what the server will actually accept.
  const [kfMinLen, setKfMinLen] = useState(12);
  const [kfBusy, setKfBusy] = useState(false);
  const [kfMsg, setKfMsg] = useState("");
  const [recover, setRecover] = useState<RecoverInfo>(null);
  const [scriptSaved, setScriptSaved] = useState(false);
  // F64: which action is waiting on a step-up re-authentication (sudo mode).
  const [stepUp, setStepUp] = useState<{ target: "reveal" | "keyfile" | "writeonly-on" | "writeonly-off"; totp: boolean; err: string } | null>(null);
  // F86 write-only backups. `woPriv` is the ONE-TIME private key: it exists in
  // this component's state and nowhere else — never sent back, never stored.
  const [wo, setWo] = useState<{ enabled: boolean; fingerprint: string } | null>(null);
  const [woPriv, setWoPriv] = useState("");
  const [woBusy, setWoBusy] = useState(false);
  const [woSaved, setWoSaved] = useState(false);
  const [woMsg, setWoMsg] = useState("");

  useEffect(() => {
    if (!open) { setKeyHex(""); setConfirmed(false); setDownloaded(false); setErr(""); setKfPass(""); setKfMsg(""); setScriptSaved(false); setStepUp(null); setWoPriv(""); setWoSaved(false); setWoMsg(""); return; }
    api.writeOnlyStatus().then(setWo).catch(() => {});
    api.keyStatus().then((s) => {
      setEphemeral(s.ephemeral); setFp(s.fingerprint);
      if (s.min_passphrase_len > 0) setKfMinLen(s.min_passphrase_len);
    }).catch(() => {});
    // F35: fetch the offline recovery tool's fingerprint so the recovery sheet is a
    // complete, self-verifying kit. Best-effort — the sheet still works without it.
    api.recoveryTool().then(setRecover).catch(() => {});
  }, [open]);

  // Revealing the key is step-up gated (F64): the first attempt sends no
  // credentials; a StepUpError swaps the button for an inline password prompt
  // and this retries with what the user entered.
  const reveal = async (password?: string, code?: string) => {
    setRevealing(true); setErr("");
    try {
      const r = await api.keyReveal(password ? { password, code } : undefined);
      setStepUp(null);
      setKeyHex(r.key_hex); setFp(r.fingerprint); setEphemeral(r.ephemeral);
    } catch (e) {
      if (e instanceof StepUpError) setStepUp({ target: "reveal", totp: e.totp_required, err: password ? e.message : "" });
      else setErr((e as Error).message);
    } finally { setRevealing(false); }
  };

  // F86: arming write-only mode. The private key comes back exactly once — if it
  // is not saved from this response it cannot be re-issued, and every backup taken
  // while the mode is armed becomes unrecoverable. The UI says so before and after.
  const enableWriteOnly = async (password?: string, code?: string) => {
    setWoBusy(true); setErr(""); setWoMsg("");
    try {
      const r = await api.enableWriteOnly(password ? { password, code } : undefined);
      setStepUp(null);
      setWoPriv(r.private_key);
      setWo({ enabled: true, fingerprint: r.fingerprint });
    } catch (e) {
      if (e instanceof StepUpError) setStepUp({ target: "writeonly-on", totp: e.totp_required, err: password ? e.message : "" });
      else setErr((e as Error).message);
    } finally { setWoBusy(false); }
  };

  const disableWriteOnly = async (password?: string, code?: string) => {
    setWoBusy(true); setErr(""); setWoMsg("");
    try {
      const r = await api.disableWriteOnly(password ? { password, code } : undefined);
      setStepUp(null);
      setWo({ enabled: false, fingerprint: "" });
      setWoPriv("");
      setWoMsg(r.still_write_only > 0
        ? `New backups use the master key again. ${r.still_write_only} existing backup${r.still_write_only === 1 ? "" : "s"} stay write-only — keep that recovery sheet.`
        : "New backups use the master key again.");
    } catch (e) {
      if (e instanceof StepUpError) setStepUp({ target: "writeonly-off", totp: e.totp_required, err: password ? e.message : "" });
      else setErr((e as Error).message);
    } finally { setWoBusy(false); }
  };

  const downloadWriteOnlySheet = () => {
    const sheet = [
      "DockBack — Write-Only Backup Recovery Sheet",
      "Generated: " + new Date().toISOString(),
      "==================================================",
      "",
      "KEEP THIS SECRET AND OFFLINE.",
      "This is the ONLY thing that can decrypt backups taken while",
      "write-only mode is enabled. DockBack does NOT have a copy —",
      "that is the entire point of the mode, and it cannot be reissued.",
      "",
      "Keypair fingerprint: " + (wo?.fingerprint || ""),
      "",
      "Offline private key (base64):",
      "  " + woPriv,
      "",
      "To restore with it:",
      "  - In DockBack: open the backup, paste this key in the restore panel.",
      "  - Without DockBack:",
      "      python3 dockback-recover.py --private-key @privkey.txt \\",
      "        --manifest <archive>.dback.manifest.json \\",
      "        --in <archive>.dback --out restored.tar",
      "",
      "Store this separately from your master key sheet.",
    ].join("\n");
    const url = URL.createObjectURL(new Blob([sheet], { type: "text/plain" }));
    const a = document.createElement("a");
    a.href = url; a.download = "dockback-write-only-key.txt";
    document.body.appendChild(a); a.click(); a.remove();
    URL.revokeObjectURL(url);
    setWoSaved(true);
  };

  const downloadScript = async () => {
    setErr("");
    try {
      const res = await fetch("/api/security/recovery-tool/download", { credentials: "include" });
      if (!res.ok) throw new Error("could not download the recovery script");
      const blob = await res.blob();
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url; a.download = recover?.filename || "dockback-recover.py";
      document.body.appendChild(a); a.click(); a.remove();
      URL.revokeObjectURL(url);
      setScriptSaved(true);
    } catch (e) { setErr((e as Error).message); }
  };

  const download = () => {
    const blob = new Blob([recoverySheet(keyHex, fp, ephemeral, recover)], { type: "text/plain" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url; a.download = `dockback-recovery-${fp || "key"}.txt`;
    document.body.appendChild(a); a.click(); a.remove();
    URL.revokeObjectURL(url);
    setDownloaded(true);
  };

  const acknowledge = async () => {
    setBusy(true); setErr("");
    try { await api.keyAcknowledge(); onAck?.(); onClose(); }
    catch (e) { setErr((e as Error).message); }
    finally { setBusy(false); }
  };

  const downloadKeyfile = async (password?: string, code?: string) => {
    setKfBusy(true); setKfMsg("");
    try {
      const r = await api.keyKeyfile(kfPass, password ? { password, code } : undefined);
      setStepUp(null);
      const blob = new Blob([r.keyfile], { type: "application/json" });
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url; a.download = "dockback.keyfile.json";
      document.body.appendChild(a); a.click(); a.remove();
      URL.revokeObjectURL(url);
      setKfMsg("Downloaded. Mount it, set DOCKBACK_ENCRYPTION_KEYFILE + DOCKBACK_ENCRYPTION_PASSPHRASE, remove DOCKBACK_ENCRYPTION_KEY, then restart.");
    } catch (e) {
      if (e instanceof StepUpError) setStepUp({ target: "keyfile", totp: e.totp_required, err: password ? e.message : "" });
      else setKfMsg((e as Error).message);
    } finally { setKfBusy(false); }
  };

  return (
    <Modal open={open} onClose={onClose} title="Back up your encryption key"
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>Close</Button>
          <Button variant="primary" disabled={!keyHex || !confirmed || busy} onClick={acknowledge}>
            {busy ? <Loader2 size={15} className="animate-spin" /> : <CheckCircle2 size={15} />} I've saved it
          </Button>
        </>
      }
    >
      <div className="space-y-4 text-sm">
        <div className="flex items-start gap-2 rounded border border-error/30 bg-error/10 p-3 text-error">
          <ShieldAlert size={18} className="mt-0.5 shrink-0" />
          <p>Your backups are encrypted with this key. <strong>If you lose it, every backup is permanently unrecoverable</strong> — there is no reset. Save it somewhere safe and offline.</p>
        </div>

        {ephemeral && (
          <div className="flex items-start gap-2 rounded border border-warning/30 bg-warning/10 p-3 text-warning">
            <AlertTriangle size={18} className="mt-0.5 shrink-0" />
            <p>No persistent key is set, so a <strong>temporary key was generated</strong> — it will change on restart and any backups made now become unreadable. Set <code className="font-mono">DOCKBACK_ENCRYPTION_KEY</code> to the value below (in your <code className="font-mono">.env</code>/secret) and restart.</p>
          </div>
        )}

        <div className="text-on-surface-variant">Key fingerprint: <span className="font-mono text-on-surface">{fp || "…"}</span></div>

        {!keyHex ? (
          stepUp?.target === "reveal" ? (
            <StepUpPrompt totp={stepUp.totp} busy={revealing} error={stepUp.err} confirmLabel="Reveal key" onConfirm={(p, c) => reveal(p, c)} />
          ) : (
            <Button variant="secondary" onClick={() => reveal()} disabled={revealing}>
              {revealing ? <Loader2 size={15} className="animate-spin" /> : <KeyRound size={15} />} Reveal key & download recovery sheet
            </Button>
          )
        ) : (
          <div className="space-y-3">
            <div>
              <div className="mb-1 text-xs font-medium uppercase tracking-wider text-on-surface-variant">Master key (DOCKBACK_ENCRYPTION_KEY)</div>
              <div className="flex items-center gap-2">
                <code className="block flex-1 break-all rounded border border-outline-variant bg-surface-lowest p-2 font-mono text-xs text-on-surface">{keyHex}</code>
                <Button variant="ghost" title="Copy" onClick={() => { navigator.clipboard?.writeText(keyHex); }}><Copy size={15} /></Button>
              </div>
            </div>
            <div className="flex gap-2">
              <Button variant="secondary" onClick={download}><Download size={15} /> Download recovery sheet</Button>
              {downloaded && <span className="flex items-center gap-1 text-xs text-success"><CheckCircle2 size={13} /> Downloaded</span>}
            </div>

            {recover?.available && (
              <div className="rounded border border-outline-variant bg-surface-lowest p-3">
                <div className="mb-1 text-xs font-medium uppercase tracking-wider text-on-surface-variant">Recover without DockBack</div>
                <p className="text-xs text-on-surface-variant">No lock-in: with this key, one dependency-free Python script decrypts any backup on any machine — no DockBack, no Docker. Keep it with your key so the sheet is a complete recovery kit.</p>
                <div className="mt-2 flex flex-wrap items-center gap-2">
                  <Button variant="secondary" onClick={downloadScript}><Download size={15} /> Download {recover.filename}</Button>
                  {scriptSaved && <span className="flex items-center gap-1 text-xs text-success"><CheckCircle2 size={13} /> Saved</span>}
                </div>
                <div className="mt-2 break-all text-xs text-on-surface-variant">SHA-256: <span className="font-mono text-on-surface">{recover.sha256}</span></div>
              </div>
            )}

            <label className="flex cursor-pointer items-start gap-2 pt-1">
              <input type="checkbox" className="mt-0.5" checked={confirmed} onChange={(e) => setConfirmed(e.target.checked)} />
              <span>I have saved this key somewhere safe (e.g. a password manager) — separate from my backups.</span>
            </label>
          </div>
        )}

        {err && <p className="text-error">{err}</p>}

        {/* F86: write-only backups — DockBack seals to a public key it cannot open. */}
        <details className="rounded border border-outline-variant bg-surface-lowest p-3">
          <summary className="flex cursor-pointer items-center gap-2 text-sm font-medium">
            <ShieldAlert size={15} className="text-primary" /> Write-only backups
            {wo?.enabled && <span className="rounded bg-success/10 px-1.5 py-0.5 text-[10px] font-bold uppercase tracking-wide text-success">On</span>}
          </summary>
          <p className="mt-2 text-xs text-on-surface-variant">
            Normally DockBack can read every backup it has ever made — so anyone who breaks into this
            container can too, on every destination, no matter what immutability you set.
            With write-only backups, DockBack seals each backup to a <strong>public</strong> key and keeps
            only that. <strong>DockBack can create backups but not read them.</strong> Restoring needs a private
            key you keep offline and paste in once, for that one restore.
          </p>
          <p className="mt-2 text-xs text-on-surface-variant">
            The trade is real and worth understanding: a backup DockBack cannot read is one it cannot
            <strong> test</strong> either. Integrity checks (checksum + signed manifest) keep working, but deep
            verification, automatic restore drills, file search and incremental deltas do not — those
            backups always capture in full, and confidence grades cap at <strong>B</strong> until you rehearse a
            restore yourself.
          </p>

          {woPriv ? (
            <div className="mt-3 rounded border border-warning/40 bg-warning/[0.08] p-3">
              <div className="mb-1 flex items-center gap-2 text-sm font-semibold text-warning">
                <AlertTriangle size={15} /> Save this private key — it is the ONLY way to restore
              </div>
              <p className="mb-2 text-xs text-on-surface-variant">
                DockBack does not keep a copy and cannot reissue it. Lose it and every backup taken from
                now on is permanently unrecoverable. Store it separately from your master key.
              </p>
              <div className="break-all rounded border border-outline-variant bg-surface px-3 py-2 font-mono text-xs text-on-surface">{woPriv}</div>
              <div className="mt-2 flex flex-wrap items-center gap-2">
                <Button variant="secondary" onClick={() => navigator.clipboard?.writeText(woPriv)}><Copy size={15} /> Copy</Button>
                <Button variant="secondary" onClick={downloadWriteOnlySheet}><Download size={15} /> Download recovery sheet</Button>
                {woSaved && <span className="flex items-center gap-1 text-xs text-success"><CheckCircle2 size={13} /> Downloaded</span>}
              </div>
              <Button className="mt-3" variant="primary" onClick={() => setWoPriv("")} disabled={!woSaved}>
                I have saved it — hide the key
              </Button>
            </div>
          ) : wo?.enabled ? (
            <div className="mt-3">
              <p className="text-xs text-on-surface-variant">
                Armed — new backups are sealed to keypair <span className="font-mono text-on-surface">{wo.fingerprint}</span>.
                Turning this off returns NEW backups to the master key; existing write-only backups still need
                their offline key, so keep the recovery sheet either way.
              </p>
              <Button className="mt-2" variant="secondary" onClick={() => disableWriteOnly()} disabled={woBusy}>
                {woBusy ? <Loader2 size={15} className="animate-spin" /> : <Lock size={15} />} Turn off write-only backups
              </Button>
            </div>
          ) : (
            <div className="mt-3">
              <Button variant="secondary" onClick={() => enableWriteOnly()} disabled={woBusy}>
                {woBusy ? <Loader2 size={15} className="animate-spin" /> : <ShieldAlert size={15} />} Enable write-only backups
              </Button>
            </div>
          )}
          {woMsg && <p className="mt-2 text-xs text-success">{woMsg}</p>}
          {(stepUp?.target === "writeonly-on" || stepUp?.target === "writeonly-off") && (
            <div className="mt-3">
              <StepUpPrompt
                totp={stepUp.totp}
                busy={woBusy}
                error={stepUp.err}
                confirmLabel={stepUp.target === "writeonly-on" ? "Enable write-only" : "Turn off write-only"}
                onConfirm={(pw, c) => (stepUp.target === "writeonly-on" ? enableWriteOnly(pw, c) : disableWriteOnly(pw, c))}
              />
            </div>
          )}
        </details>

        {/* Advanced: store the key encrypted at rest (passphrase-wrapped keyfile, PLAN §9.2) */}
        <details className="rounded border border-outline-variant bg-surface-lowest p-3">
          <summary className="flex cursor-pointer items-center gap-2 text-sm font-medium"><Lock size={15} className="text-primary" /> Advanced: passphrase-protected keyfile</summary>
          <p className="mt-2 text-xs text-on-surface-variant">Instead of keeping the raw key in your <code className="font-mono">.env</code>, download an encrypted keyfile and unlock it with a passphrase at boot. Mount the file, set <code className="font-mono">DOCKBACK_ENCRYPTION_KEYFILE</code> + <code className="font-mono">DOCKBACK_ENCRYPTION_PASSPHRASE</code> (or <code className="font-mono">…_PASSPHRASE_FILE</code>), then remove <code className="font-mono">DOCKBACK_ENCRYPTION_KEY</code> and restart. <strong>Keep the passphrase safe too</strong> — it can't be recovered.</p>
          <div className="mt-2 flex items-end gap-2">
            <div className="flex-1">
              <label className="mb-1 block text-xs text-on-surface-variant">Passphrase (min {kfMinLen} chars)</label>
              <input type="password" value={kfPass} onChange={(e) => setKfPass(e.target.value)} placeholder="a strong passphrase"
                className="w-full rounded border border-outline-variant bg-surface px-3 py-1.5 text-sm text-on-surface outline-none focus:border-docker-blue" />
            </div>
            <Button variant="secondary" disabled={kfBusy || kfPass.length < kfMinLen} onClick={() => downloadKeyfile()}>
              {kfBusy ? <Loader2 size={15} className="animate-spin" /> : <Download size={15} />} Keyfile
            </Button>
          </div>
          {stepUp?.target === "keyfile" && (
            <div className="mt-2">
              <StepUpPrompt totp={stepUp.totp} busy={kfBusy} error={stepUp.err} confirmLabel="Generate keyfile" onConfirm={(p, c) => downloadKeyfile(p, c)} />
            </div>
          )}
          {kfMsg && <p className="mt-2 text-xs text-on-surface-variant">{kfMsg}</p>}
        </details>
      </div>
    </Modal>
  );
}
