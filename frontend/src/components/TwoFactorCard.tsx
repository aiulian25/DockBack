// Two-factor authentication (TOTP) management — enrol with a QR, confirm a code,
// store one-time recovery codes; disable with the password AND a current code
// (or a recovery code, for a lost authenticator). PLAN §10.2.
import { useEffect, useRef, useState } from "react";
import { ShieldCheck, ShieldOff, Loader2, Copy, Check } from "lucide-react";
import { api } from "../api";
import { Button, Card, Input, Label } from "./ui";

type Stage = "idle" | "enrolling" | "codes" | "disabling";

export default function TwoFactorCard() {
  const [status, setStatus] = useState<{ enabled: boolean; recovery_remaining: number } | null>(null);
  const [stage, setStage] = useState<Stage>("idle");
  const [secret, setSecret] = useState("");
  const [qr, setQr] = useState("");
  const [code, setCode] = useState("");
  const [password, setPassword] = useState("");
  const [recovery, setRecovery] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const [copied, setCopied] = useState(false);
  const reset = useRef(() => {});

  const load = () => api.twofaStatus().then(setStatus).catch(() => setStatus(null));
  useEffect(() => { load(); }, []);

  reset.current = () => {
    setStage("idle"); setSecret(""); setQr(""); setCode(""); setPassword(""); setRecovery([]); setMsg(null);
  };

  const begin = async () => {
    setBusy(true); setMsg(null);
    try {
      const r = await api.twofaBegin();
      setSecret(r.secret);
      // qrcode loads on demand (perf Fix 3): ~70 KB that only 2FA enrolment needs.
      const { toDataURL } = await import("qrcode");
      setQr(await toDataURL(r.otpauth_uri, { margin: 1, width: 200 }));
      setStage("enrolling");
    } catch (e) { setMsg({ ok: false, text: (e as Error).message }); }
    finally { setBusy(false); }
  };

  const enable = async () => {
    setBusy(true); setMsg(null);
    try {
      const r = await api.twofaEnable(code.trim());
      setRecovery(r.recovery_codes); setStage("codes"); setCode("");
    } catch (e) { setMsg({ ok: false, text: (e as Error).message }); }
    finally { setBusy(false); }
  };

  const disable = async () => {
    setBusy(true); setMsg(null);
    try {
      await api.twofaDisable(password, code.trim());
      reset.current(); await load();
      setMsg({ ok: true, text: "Two-factor authentication disabled." });
    } catch (e) { setMsg({ ok: false, text: (e as Error).message }); }
    finally { setBusy(false); }
  };

  const finishCodes = async () => { reset.current(); await load(); };
  const copyCodes = () => {
    navigator.clipboard?.writeText(recovery.join("\n"));
    setCopied(true); setTimeout(() => setCopied(false), 1500);
  };

  return (
    <Card className="h-full p-5">
      <div className="mb-4 flex items-center gap-2 text-lg font-semibold">
        <ShieldCheck size={18} className="text-primary" /> Two-Factor Authentication
      </div>

      {status === null && <div className="text-sm text-on-surface-variant">Loading…</div>}

      {/* Enabled, idle */}
      {status?.enabled && stage === "idle" && (
        <div className="space-y-3">
          <div className="flex items-center gap-2 text-sm">
            <span className="inline-flex items-center gap-1.5 rounded bg-success/10 px-2 py-0.5 text-xs font-semibold uppercase text-success"><ShieldCheck size={12} /> On</span>
            <span className="text-on-surface-variant">{status.recovery_remaining} recovery code{status.recovery_remaining === 1 ? "" : "s"} remaining</span>
          </div>
          <Button variant="secondary" onClick={() => { setStage("disabling"); setMsg(null); }}><ShieldOff size={15} /> Disable 2FA</Button>
        </div>
      )}

      {/* Disabled, idle */}
      {status && !status.enabled && stage === "idle" && (
        <div className="space-y-3">
          <p className="text-sm text-on-surface-variant">Require a time-based code from an authenticator app in addition to your password.</p>
          <Button variant="primary" disabled={busy} onClick={begin}>{busy ? <Loader2 size={15} className="animate-spin" /> : <ShieldCheck size={15} />} Enable 2FA</Button>
        </div>
      )}

      {/* Enrolling */}
      {stage === "enrolling" && (
        <div className="space-y-3">
          <div className="flex items-center gap-2">
            <img src="/dockback-mark.png" alt="" className="h-6 w-6 rounded object-contain" />
            <span className="text-sm font-medium">Add <span className="font-semibold text-primary">DockBack</span> to your authenticator</span>
          </div>
          <p className="text-sm text-on-surface-variant">Scan this with your authenticator app (or enter the key manually), then enter the 6-digit code to confirm.</p>
          <div className="flex flex-wrap items-center gap-4">
            {qr && <img src={qr} alt="2FA QR code" className="rounded border border-outline-variant bg-white p-1" />}
            <div className="text-xs">
              <div className="mb-1 text-on-surface-variant">Manual key</div>
              <code className="select-all break-all font-mono text-on-surface">{secret}</code>
            </div>
          </div>
          <div className="max-w-xs">
            <Label>6-digit code</Label>
            <Input value={code} onChange={(e) => setCode(e.target.value)} inputMode="numeric" autoComplete="one-time-code" placeholder="123456" />
          </div>
          {msg && <div className={`rounded px-3 py-2 text-sm ${msg.ok ? "bg-success/10 text-success" : "bg-error/10 text-error"}`}>{msg.text}</div>}
          <div className="flex gap-2">
            <Button variant="primary" disabled={busy || code.trim().length < 6} onClick={enable}>{busy ? <Loader2 size={15} className="animate-spin" /> : <ShieldCheck size={15} />} Verify &amp; enable</Button>
            <Button variant="ghost" onClick={() => reset.current()}>Cancel</Button>
          </div>
        </div>
      )}

      {/* Recovery codes (shown once) */}
      {stage === "codes" && (
        <div className="space-y-3">
          <div className="rounded border border-warning/30 bg-warning/10 px-3 py-2 text-sm text-warning">
            Save these recovery codes now — each works once and they won't be shown again. Use one if you lose your authenticator.
          </div>
          <div className="grid max-w-md grid-cols-2 gap-2 rounded border border-outline-variant bg-surface-lowest p-3 font-mono text-sm">
            {recovery.map((c) => <span key={c} className="select-all">{c}</span>)}
          </div>
          <div className="flex gap-2">
            <Button variant="secondary" onClick={copyCodes}>{copied ? <><Check size={15} className="text-success" /> Copied</> : <><Copy size={15} /> Copy codes</>}</Button>
            <Button variant="primary" onClick={finishCodes}>I've saved them — done</Button>
          </div>
        </div>
      )}

      {/* Disabling (re-auth) */}
      {stage === "disabling" && (
        <div className="max-w-xs space-y-3">
          <p className="text-sm text-on-surface-variant">Confirm your password and a current two-factor code to turn off two-factor authentication.</p>
          <div>
            <Label>Password</Label>
            <Input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" />
          </div>
          <div>
            <Label>Two-factor code</Label>
            {/* The `code` state is shared with enrolment — the two stages never
                overlap, and reset() clears it between them. */}
            <Input value={code} onChange={(e) => setCode(e.target.value)} inputMode="numeric" autoComplete="one-time-code"
              placeholder="123456 or a recovery code" />
            <p className="mt-1 text-xs text-on-surface-variant">Lost your authenticator? Use one of your recovery codes.</p>
          </div>
          {msg && <div className={`rounded px-3 py-2 text-sm ${msg.ok ? "bg-success/10 text-success" : "bg-error/10 text-error"}`}>{msg.text}</div>}
          <div className="flex gap-2">
            <Button variant="danger" disabled={busy || !password || !code.trim()} onClick={disable}>{busy ? <Loader2 size={15} className="animate-spin" /> : <ShieldOff size={15} />} Disable 2FA</Button>
            <Button variant="ghost" onClick={() => reset.current()}>Cancel</Button>
          </div>
        </div>
      )}

      {msg && stage === "idle" && <div className={`mt-3 rounded px-3 py-2 text-sm ${msg.ok ? "bg-success/10 text-success" : "bg-error/10 text-error"}`}>{msg.text}</div>}
    </Card>
  );
}
