// Passkeys: sign in with the device's own lock (fingerprint, face, PIN) instead
// of the password and code. Adding one needs a fresh password (and code); the
// passkey names itself after the password manager that keeps it.
import { useEffect, useState } from "react";
import { KeyRound, Loader2, Pencil, Trash2 } from "lucide-react";
import { api, Passkey, PasskeyStatus, StepUpError } from "../api";
import { browserName, makePasskey, passkeysSupported } from "../lib/passkey";
import { Button, Card, Input } from "./ui";
import StepUpPrompt from "./StepUpPrompt";

const NAME_MAX = 64;

function when(unix: number): string {
  return new Date(unix * 1000).toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
}

// The page is at the address passkeys are bound to; elsewhere the browser refuses.
const atPublicAddress = (status: PasskeyStatus) => status.origin === "" || status.origin === window.location.origin;

function PasskeyRow({ passkey, onChanged }: { passkey: Passkey; onChanged: () => void }) {
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState(passkey.name);
  const [err, setErr] = useState("");

  const rename = async () => {
    setErr("");
    try { await api.renamePasskey(passkey.id, name.trim()); setEditing(false); onChanged(); }
    catch (e) { setErr((e as Error).message); }
  };
  const remove = async () => {
    if (!window.confirm(`Remove ${passkey.name}? You can still sign in with your password.`)) return;
    try { await api.removePasskey(passkey.id); onChanged(); }
    catch (e) { setErr((e as Error).message); }
  };

  return (
    <li className="rounded border border-outline-variant/60 px-3 py-2 text-sm">
      {editing ? (
        <div className="flex items-center gap-2">
          <Input value={name} maxLength={NAME_MAX} onChange={(e) => setName(e.target.value)} aria-label={`New name for ${passkey.name}`}
            onKeyDown={(e) => { if (e.key === "Enter") rename(); }} autoFocus />
          <Button variant="primary" disabled={!name.trim()} onClick={rename}>Save</Button>
          <Button variant="ghost" onClick={() => { setEditing(false); setName(passkey.name); }}>Cancel</Button>
        </div>
      ) : (
        <div className="flex items-center justify-between gap-2">
          <div className="min-w-0">
            <div className="flex items-center gap-2">
              <span className="truncate font-medium">{passkey.name}</span>
              {passkey.synced && <span className="rounded bg-surface-high px-1.5 py-0.5 text-[10px] uppercase text-on-surface-variant">synced</span>}
            </div>
            <div className="text-xs text-on-surface-variant">
              Added {when(passkey.created_at)} · {passkey.last_used_at ? `last used ${when(passkey.last_used_at)}` : "not used yet"}
            </div>
            {passkey.other_host && (
              <div className="text-xs text-warning">Made for {passkey.other_host}: browsers won't offer it at this address.</div>
            )}
          </div>
          <div className="flex shrink-0 gap-1">
            <Button variant="ghost" title="Rename" aria-label={`Rename ${passkey.name}`} onClick={() => setEditing(true)}><Pencil size={14} /></Button>
            <Button variant="ghost" title="Remove" aria-label={`Remove ${passkey.name}`} onClick={remove}><Trash2 size={14} /></Button>
          </div>
        </div>
      )}
      {err && <p role="alert" className="mt-1 text-xs text-error">{err}</p>}
    </li>
  );
}

export default function PasskeysCard() {
  const [state, setState] = useState<(PasskeyStatus & { passkeys: Passkey[] }) | null>(null);
  const [busy, setBusy] = useState(false);
  const [stepUp, setStepUp] = useState<{ totp: boolean; err: string } | null>(null);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);

  const load = () => api.listPasskeys().then(setState).catch(() => setState(null));
  useEffect(() => { load(); }, []);

  const add = async (password?: string, code?: string) => {
    setBusy(true); setMsg(null);
    try {
      const begun = await api.addPasskeyBegin(password ? { password, code } : {});
      setStepUp(null);
      const credential = await makePasskey(begun.options);
      const added = await api.addPasskey(begun.ceremony, credential, browserName(navigator.userAgent).slice(0, NAME_MAX));
      setMsg({ ok: true, text: `Added ${added.name}. Next time, choose "Sign in with a passkey" on the sign-in page.` });
      await load();
    } catch (e) {
      if (e instanceof StepUpError) setStepUp({ totp: e.totp_required, err: password ? e.message : "" });
      else setMsg({ ok: false, text: (e as Error).message });
    } finally { setBusy(false); }
  };

  const usableHere = state !== null && state.usable && atPublicAddress(state) && passkeysSupported();

  return (
    <Card className="h-full p-5">
      <div className="mb-2 flex items-center gap-2 text-lg font-semibold">
        <KeyRound size={18} className="text-primary" /> Passkeys
      </div>
      <p className="mb-3 text-sm text-on-surface-variant">
        Sign in with your fingerprint, face or device PIN instead of your password and code. The passkey stays on your phone,
        computer or password manager; DockBack keeps only its public key.
      </p>

      {state === null && <div className="text-sm text-on-surface-variant">Loading…</div>}
      {state && !state.usable && <div className="rounded bg-surface-high px-3 py-2 text-sm text-on-surface-variant">{state.reason}</div>}
      {state?.usable && !atPublicAddress(state) && (
        <div className="rounded bg-surface-high px-3 py-2 text-sm text-on-surface-variant">
          Passkeys work at {state.origin}. Open DockBack there to add one; this address can't use them.
        </div>
      )}
      {state?.usable && atPublicAddress(state) && !passkeysSupported() && (
        <div className="rounded bg-surface-high px-3 py-2 text-sm text-on-surface-variant">This browser can't make passkeys.</div>
      )}

      {state && state.passkeys.length > 0 && (
        <ul className="mb-3 space-y-2">{state.passkeys.map((p) => <PasskeyRow key={p.id} passkey={p} onChanged={load} />)}</ul>
      )}
      {state && state.passkeys.length === 0 && state.usable && <p className="mb-3 text-sm text-on-surface-variant">No passkeys yet.</p>}

      {usableHere && !stepUp && (
        <Button variant="primary" disabled={busy} onClick={() => add()}>
          {busy ? <Loader2 size={15} className="animate-spin" /> : <KeyRound size={15} />} Add a passkey
        </Button>
      )}
      {stepUp && (
        <StepUpPrompt totp={stepUp.totp} busy={busy} error={stepUp.err} confirmLabel="Add a passkey" onConfirm={(p, c) => add(p, c)} />
      )}
      {msg && <div role={msg.ok ? "status" : "alert"} className={`mt-3 rounded px-3 py-2 text-sm ${msg.ok ? "bg-success/10 text-success" : "bg-error/10 text-error"}`}>{msg.text}</div>}
    </Card>
  );
}
