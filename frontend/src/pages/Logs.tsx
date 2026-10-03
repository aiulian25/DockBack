// System Logs — live SSE stream in a terminal view + persistent Alerts inbox (F46).
import { useEffect, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { isConnected, subscribeConnection, subscribeLines } from "../lib/logStream";
import { Circle, Trash2, Loader2, Bell, Check, ShieldAlert, ShieldX } from "lucide-react";
import { Card } from "../components/ui";
import { useStickyScroll } from "../hooks/useStickyScroll";
import { api, Alert, fmtAgo } from "../api";
import { notifyAlertsChanged } from "../lib/alertsChanged";

interface Line { time: string; backup_id: string; node_id?: string; node_name?: string; level: string; msg: string; }

// Persisted fleet-wide activity sources (F46) — reserved logSink ids whose history
// survives a restart in the run_logs table under an "ops:" key.
const OPS_SOURCES = [
  { key: "schedule", label: "Scheduler" },
  { key: "queue", label: "Queue" },
  { key: "critical", label: "Critical" },
  { key: "notify", label: "Notifications" },
];

export default function Logs() {
  const [sp, setSp] = useSearchParams();
  const tab: "live" | "alerts" = sp.get("tab") === "alerts" ? "alerts" : "live";
  const setTab = (t: "live" | "alerts") => setSp(t === "alerts" ? { tab: "alerts" } : {}, { replace: true });

  const [lines, setLines] = useState<Line[]>([]);
  const [filter, setFilter] = useState<"ALL" | "INFO" | "WARN" | "ERR">("ALL");
  const [node, setNode] = useState("ALL"); // node filter (PLAN §4.13 node-keyed logs)
  const [live, setLive] = useState(false);
  // connecting = the stream hasn't opened or errored yet, so an empty view reads
  // as "connecting" rather than "connected but idle" or "disconnected".
  const [connecting, setConnecting] = useState(true);
  // F46: when a reserved source is chosen, show its PERSISTED history instead of the
  // live stream (same renderer). "" = the live stream (unchanged default behavior).
  const [histSource, setHistSource] = useState("");
  const [histLines, setHistLines] = useState<Line[]>([]);

  useEffect(() => {
    const stopLines = subscribeLines((l) => setLines((prev) => [...prev.slice(-2000), l as Line]));
    const stopStatus = subscribeConnection((up) => { setLive(up); setConnecting(false); });
    if (isConnected()) { setLive(true); setConnecting(false); }
    return () => { stopLines(); stopStatus(); };
  }, []);

  // Load a source's persisted activity when selected.
  useEffect(() => {
    if (!histSource) { setHistLines([]); return; }
    api.opsLog(histSource)
      .then((r) => setHistLines((r.lines || []).map((l) => ({ time: new Date(l.ts * 1000).toISOString(), backup_id: histSource, level: l.level, msg: l.msg }))))
      .catch(() => setHistLines([]));
  }, [histSource]);

  const sourceLines = histSource ? histLines : lines;
  // Distinct nodes seen in the LIVE stream (for the node filter; ops history has none).
  const nodeNames = [...new Set(sourceLines.map((l) => l.node_name).filter((n): n is string => !!n))].sort();
  const shown = sourceLines.filter((l) =>
    (filter === "ALL" || l.level === filter) &&
    (node === "ALL" || l.node_name === node)
  );
  const log = useStickyScroll(shown.length); // follow the newest line unless scrolled up
  const color = (lvl: string) => lvl === "ERR" ? "text-error" : lvl === "WARN" ? "text-warning" : "text-secondary";

  // --- Alerts inbox ---
  const [alerts, setAlerts] = useState<Alert[]>([]);
  const [alertSev, setAlertSev] = useState<"ALL" | "warning" | "critical">("ALL");
  // F92: filter by KIND as well as severity. Restore failures used to be
  // indistinguishable from overnight scrub regressions — both arrived as one
  // critical kind — so "show me what went wrong with restores" was impossible.
  const [alertKind, setAlertKind] = useState<string>("ALL");
  const [onlyUnacked, setOnlyUnacked] = useState(true);
  const [alertsLoading, setAlertsLoading] = useState(false);
  const loadAlerts = () => {
    setAlertsLoading(true);
    api.listAlerts({ unacked: onlyUnacked }).then((r) => setAlerts(r.alerts || [])).catch(() => setAlerts([])).finally(() => setAlertsLoading(false));
  };
  useEffect(() => { if (tab === "alerts") loadAlerts(); /* eslint-disable-next-line react-hooks/exhaustive-deps */ }, [tab, onlyUnacked]);
  // F229: the header's bell reads its own count and had no idea this happened, so
  // acknowledging everything left the badge showing alerts that were already
  // acknowledged until a route change made it re-read. Announced on both paths,
  // and after a failure too — the listener re-reads the real count rather than
  // adjusting a local one, so a wasted signal costs one cheap query and a missed
  // one leaves the badge wrong.
  const ack = async (id: number) => {
    await api.ackAlert(id).catch(() => {});
    notifyAlertsChanged();
    loadAlerts();
  };
  const ackAll = async () => {
    await api.ackAllAlerts().catch(() => {});
    notifyAlertsChanged();
    loadAlerts();
  };
  // Derived from the rows themselves, so a kind added server-side appears here
  // with no frontend change.
  const alertKinds = [...new Set(alerts.map((a) => a.kind).filter(Boolean))].sort();
  const shownAlerts = alerts.filter(
    (a) => (alertSev === "ALL" || a.severity === alertSev) && (alertKind === "ALL" || a.kind === alertKind),
  );

  return (
    <div>
      <div className="mb-4 flex flex-wrap items-center justify-between gap-2">
        <h1 className="text-2xl font-bold">System Logs</h1>
        {tab === "live" && (
          <div className="flex items-center gap-2">
            <span className="flex items-center gap-1.5 text-sm text-on-surface-variant">
              {histSource
                ? <>History · {OPS_SOURCES.find((o) => o.key === histSource)?.label}</>
                : connecting
                  ? <><Loader2 size={12} className="animate-spin text-on-surface-variant" /> Connecting…</>
                  : <><Circle size={9} className={live ? "fill-success text-success" : "fill-error text-error"} /> {live ? "Live" : "Disconnected"}</>}
            </span>
            <button onClick={() => setLines([])} className="flex items-center gap-1.5 rounded border border-outline-variant px-3 py-1.5 text-sm text-on-surface-variant hover:text-on-surface">
              <Trash2 size={14} /> Clear
            </button>
          </div>
        )}
      </div>

      {/* Tabs */}
      <div className="mb-4 flex gap-1 border-b border-outline-variant/60">
        {([["live", "Live log"], ["alerts", "Alerts"]] as const).map(([key, label]) => (
          <button key={key} onClick={() => setTab(key)}
            className={`-mb-px border-b-2 px-4 py-2 text-sm font-semibold ${tab === key ? "border-docker-blue text-primary" : "border-transparent text-on-surface-variant hover:text-on-surface"}`}>
            {label}
          </button>
        ))}
      </div>

      {tab === "live" ? (
        <>
          <div className="mb-3 flex flex-wrap items-center gap-3">
            <div className="flex gap-1">
              {(["ALL", "INFO", "WARN", "ERR"] as const).map((f) => (
                <button key={f} onClick={() => setFilter(f)}
                  className={`rounded px-3 py-1.5 text-xs font-semibold ${filter === f ? "bg-docker-blue/20 text-primary" : "text-on-surface-variant hover:bg-surface-high"}`}>
                  {f}
                </button>
              ))}
            </div>
            {nodeNames.length > 0 && (
              <select
                value={node} onChange={(e) => setNode(e.target.value)}
                className="rounded border border-outline-variant bg-surface-lowest px-3 py-1.5 text-xs text-on-surface-variant outline-none focus:border-docker-blue"
                title="Filter by node"
              >
                <option value="ALL">All nodes</option>
                {nodeNames.map((n) => <option key={n} value={n}>{n}</option>)}
              </select>
            )}
            <select
              value={histSource} onChange={(e) => setHistSource(e.target.value)}
              className="rounded border border-outline-variant bg-surface-lowest px-3 py-1.5 text-xs text-on-surface-variant outline-none focus:border-docker-blue"
              title="View persisted activity history for a fleet-wide source (survives restarts)"
            >
              <option value="">Live stream</option>
              {OPS_SOURCES.map((o) => <option key={o.key} value={o.key}>Activity: {o.label}</option>)}
            </select>
          </div>

          <Card className="overflow-hidden">
            <div className="flex items-center gap-2 border-b border-outline-variant/60 bg-surface-low px-4 py-2">
              <span className="h-3 w-3 rounded-full bg-error/70" /><span className="h-3 w-3 rounded-full bg-warning/70" /><span className="h-3 w-3 rounded-full bg-success/70" />
              <span className="ml-2 font-mono text-xs text-on-surface-variant">{histSource ? `dockback-activity ${histSource}` : "dockback-stream --follow"}</span>
              <span className="ml-auto font-mono text-xs text-on-surface-variant">Lines: {shown.length}</span>
            </div>
            <div
              ref={log.ref}
              onScroll={log.onScroll}
              className="h-[60vh] overflow-y-auto bg-surface-lowest p-4 font-mono text-xs leading-relaxed"
            >
              {shown.length === 0 && (
                histSource
                  ? <div className="text-on-surface-variant">No recorded activity for this source yet.</div>
                  : connecting
                    ? <div className="flex items-center gap-2 text-on-surface-variant"><Loader2 size={13} className="animate-spin" /> Connecting to the log stream…</div>
                    : !live
                      ? <div className="text-error">Disconnected from the log stream — attempting to reconnect…</div>
                      : <div className="text-on-surface-variant">Waiting for events… start a backup to see live output.</div>
              )}
              {shown.map((l, i) => (
                <div key={i} className="flex gap-3 py-0.5 hover:bg-white/5">
                  <span className="shrink-0 text-on-surface-variant">{l.time.replace("T", " ").replace("Z", "").replace(/\.\d+/, "")}</span>
                  <span className={`shrink-0 font-semibold ${color(l.level)}`}>[{l.level}]</span>
                  {l.node_name && <span className="shrink-0 rounded bg-surface-high/60 px-1.5 text-primary">{l.node_name}</span>}
                  {l.backup_id && !histSource && <span className="shrink-0 text-on-surface-variant/60">{l.backup_id.slice(0, 8)}</span>}
                  <span className="text-on-surface">{l.msg}</span>
                </div>
              ))}
            </div>
          </Card>
        </>
      ) : (
        <>
          <div className="mb-3 flex flex-wrap items-center gap-3">
            <div className="flex gap-1">
              {(["ALL", "warning", "critical"] as const).map((f) => (
                <button key={f} onClick={() => setAlertSev(f)}
                  className={`rounded px-3 py-1.5 text-xs font-semibold capitalize ${alertSev === f ? "bg-docker-blue/20 text-primary" : "text-on-surface-variant hover:bg-surface-high"}`}>
                  {f === "ALL" ? "All" : f}
                </button>
              ))}
            </div>
            {alertKinds.length > 1 && (
              <label className="flex items-center gap-2 text-sm text-on-surface-variant">
                <span className="shrink-0">Kind</span>
                <select
                  value={alertKind}
                  onChange={(e) => setAlertKind(e.target.value)}
                  className="min-w-0 max-w-[16rem] rounded border border-outline-variant bg-surface-lowest px-2 py-1 text-xs text-on-surface outline-none focus:border-docker-blue"
                >
                  <option value="ALL">All kinds</option>
                  {alertKinds.map((k) => <option key={k} value={k}>{k}</option>)}
                </select>
              </label>
            )}
            <label className="flex cursor-pointer items-center gap-2 text-sm text-on-surface-variant">
              <input type="checkbox" checked={onlyUnacked} onChange={(e) => setOnlyUnacked(e.target.checked)} /> Only unacknowledged
            </label>
            <button onClick={ackAll} disabled={shownAlerts.length === 0}
              className="ml-auto flex items-center gap-1.5 rounded border border-outline-variant px-3 py-1.5 text-sm text-on-surface-variant hover:text-on-surface disabled:opacity-50">
              <Check size={14} /> Acknowledge all
            </button>
          </div>

          <Card className="overflow-hidden">
            {alertsLoading && shownAlerts.length === 0 ? (
              <div className="flex items-center gap-2 p-6 text-sm text-on-surface-variant"><Loader2 size={14} className="animate-spin" /> Loading…</div>
            ) : shownAlerts.length === 0 ? (
              <div className="flex items-center gap-2 p-6 text-sm text-on-surface-variant"><Bell size={15} /> No alerts — all clear.</div>
            ) : (
              <ul className="divide-y divide-outline-variant/50">
                {shownAlerts.map((a) => (
                  <li key={a.id} className="flex flex-wrap items-start gap-x-3 gap-y-1 px-4 py-3">
                    <span className={`mt-0.5 flex shrink-0 items-center gap-1 rounded-full px-2 py-0.5 text-[11px] font-semibold ${a.severity === "critical" ? "bg-error/15 text-error" : "bg-warning/15 text-warning"}`}>
                      {a.severity === "critical" ? <ShieldX size={12} /> : <ShieldAlert size={12} />} {a.severity}
                    </span>
                    <div className="min-w-0 flex-1">
                      <div className="flex flex-wrap items-baseline gap-x-2">
                        <span className="font-medium text-on-surface">{a.title}</span>
                        {a.kind && <span className="shrink-0 font-mono text-[11px] text-outline">{a.kind}</span>}
                      </div>
                      <div className="break-words text-sm text-on-surface-variant">{a.message}</div>
                    </div>
                    <span className="mt-0.5 shrink-0 text-xs text-on-surface-variant">{fmtAgo(a.ts)}</span>
                    {a.acked
                      ? <span className="mt-0.5 flex shrink-0 items-center gap-1 text-xs text-success"><Check size={13} /> acknowledged</span>
                      : <button onClick={() => ack(a.id)} className="mt-0.5 flex shrink-0 items-center gap-1 rounded border border-outline-variant px-2 py-0.5 text-xs text-on-surface-variant hover:text-on-surface"><Check size={13} /> Acknowledge</button>}
                  </li>
                ))}
              </ul>
            )}
          </Card>
        </>
      )}
    </div>
  );
}
