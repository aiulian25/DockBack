// KeyRotateModal — supervised master-key rotation (F16). Supplies the current +
// a new 64-hex key (plus the account password to re-authenticate) and re-wraps
// every backup's data key without re-encrypting archives, then re-seals node,
// destination, notification and 2FA secrets so the running app keeps working.
// Losing the NEW key strands every backup, so the flow forces a "back up the new
// key first" acknowledgement and shows the exact key to persist afterward.
import { useState } from "react";
import { KeyRound, ShieldAlert, Loader2, CheckCircle2, AlertTriangle, RefreshCw, Copy } from "lucide-react";
import { api, KeyRotateResult, StepUpError } from "../api";
import { Button, Modal, Input } from "./ui";

// randomKeyHex returns a fresh 64-hex-character (32-byte) key from the browser CSPRNG.
function randomKeyHex(): string {
  const b = new Uint8Array(32);
  crypto.getRandomValues(b);
  return Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
}

const HEX64 = /^[0-9a-fA-F]{64}$/;

export default function KeyRotateModal({ open, onClose, onRotated }: { open: boolean; onClose: () => void; onRotated?: (fp: string) => void }) {
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [password, setPassword] = useState("");
  const [code, setCode] = useState(""); // F64: TOTP code, asked for when 2FA is on
  const [needCode, setNeedCode] = useState(false);
  const [ackNewKeySaved, setAck] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const [result, setResult] = useState<KeyRotateResult | null>(null);

  const reset = () => { setCurrent(""); setNext(""); setPassword(""); setCode(""); setNeedCode(false); setAck(false); setErr(""); setResult(null); setBusy(false); };
  const close = () => { reset(); onClose(); };

  const canRotate = HEX64.test(current.trim()) && HEX64.test(next.trim()) && current.trim().toLowerCase() !== next.trim().toLowerCase() && password.length > 0 && (!needCode || code.trim().length > 0) && ackNewKeySaved && !busy;

  const rotate = async () => {
    setBusy(true); setErr("");
    try {
      const r = await api.rotateKey(current.trim(), next.trim(), password, code.trim() || undefined);
      setResult(r);
      onRotated?.(r.new_fingerprint);
    } catch (e) {
      // F64 step-up: with 2FA enabled the server asks for the code — surface the
      // field and let the user retry without retyping everything.
      if (e instanceof StepUpError && e.totp_required && !needCode) {
        setNeedCode(true);
        setErr("");
      } else {
        setErr((e as Error).message);
      }
    } finally {
      setBusy(false);
    }
  };

  const copy = (v: string) => navigator.clipboard?.writeText(v).catch(() => {});

  return (
    <Modal open={open} onClose={close} title="Rotate encryption key"
      footer={result
        ? <Button variant="primary" onClick={close}>Done</Button>
        : <><Button variant="ghost" onClick={close} disabled={busy}>Cancel</Button>
            <Button variant="danger" onClick={rotate} disabled={!canRotate}>{busy ? <Loader2 size={15} className="animate-spin" /> : <RefreshCw size={15} />} Rotate key</Button></>}>
      {!result ? (
        <div className="space-y-3 text-sm">
          <div className="flex items-start gap-2 rounded border border-warning/40 bg-warning/10 px-3 py-2 text-warning">
            <ShieldAlert size={16} className="mt-0.5 shrink-0" />
            <span><b>Back up the NEW key before rotating</b> — losing it makes every backup unrecoverable. Rotation re-wraps each backup's key and re-seals node, destination, notification and 2FA secrets; archives are not re-encrypted.</span>
          </div>

          <div>
            <label className="mb-1 block text-xs font-medium text-on-surface-variant">Current key (64 hex characters)</label>
            <Input value={current} onChange={(e) => setCurrent(e.target.value)} placeholder="the DOCKBACK_ENCRYPTION_KEY in use now" className="font-mono text-xs" autoComplete="off" />
          </div>
          <div>
            <div className="mb-1 flex items-center justify-between">
              <label className="text-xs font-medium text-on-surface-variant">New key (64 hex characters)</label>
              <button type="button" onClick={() => setNext(randomKeyHex())} className="flex items-center gap-1 text-xs text-primary hover:underline"><RefreshCw size={12} /> Generate</button>
            </div>
            <Input value={next} onChange={(e) => setNext(e.target.value)} placeholder="generate one, or paste your own" className="font-mono text-xs" autoComplete="off" />
            {next && !HEX64.test(next.trim()) && <p className="mt-1 text-xs text-error">Must be exactly 64 hexadecimal characters.</p>}
            {next && HEX64.test(next.trim()) && (
              <button type="button" onClick={() => copy(next.trim())} className="mt-1 flex items-center gap-1 text-xs text-on-surface-variant hover:text-on-surface"><Copy size={12} /> Copy new key</button>
            )}
          </div>
          <div>
            <label className="mb-1 block text-xs font-medium text-on-surface-variant">Account password</label>
            <Input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" />
          </div>
          {needCode && (
            <div>
              <label className="mb-1 block text-xs font-medium text-on-surface-variant">Two-factor code</label>
              <Input value={code} onChange={(e) => setCode(e.target.value)} inputMode="numeric" autoComplete="one-time-code" placeholder="123456 or a recovery code" />
            </div>
          )}

          <label className="flex cursor-pointer items-start gap-2 text-xs text-on-surface-variant">
            <input type="checkbox" className="mt-0.5" checked={ackNewKeySaved} onChange={(e) => setAck(e.target.checked)} />
            <span>I have <b>saved the new key</b> somewhere safe and offline, and I will set <span className="font-mono">DOCKBACK_ENCRYPTION_KEY</span> to it before the next restart.</span>
          </label>

          {err && <div className="flex items-start gap-2 rounded bg-error/10 px-3 py-2 text-xs text-error"><AlertTriangle size={14} className="mt-0.5 shrink-0" /> {err}</div>}
        </div>
      ) : (
        <div className="space-y-3 text-sm">
          <div className="flex items-start gap-2 rounded border border-success/40 bg-success/10 px-3 py-2 text-success">
            <CheckCircle2 size={16} className="mt-0.5 shrink-0" />
            <span>Key rotated. New fingerprint <span className="font-mono">{result.new_fingerprint}</span>.</span>
          </div>
          <div className="rounded border border-outline-variant bg-surface-lowest px-3 py-2 text-xs">
            <div>Backups re-wrapped: <b className="text-on-surface">{result.backups.rewrapped}</b>{result.backups.skipped > 0 ? <> · skipped: {result.backups.skipped}</> : null}{result.backups.failed > 0 ? <> · failed: <span className="text-error">{result.backups.failed}</span></> : null}</div>
            {result.app_backups && (
              <div className="mt-0.5">App backups re-encrypted: <b className="text-on-surface">{result.app_backups.rewrapped}</b>{result.app_backups.skipped > 0 ? <> · skipped (pre-envelope): {result.app_backups.skipped}</> : null}{result.app_backups.failed > 0 ? <> · <span className="text-error">{result.app_backups.failed} could not be re-encrypted (foreign key)</span></> : null}</div>
            )}
            {/* F72: remote app-config archives on the app-backup destinations. */}
            {result.remote_app_backups && (result.remote_app_backups.rewrapped > 0 || result.remote_app_backups.failed > 0) && (
              <div className="mt-0.5">Remote app backups re-encrypted: <b className="text-on-surface">{result.remote_app_backups.rewrapped}</b>{result.remote_app_backups.failed > 0 ? <> · failed: <span className="text-error">{result.remote_app_backups.failed}</span></> : null}</div>
            )}
            <div className="mt-0.5">Secrets re-sealed: {result.nodes_resealed} node(s), {result.destinations_resealed} destination(s){(result.app_dests_resealed || 0) > 0 ? `, ${result.app_dests_resealed} app-backup destination(s)` : ""}{result.totp_resealed ? ", 2FA" : ""}.</div>
            {result.fresh_app_backup?.file && (
              <div className="mt-0.5">Fresh app-backup created: <span className="font-mono break-all text-on-surface">{result.fresh_app_backup.file}</span></div>
            )}
          </div>
          {result.warnings?.length > 0 && (
            <div className="rounded border border-warning/40 bg-warning/10 px-3 py-2 text-xs text-warning">
              <div className="flex items-center gap-1.5 font-semibold"><AlertTriangle size={13} /> Follow up</div>
              <ul className="mt-1 list-disc pl-5">{result.warnings.map((wm, i) => <li key={i}>{wm}</li>)}</ul>
            </div>
          )}
          <div className="flex items-start gap-2 rounded border border-warning/40 bg-warning/10 px-3 py-2 text-xs text-warning">
            <ShieldAlert size={14} className="mt-0.5 shrink-0" />
            <span>{result.message}</span>
          </div>
        </div>
      )}
    </Modal>
  );
}
