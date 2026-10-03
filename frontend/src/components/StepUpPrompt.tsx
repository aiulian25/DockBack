// StepUpPrompt — inline re-authentication ("sudo mode", F64). Rendered inside
// the flow that triggered a StepUpError so the user confirms their password
// (and TOTP code when 2FA is on) without losing context; the parent retries the
// original call with the entered credentials.
import { useState } from "react";
import { Lock, Loader2, ShieldCheck } from "lucide-react";
import { Button, Input, Label } from "./ui";

export default function StepUpPrompt({ totp, busy, error, onConfirm, confirmLabel = "Confirm" }: {
  totp: boolean; // show the two-factor code field
  busy?: boolean;
  error?: string; // server message from a failed attempt ("password is incorrect", …)
  onConfirm: (password: string, code: string) => void;
  confirmLabel?: string;
}) {
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const canConfirm = password.length > 0 && (!totp || code.trim().length > 0) && !busy;
  const submit = () => { if (canConfirm) onConfirm(password, code.trim()); };

  return (
    <div className="rounded border border-outline-variant bg-surface-lowest p-3">
      <div className="flex items-center gap-2 text-sm font-medium text-on-surface"><Lock size={15} className="text-primary" /> Re-authentication required</div>
      <p className="mt-1 text-xs text-on-surface-variant">This is a security-critical action. Confirm your password to continue{totp ? " (two-factor code required)" : ""} — you won't be asked again for a few minutes.</p>
      <div className="mt-2 flex flex-col gap-2">
        <div>
          <Label>Password</Label>
          <Input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" autoFocus
            onKeyDown={(e) => { if (e.key === "Enter") submit(); }} />
        </div>
        {totp && (
          <div>
            <Label>Two-factor code</Label>
            <Input value={code} onChange={(e) => setCode(e.target.value)} inputMode="numeric" autoComplete="one-time-code" placeholder="123456 or a recovery code"
              onKeyDown={(e) => { if (e.key === "Enter") submit(); }} />
          </div>
        )}
        {error && <p className="text-xs text-error">{error}</p>}
        <div>
          <Button variant="primary" disabled={!canConfirm} onClick={submit}>
            {busy ? <Loader2 size={15} className="animate-spin" /> : <ShieldCheck size={15} />} {confirmLabel}
          </Button>
        </div>
      </div>
    </div>
  );
}
