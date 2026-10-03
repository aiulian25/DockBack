import { useEffect, useRef, useState } from "react";
import { useNavigate, useLocation } from "react-router-dom";
import { api } from "../api";
import { Button, Input, Label } from "../components/ui";

export default function Login() {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [needCode, setNeedCode] = useState(false);
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const navigate = useNavigate();
  const expired = new URLSearchParams(useLocation().search).get("expired") === "1";
  useEffect(() => { document.title = "Sign in · DockBack"; }, []);
  // Remember the last code we auto-submitted so a rejected code doesn't loop —
  // the user must change it (which yields a new value) before we retry.
  const lastTried = useRef("");

  const submit = async (e?: React.FormEvent) => {
    e?.preventDefault();
    if (busy) return;
    setBusy(true); setErr("");
    try {
      const r = await api.login(username, password, needCode ? code.trim() : undefined);
      if (r.status === "totp_required") { setNeedCode(true); setBusy(false); lastTried.current = ""; return; }
      navigate("/");
    } catch (e) {
      setErr((e as Error).message || "login failed");
    } finally {
      setBusy(false);
    }
  };

  // Auto-submit once a complete 2FA code is entered (or autofilled), so the user
  // doesn't have to press Verify: a 6-digit TOTP code, or an 8-char recovery
  // code ("abcd-efgh", dashes/spaces ignored). Both complete formats are
  // unambiguous, so there's no premature submit on a partially typed code.
  useEffect(() => {
    if (!needCode || busy) return;
    const norm = code.replace(/[\s-]/g, "").toLowerCase();
    const complete = /^\d{6}$/.test(norm) || /^[a-z2-7]{8}$/.test(norm);
    if (complete && norm !== lastTried.current) {
      lastTried.current = norm;
      submit();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [code, needCode, busy]);

  return (
    <div className="grid h-full place-items-center p-4">
      <form onSubmit={submit} className="w-full max-w-sm rounded-lg border border-outline-variant/60 bg-surface-container p-8">
        <div className="mb-6 text-center">
          {/* Full logo carries the name; theme-aware variant swaps in via CSS. */}
          <img src="/dockback-logo-dark.png" alt="DockBack" className="brand-logo-dark mx-auto mb-2 h-20 w-auto object-contain" />
          <img src="/dockback-logo-light.png" alt="DockBack" className="brand-logo-light mx-auto mb-2 h-20 w-auto object-contain" />
          <div className="text-xs uppercase tracking-wider text-on-surface-variant">Backup Engine</div>
        </div>
        <div className="space-y-4">
          {expired && !err && (
            <div className="rounded bg-warning/10 px-3 py-2 text-sm text-warning">
              Your session expired and you were signed out. Please sign in again.
            </div>
          )}
          {!needCode ? (
            <>
              <div>
                <Label>Username</Label>
                <Input value={username} onChange={(e) => setUsername(e.target.value)} autoFocus autoComplete="username" />
              </div>
              <div>
                <Label>Password</Label>
                <Input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" />
              </div>
            </>
          ) : (
            <div>
              <Label>Authentication code</Label>
              <Input
                value={code} onChange={(e) => setCode(e.target.value)} autoFocus
                inputMode="numeric" autoComplete="one-time-code" placeholder="6-digit code"
              />
              <p className="mt-1 text-xs text-on-surface-variant">
                Enter the code from your authenticator app, or a recovery code.
              </p>
            </div>
          )}
          {err && <div className="rounded bg-error/10 px-3 py-2 text-sm text-error">{err}</div>}
          <Button variant="primary" className="w-full" disabled={busy}>
            {busy ? "Signing in…" : needCode ? "Verify" : "Sign In"}
          </Button>
        </div>
      </form>
    </div>
  );
}
