// Notifications settings (PLAN §4.10): pluggable Gotify / Email / Webhook
// channels, each subscribing to success and/or failure (verification failure is
// the headline alert). Secrets are write-only — the server masks them and keeps
// the stored value when a field is left blank. Nothing is hardcoded.
import { useCallback, useEffect, useRef, useState } from "react";
import { Bell, Send, Loader2 } from "lucide-react";
import { api, NotifyConfig, emptyNotifyConfig } from "../api";
import { Card, Button, Input, Label, Select } from "./ui";
import { useRegisterSaver } from "./SettingsSave";

export default function NotificationsCard() {
  const [cfg, setCfg] = useState<NotifyConfig>(emptyNotifyConfig);
  const [cfgBaseline, setCfgBaseline] = useState<NotifyConfig>(emptyNotifyConfig); // last-saved
  const [tokenSet, setTokenSet] = useState(false);
  const [pwSet, setPwSet] = useState(false);
  const [testing, setTesting] = useState("");                                        // channel whose test is in flight
  const [results, setResults] = useState<Record<string, { ok: boolean; msg: string }>>({}); // per-channel last test result

  const load = () =>
    api.getNotifications().then((r) => { setCfg(r.config); setCfgBaseline(r.config); setTokenSet(r.gotify_token_set); setPwSet(r.email_password_set); }).catch(() => {});
  useEffect(() => { load(); }, []);

  // Register with the single global Save (replaces this card's own Save button).
  const cfgRef = useRef(cfg); cfgRef.current = cfg;
  const cfgBaseRef = useRef(cfgBaseline); cfgBaseRef.current = cfgBaseline;
  const saveNotif = useCallback(async () => {
    const r = await api.setNotifications(cfgRef.current);
    setCfg(r.config); setCfgBaseline(r.config); setTokenSet(r.gotify_token_set); setPwSet(r.email_password_set);
  }, []);
  // Discard: revert to the last-saved config (drives the global bar's Discard button).
  const resetNotif = useCallback(() => setCfg(cfgBaseRef.current), []);
  const dirty = JSON.stringify(cfg) !== JSON.stringify(cfgBaseline);
  useRegisterSaver("notifications", dirty, saveNotif, resetNotif);
  // Per-channel test: shows a pending state on the button and a clear success/
  // failure box directly beneath it, so feedback is never lost at the foot of a
  // long card. Tests the IN-FORM config (the server merges any stored secret), so
  // a channel can be verified before saving. The failure text is the real server
  // error (e.g. an Uptime Kuma "HTTP 404: Monitor not found or not active.").
  const test = async (channel: string) => {
    setTesting(channel);
    setResults((r) => { const n = { ...r }; delete n[channel]; return n; });
    try {
      await api.testNotification(channel, cfg);
      setResults((r) => ({ ...r, [channel]: { ok: true, msg: "Test notification sent — check it arrived." } }));
    } catch (e) {
      setResults((r) => ({ ...r, [channel]: { ok: false, msg: (e as Error).message } }));
    } finally {
      setTesting("");
    }
  };

  // testBtn renders one channel's Send-test button with a pending spinner and its
  // inline colored result, so every Test button gives immediate, unambiguous feedback.
  const testBtn = (channel: string) => {
    const res = results[channel];
    const busy = testing === channel;
    return (
      <div className="space-y-2">
        <Button variant="secondary" onClick={() => test(channel)} disabled={busy}>
          {busy ? <Loader2 size={14} className="animate-spin" /> : <Send size={14} />} {busy ? "Sending…" : "Send test"}
        </Button>
        {res && (
          <div className={`rounded px-3 py-2 text-sm ${res.ok ? "bg-success/10 text-success" : "bg-error/10 text-error"}`}>
            {res.ok ? "✓ " : "✗ "}{res.msg}
          </div>
        )}
      </div>
    );
  };

  // Per-channel minimum-severity routing (F14). An explicit floor supersedes the
  // success/failure toggles; "Match toggles" (empty) keeps the legacy behavior.
  const events = (ch: "gotify" | "email" | "webhook") => {
    const min = cfg[ch].min_severity || "";
    return (
      <div>
        <Label>Minimum severity</Label>
        <div className="max-w-xs">
          <Select value={min} onChange={(e) => setCfg({ ...cfg, [ch]: { ...cfg[ch], min_severity: e.target.value } })}>
            <option value="">Match success/failure toggles</option>
            <option value="info">Info and up — everything, incl. successes</option>
            <option value="warning">Warning and up — alerts &amp; failures</option>
            <option value="critical">Critical only — data at risk</option>
          </Select>
        </div>
        {min === "" ? (
          <div className="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-sm">
            <label className="flex items-center gap-2"><input type="checkbox" checked={cfg[ch].on_failure} onChange={(e) => setCfg({ ...cfg, [ch]: { ...cfg[ch], on_failure: e.target.checked } })} /> On failure &amp; alerts</label>
            <label className="flex items-center gap-2"><input type="checkbox" checked={cfg[ch].on_success} onChange={(e) => setCfg({ ...cfg, [ch]: { ...cfg[ch], on_success: e.target.checked } })} /> On success</label>
          </div>
        ) : (
          <p className="mt-1 text-xs text-on-surface-variant">
            Sends {min === "info" ? "all events, including successes" : min === "warning" ? "warnings and critical alerts" : "critical alerts only"}. This overrides the success/failure toggles.
          </p>
        )}
        {/* F198: security events route through this same severity floor — no
            separate switch, because routing here is per-channel severity, not
            per-kind. Naming them is what tells an operator the channel now
            carries them. */}
        <p className="mt-1 text-xs text-on-surface-variant">Alerts include verification/scrub failure, a missed offsite copy, a destination over 90% full, a missed schedule, and the encryption key not being backed up — sent at high priority (repeated conditions are throttled).</p>
        <p className="mt-1 text-xs text-on-surface-variant">Security events use the same floor: a sign-in lockout and a master-key reveal are <span className="font-medium text-on-surface">critical</span>; repeated failed sign-ins and a failed re-authentication are <span className="font-medium text-on-surface">warnings</span>. Repeats from one address are throttled to one alert per lockout window.</p>
      </div>
    );
  };

  return (
    <Card className="p-5">
      <div className="mb-1 flex items-center gap-2 text-lg font-semibold"><Bell size={18} className="text-primary" /> Notifications</div>
      <p className="mb-4 text-sm text-on-surface-variant">Get told when a backup fails or — most importantly — when a backup completes but <b>fails verification</b>. No more finding out months later.</p>

      {/* Gotify */}
      <div className="mb-3 rounded border border-outline-variant bg-surface-lowest p-3">
        <label className="flex items-center gap-2 text-sm font-medium"><input type="checkbox" checked={cfg.gotify.enabled} onChange={(e) => setCfg({ ...cfg, gotify: { ...cfg.gotify, enabled: e.target.checked } })} /> Gotify</label>
        {cfg.gotify.enabled && (
          <div className="mt-2 space-y-2">
            <Label>Server URL</Label>
            <Input value={cfg.gotify.url} placeholder="https://gotify.example.com" onChange={(e) => setCfg({ ...cfg, gotify: { ...cfg.gotify, url: e.target.value } })} />
            <Label>App token {tokenSet && <span className="text-xs text-on-surface-variant">(stored — leave blank to keep)</span>}</Label>
            <Input type="password" value={cfg.gotify.token} placeholder={tokenSet ? "••••••••" : ""} onChange={(e) => setCfg({ ...cfg, gotify: { ...cfg.gotify, token: e.target.value } })} />
            {events("gotify")}
            {testBtn("gotify")}
          </div>
        )}
      </div>

      {/* Email */}
      <div className="mb-3 rounded border border-outline-variant bg-surface-lowest p-3">
        <label className="flex items-center gap-2 text-sm font-medium"><input type="checkbox" checked={cfg.email.enabled} onChange={(e) => setCfg({ ...cfg, email: { ...cfg.email, enabled: e.target.checked } })} /> Email (SMTP)</label>
        {cfg.email.enabled && (
          <div className="mt-2 space-y-2">
            <div className="grid grid-cols-3 gap-2">
              <div className="col-span-2"><Label>SMTP host</Label><Input value={cfg.email.host} placeholder="smtp.example.com" onChange={(e) => setCfg({ ...cfg, email: { ...cfg.email, host: e.target.value } })} /></div>
              <div><Label>Port</Label><Input type="number" value={cfg.email.port} onChange={(e) => setCfg({ ...cfg, email: { ...cfg.email, port: parseInt(e.target.value || "587", 10) } })} /></div>
            </div>
            <Label>Username</Label>
            <Input value={cfg.email.username} onChange={(e) => setCfg({ ...cfg, email: { ...cfg.email, username: e.target.value } })} />
            <Label>Password {pwSet && <span className="text-xs text-on-surface-variant">(stored — leave blank to keep)</span>}</Label>
            <Input type="password" value={cfg.email.password} placeholder={pwSet ? "••••••••" : ""} onChange={(e) => setCfg({ ...cfg, email: { ...cfg.email, password: e.target.value } })} />
            <div className="grid grid-cols-2 gap-2">
              <div><Label>From</Label><Input value={cfg.email.from} placeholder="dockback@example.com" onChange={(e) => setCfg({ ...cfg, email: { ...cfg.email, from: e.target.value } })} /></div>
              <div><Label>To</Label><Input value={cfg.email.to} placeholder="you@example.com" onChange={(e) => setCfg({ ...cfg, email: { ...cfg.email, to: e.target.value } })} /></div>
            </div>
            {events("email")}
            {testBtn("email")}
          </div>
        )}
      </div>

      {/* Webhook */}
      <div className="mb-3 rounded border border-outline-variant bg-surface-lowest p-3">
        <label className="flex items-center gap-2 text-sm font-medium"><input type="checkbox" checked={cfg.webhook.enabled} onChange={(e) => setCfg({ ...cfg, webhook: { ...cfg.webhook, enabled: e.target.checked } })} /> Webhook (Slack-compatible JSON)</label>
        {cfg.webhook.enabled && (
          <div className="mt-2 space-y-2">
            <Label>URL</Label>
            <Input value={cfg.webhook.url} placeholder="https://hooks.slack.com/services/…" onChange={(e) => setCfg({ ...cfg, webhook: { ...cfg.webhook, url: e.target.value } })} />
            {events("webhook")}
            {testBtn("webhook")}
          </div>
        )}
      </div>

      {/* Heartbeat / dead-man's-switch (PLAN §9.11) */}
      <div className="mb-3 rounded border border-outline-variant bg-surface-lowest p-3">
        <label className="flex items-center gap-2 text-sm font-medium"><input type="checkbox" checked={cfg.heartbeat.enabled} onChange={(e) => setCfg({ ...cfg, heartbeat: { ...cfg.heartbeat, enabled: e.target.checked } })} /> Heartbeat (dead-man's-switch)</label>
        <p className="mt-1 text-xs text-on-surface-variant">Pings an external monitor (healthchecks.io, Uptime Kuma, …) on every verified backup. If DockBack stops, the missing heartbeat alerts you — something in-app alerts can't do.</p>
        {cfg.heartbeat.enabled && (
          <div className="mt-2 space-y-2">
            <Label>Ping URL</Label>
            <Input value={cfg.heartbeat.url} placeholder="https://hc-ping.com/your-uuid" onChange={(e) => setCfg({ ...cfg, heartbeat: { ...cfg.heartbeat, url: e.target.value } })} />
            <Label>Keep-alive interval (minutes, 0 = only on verified backup)</Label>
            <Input type="number" min={0} value={cfg.heartbeat.interval_minutes} onChange={(e) => setCfg({ ...cfg, heartbeat: { ...cfg.heartbeat, interval_minutes: Math.max(0, parseInt(e.target.value || "0", 10)) } })} />
            {testBtn("heartbeat")}
          </div>
        )}
      </div>

      {/* F200: off-host audit checkpoint. Grouped with the heartbeat because
          both work the same way — their value is that they leave the machine. */}
      <div className="mb-3 rounded border border-outline-variant bg-surface-lowest p-3">
        <label className="flex items-center gap-2 text-sm font-medium"><input type="checkbox" checked={cfg.head_beacon?.enabled ?? false} onChange={(e) => setCfg({ ...cfg, head_beacon: { ...(cfg.head_beacon ?? { enabled: false, interval_hours: 24 }), enabled: e.target.checked } })} /> Audit checkpoint (tamper-evidence off this machine)</label>
        <p className="mt-1 text-xs text-on-surface-variant">Sends the audit trail&rsquo;s current head to your channels on a schedule. The built-in integrity check proves the trail is self-consistent — but anyone who reaches this container holds the key that signs it, so they could rewrite the log and its own records and still pass. A checkpoint sitting in your inbox is the one copy they can&rsquo;t edit: paste it into <span className="font-medium text-on-surface">Audit Trail &rarr; Verify against a checkpoint</span> to prove nothing was changed.</p>
        {(cfg.head_beacon?.enabled ?? false) && (
          <div className="mt-2 space-y-2">
            <Label>How often (hours)</Label>
            <Input type="number" min={1} max={168} value={cfg.head_beacon?.interval_hours ?? 24}
              onChange={(e) => setCfg({ ...cfg, head_beacon: { ...(cfg.head_beacon ?? { enabled: true, interval_hours: 24 }), interval_hours: Math.max(1, Math.min(168, parseInt(e.target.value || "24", 10))) } })} />
            <p className="text-xs text-on-surface-variant">Keep the messages. Each one attests to the trail as it stood at that moment, so the oldest one you still have sets how far back you can prove.</p>
          </div>
        )}
      </div>

      {/* Daily summary digest (F15) */}
      <div className="mb-3 rounded border border-outline-variant bg-surface-lowest p-3">
        <label className="flex items-center gap-2 text-sm font-medium"><input type="checkbox" checked={cfg.digest.enabled} onChange={(e) => setCfg({ ...cfg, digest: { ...cfg.digest, enabled: e.target.checked } })} /> Daily summary</label>
        <p className="mt-1 text-xs text-on-surface-variant">Once a day, get a single "last 24h" summary — backups run, verified, failed, plus destination and RPO status — instead of a ping per backup. Failures and critical alerts still notify immediately.</p>
        {cfg.digest.enabled && (
          <div className="mt-2 grid grid-cols-1 gap-3 sm:grid-cols-2">
            <div>
              <Label>Summary time</Label>
              <Input type="time" value={cfg.digest.time || "09:00"} onChange={(e) => setCfg({ ...cfg, digest: { ...cfg.digest, time: e.target.value } })} />
            </div>
            <div>
              <Label>Success notifications</Label>
              <Select value={cfg.digest.success_mode || "per_backup"} onChange={(e) => setCfg({ ...cfg, digest: { ...cfg.digest, success_mode: e.target.value } })}>
                <option value="per_backup">Per backup — a ping for each</option>
                <option value="digest">Daily summary — one message a day</option>
                <option value="both">Both — per backup and a daily summary</option>
              </Select>
            </div>
            <label className="flex items-start gap-2 text-sm sm:col-span-2">
              <input type="checkbox" className="mt-0.5" checked={cfg.digest.include_dr !== false} onChange={(e) => setCfg({ ...cfg, digest: { ...cfg.digest, include_dr: e.target.checked } })} />
              <span>Include DR confidence<span className="block text-xs text-on-surface-variant">Append a disaster-recovery line: how many stacks are drill-proven vs. undrilled, partial backups, whether the app-backup restore has been proven, and whether the master key is confirmed backed up.</span></span>
            </label>
          </div>
        )}
      </div>

    </Card>
  );
}
