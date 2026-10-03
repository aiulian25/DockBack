// App shell: 240px fixed sidebar + top bar, matching the hub(5) reference.
import { useEffect, useRef, useState, useCallback } from "react";
import { NavLink, useNavigate, Link, useLocation } from "react-router-dom";
import {
  LayoutDashboard, Server, CloudUpload, Terminal, Settings as Cog,
  FileText, Search, LogOut, ScrollText, Clock, Loader2, ExternalLink, BarChart3, LifeBuoy, Bell,
} from "lucide-react";
import { api, sessionGate } from "../api";
import { Button } from "./ui";
import KeyEscrowBanner from "./KeyEscrowBanner";
import VersionModal from "./VersionModal";
import CommandPalette from "./CommandPalette";
import QuickBackup from "./QuickBackup";
import { onAlertsChanged } from "../lib/alertsChanged";
import { usePoll } from "../hooks/usePoll";

const isMac = typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform);

// Show the expiry warning this many seconds before the session ends.
const WARN_BEFORE = 120;

function mmss(secs: number): string {
  const s = Math.max(0, secs);
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
}

// Resolve the browser-tab page name from the route, so each tab is identifiable
// (e.g. "Servers · DockBack") instead of a single static title.
function pageTitle(path: string): string {
  if (path === "/") return "Dashboard";
  if (path.startsWith("/servers/") && path.includes("/containers/")) return "Container";
  if (path.startsWith("/servers/")) return "Server";
  if (path.startsWith("/servers")) return "Servers";
  if (path.startsWith("/backups")) return "Backups";
  if (path.startsWith("/insights")) return "Insights";
  if (path.startsWith("/recovery")) return "Recovery";
  if (path.startsWith("/logs")) return "Logs";
  if (path.startsWith("/audit")) return "Audit Trail";
  if (path.startsWith("/settings")) return "Settings";
  if (path.startsWith("/docs")) return "Documentation";
  return "";
}

const nav = [
  { to: "/", label: "Dashboard", icon: LayoutDashboard, end: true },
  { to: "/servers", label: "Servers", icon: Server },
  { to: "/backups", label: "Backups", icon: CloudUpload },
  { to: "/insights", label: "Insights", icon: BarChart3 },
  { to: "/recovery", label: "Recovery", icon: LifeBuoy },
  { to: "/logs", label: "Logs", icon: Terminal },
  { to: "/audit", label: "Audit Trail", icon: ScrollText },
  { to: "/settings", label: "Settings", icon: Cog },
];

export default function Layout({ children }: { children: React.ReactNode }) {
  const navigate = useNavigate();
  const location = useLocation();
  // Logout is an SPA navigation (module state survives), so the once-per-load
  // session gate must be re-armed here (perf Fix 4).
  const logout = async () => { await api.logout().catch(() => {}); sessionGate.checked = false; navigate("/login"); };

  // Reflect the current page in the browser tab title.
  useEffect(() => {
    const t = pageTitle(location.pathname);
    document.title = t ? `${t} · DockBack` : "DockBack";
  }, [location.pathname]);

  // Real, build-stamped version (never hardcoded). "dev" for local builds.
  const [version, setVersion] = useState("");
  const [username, setUsername] = useState("");
  const [verOpen, setVerOpen] = useState(false);
  const [paletteOpen, setPaletteOpen] = useState(false);
  useEffect(() => { api.version().then((v) => setVersion(v.version)).catch(() => {}); }, []);

  // F46: unacknowledged-alert badge. Polled on a light interval (cheap COUNT) and
  // refreshed whenever the tab regains focus, so a warning that fired while you were
  // away shows up promptly without a heavy live subscription.
  const [alertCount, setAlertCount] = useState(0);
  const loadAlertCount = useCallback(() => api.alertsCount().then((r) => setAlertCount(r.unacked)).catch(() => {}), []);
  useEffect(() => {
    loadAlertCount();
    const onFocus = () => loadAlertCount();
    window.addEventListener("focus", onFocus);
    // F229: and immediately when alerts are acknowledged elsewhere in the app.
    // Without this the badge only caught up on the 30s poll, on focus, or on a
    // route change — so "Acknowledge all" appeared to do nothing until you left
    // the page, which is the one moment the operator is looking straight at it.
    const offAlerts = onAlertsChanged(loadAlertCount);
    return () => { window.removeEventListener("focus", onFocus); offAlerts(); };
  }, [location.pathname, loadAlertCount]);
  usePoll(loadAlertCount, 30000);

  // Global command-palette shortcuts: Cmd/Ctrl-K toggles anywhere; "/" opens it
  // only when focus isn't in a text field (so "/" still types inside inputs).
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.key === "k" || e.key === "K") && (e.metaKey || e.ctrlKey)) {
        e.preventDefault(); setPaletteOpen((o) => !o); return;
      }
      if (e.key === "/" && !paletteOpen) {
        const t = e.target as HTMLElement | null;
        const tag = t?.tagName;
        if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || t?.isContentEditable) return;
        e.preventDefault(); setPaletteOpen(true);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [paletteOpen]);

  // --- Session lifetime ---
  // Two independent deadlines, both enforced server-side: the absolute 12h
  // auto-logout (one-time 30-min extend) and a sliding 30-min idle timeout. The
  // idle window is slid only by GENUINE user interaction (a throttled activity
  // ping) — never by background polling — so an abandoned tab still times out.
  const [expiresAt, setExpiresAt] = useState<number | null>(null);         // unix s (absolute)
  const [idleExpiresAt, setIdleExpiresAt] = useState<number | null>(null); // unix s (idle)
  const [warnLeft, setWarnLeft] = useState<number | null>(null);           // seconds left, when warning
  const [warnReason, setWarnReason] = useState<"idle" | "absolute">("absolute");
  const [extending, setExtending] = useState(false);
  const [keepingAlive, setKeepingAlive] = useState(false);
  const [extendedOnce, setExtendedOnce] = useState(false);                 // one extension allowed
  const lastPing = useRef(0);

  useEffect(() => {
    api.me().then((m) => {
      if (m.username) setUsername(m.username);
      if (m.expires_at) setExpiresAt(m.expires_at);
      if (m.idle_expires_at) setIdleExpiresAt(m.idle_expires_at);
      setExtendedOnce(!!m.extended);
    }).catch(() => {});
  }, []);

  const doLogout = async (expired: boolean) => {
    await api.logout().catch(() => {});
    sessionGate.checked = false; // re-arm the once-per-load session gate (perf Fix 4)
    navigate(expired ? "/login?expired=1" : "/login");
  };

  // Report real interaction to the server (throttled to once / 60s) to slide the
  // idle window. Attached to DOM events only — background fetches never call it.
  useEffect(() => {
    const onActivity = () => {
      const t = Date.now();
      if (t - lastPing.current < 60000) return;
      lastPing.current = t;
      api.sessionActivity().then((r) => {
        setExpiresAt(r.expires_at); setIdleExpiresAt(r.idle_expires_at); setWarnLeft(null);
      }).catch(() => {});
    };
    const evs: (keyof WindowEventMap)[] = ["mousemove", "keydown", "click", "touchstart", "scroll"];
    evs.forEach((e) => window.addEventListener(e, onActivity, { passive: true }));
    return () => evs.forEach((e) => window.removeEventListener(e, onActivity));
  }, []);

  useEffect(() => {
    if (!expiresAt && !idleExpiresAt) return;
    const tick = () => {
      const nowS = Math.floor(Date.now() / 1000);
      const absLeft = expiresAt ? expiresAt - nowS : Infinity;
      const idleLeft = idleExpiresAt ? idleExpiresAt - nowS : Infinity;
      const left = Math.min(absLeft, idleLeft);
      if (left <= 0) { setWarnLeft(null); doLogout(true); return; }
      if (left <= WARN_BEFORE) { setWarnReason(idleLeft < absLeft ? "idle" : "absolute"); setWarnLeft(left); }
      else setWarnLeft(null);
    };
    tick();
    const iv = setInterval(tick, 1000);
    return () => clearInterval(iv);
  }, [expiresAt, idleExpiresAt]);

  const extendSession = async () => {
    setExtending(true);
    try { const r = await api.extendSession(); setExpiresAt(r.expires_at); setIdleExpiresAt(r.idle_expires_at); setExtendedOnce(true); setWarnLeft(null); }
    catch { setExtendedOnce(true); } // already extended (or failed) — keep the warning, force sign-out
    finally { setExtending(false); }
  };

  // "Stay signed in" from the inactivity warning — an explicit activity ping.
  const stayActive = async () => {
    setKeepingAlive(true);
    try { lastPing.current = Date.now(); const r = await api.sessionActivity(); setExpiresAt(r.expires_at); setIdleExpiresAt(r.idle_expires_at); setWarnLeft(null); }
    catch { doLogout(true); }
    finally { setKeepingAlive(false); }
  };

  return (
    <div className="flex h-full">
      {/* Sidebar */}
      <aside className="flex w-52 shrink-0 flex-col border-r border-outline-variant/50 bg-surface-low">
        <Link to="/" className="flex items-center gap-2.5 px-4 py-3.5 transition-opacity hover:opacity-80" title="Go to dashboard">
          <img src="/dockback-mark.png" alt="" className="h-8 w-8 shrink-0 rounded-md object-contain" />
          <div className="min-w-0">
            <div className="text-lg font-extrabold leading-tight tracking-tight text-primary">DockBack</div>
            <div className="text-[10px] uppercase tracking-wider text-on-surface-variant">Infrastructure</div>
          </div>
        </Link>
        <nav className="flex-1 space-y-0.5 px-2.5">
          {nav.map(({ to, label, icon: Icon, end }) => (
            <NavLink
              key={to} to={to} end={end}
              className={({ isActive }) =>
                `flex items-center gap-2.5 rounded px-2.5 py-2 text-sm font-medium transition-colors ${
                  isActive
                    ? "bg-docker-blue/15 text-primary ring-1 ring-inset ring-docker-blue/30"
                    : "text-on-surface-variant hover:bg-surface-high/50 hover:text-on-surface"
                }`
              }
            >
              <Icon size={16} strokeWidth={2} />
              {label}
            </NavLink>
          ))}
        </nav>
        {/* Version box — clickable; opens the version/releases modal. DockBack does
            not auto-check for updates (no telemetry/egress), so this is manual. */}
        <div className="px-2.5 pb-2">
          <button
            onClick={() => setVerOpen(true)}
            title="View version & releases"
            className="w-full rounded-lg border border-outline-variant/60 bg-surface-lowest px-3 py-2 text-left transition-colors hover:border-docker-blue/50 hover:bg-surface-high/40"
          >
            <div className="text-[10px] font-medium uppercase tracking-wider text-on-surface-variant">Version</div>
            <div className="flex items-center gap-1.5">
              <span className="tnum text-sm font-bold text-primary">{version || "…"}</span>
              <ExternalLink size={12} className="ml-auto text-on-surface-variant" />
            </div>
          </button>
        </div>

        <div className="px-2.5 pb-2.5">
          <QuickBackup />
        </div>
        <div className="space-y-0.5 border-t border-outline-variant/40 px-2.5 py-2.5 text-sm text-on-surface-variant">
          <button className="flex w-full items-center gap-2.5 rounded px-2.5 py-1.5 hover:text-on-surface" onClick={() => navigate("/docs")}>
            <FileText size={16} /> Docs
          </button>
          <button className="flex w-full items-center gap-2.5 rounded px-2.5 py-1.5 hover:text-on-surface" onClick={logout}>
            <LogOut size={16} /> Sign Out
          </button>
        </div>
      </aside>

      {/* Main */}
      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex items-center gap-4 border-b border-outline-variant/50 bg-surface-low/60 px-5 py-2">
          <button
            onClick={() => setPaletteOpen(true)}
            title="Search infrastructure (⌘K / Ctrl K)"
            className="group relative flex max-w-md flex-1 items-center gap-2 rounded border border-outline-variant bg-surface-lowest py-1.5 pl-3 pr-2 text-sm text-on-surface-variant transition-colors hover:border-docker-blue"
          >
            <Search size={15} className="shrink-0" />
            <span className="flex-1 text-left">Search infrastructure…</span>
            <kbd className="shrink-0 rounded border border-outline-variant/70 bg-surface-high px-1.5 py-0.5 font-mono text-[10px] text-on-surface-variant">{isMac ? "⌘K" : "Ctrl K"}</kbd>
          </button>
          <div className="ml-auto flex items-center gap-3 text-on-surface-variant">
            <button
              onClick={() => navigate("/logs?tab=alerts")}
              title={alertCount > 0 ? `${alertCount} unacknowledged alert${alertCount === 1 ? "" : "s"}` : "Alerts — all clear"}
              aria-label={alertCount > 0 ? `${alertCount} unacknowledged alerts` : "Alerts"}
              className="relative grid h-8 w-8 place-items-center rounded-full transition-colors hover:bg-surface-high/60 hover:text-on-surface"
            >
              <Bell size={17} className={alertCount > 0 ? "text-warning" : ""} />
              {alertCount > 0 && (
                <span className="absolute -right-0.5 -top-0.5 grid min-w-[16px] place-items-center rounded-full bg-warning px-1 text-[10px] font-bold leading-4 text-black">
                  {alertCount > 99 ? "99+" : alertCount}
                </span>
              )}
            </button>
            <div className="flex items-center gap-2">
              <div className="grid h-7 w-7 place-items-center rounded-full bg-docker-blue/20 text-xs font-bold text-primary">{(username || "Admin").charAt(0).toUpperCase()}</div>
              <span className="text-sm font-medium text-on-surface" title={username || "Admin"}>{username || "Admin"}</span>
            </div>
          </div>
        </header>
        <main className="flex-1 overflow-y-auto p-5">
          <div className="mx-auto max-w-container">
            <KeyEscrowBanner />
            {children}
          </div>
        </main>
      </div>

      {/* Command palette (Cmd/Ctrl-K) — replaces the old dead search box. */}
      <CommandPalette open={paletteOpen} onClose={() => setPaletteOpen(false)} />

      {/* Version / releases modal (manual — no auto update check). */}
      <VersionModal open={verOpen} version={version} onClose={() => setVerOpen(false)} />

      {/* Session warning: inactivity (sliding 30m) or absolute expiry (12h). */}
      {warnLeft !== null && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4">
          <div className="w-full max-w-sm rounded-lg border border-outline-variant bg-surface-high p-6 shadow-2xl">
            <div className="mb-2 flex items-center gap-2 text-lg font-semibold text-on-surface">
              <Clock size={18} className="text-warning" />
              {warnReason === "idle" ? "Inactivity sign-out" : "Session about to expire"}
            </div>
            <p className="text-sm text-on-surface-variant">
              {warnReason === "idle" ? "No activity detected — you'll be signed out in " : "You'll be signed out in "}
              <span className="tnum font-semibold text-on-surface">{mmss(warnLeft)}</span>.
              {" "}Any running backups will <span className="font-medium text-on-surface">continue in the background</span>.
            </p>
            {warnReason === "absolute" && extendedOnce && (
              <p className="mt-2 text-xs text-warning">
                This session was already extended once — sign in again to keep working.
              </p>
            )}
            <div className="mt-5 flex justify-end gap-2">
              <Button variant="ghost" onClick={() => doLogout(false)}>Sign out now</Button>
              {warnReason === "idle" ? (
                <Button variant="primary" disabled={keepingAlive} onClick={stayActive}>
                  {keepingAlive ? <Loader2 size={15} className="animate-spin" /> : <Clock size={15} />} Stay signed in
                </Button>
              ) : (!extendedOnce && (
                <Button variant="primary" disabled={extending} onClick={extendSession}>
                  {extending ? <Loader2 size={15} className="animate-spin" /> : <Clock size={15} />} Extend 30 min
                </Button>
              ))}
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
