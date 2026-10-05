// System Settings — Backup Policy (default destinations + retention), Scheduled
// Backups (one global schedule + node/container target picker), Encryption, and
// External Backup Destinations. Nothing hardcoded.
import { useCallback, useEffect, useMemo, useRef, useState, type ElementType } from "react";
import {
  KeyRound, HardDrive, History, ShieldAlert, Plus, Trash2, Server, FolderTree,
  Cloud, Database, CalendarClock, ChevronRight, ChevronDown, CloudUpload, Laptop, Lock, Loader2, LogOut,
  DatabaseBackup, Download, Upload, RotateCcw, Pencil, Palette, Check, AlertTriangle, X, Gauge, RefreshCw, ScanSearch, Search, Bell, Copy, Layers, ShieldCheck, Timer,
} from "lucide-react";
import { minPasswordLength, PASSWORD_LEN_FLOOR, PASSWORD_LEN_CEILING } from "../lib/passwordPolicy";
import { WEEKDAYS } from "../lib/format";
import ExportPresetsCard from "../components/ExportPresetsCard";
import { api, AdoptSkip, Destination, Policy, Schedule, Node, Container, AppBackup, AppDest, AppBackupSchedule, MigrateLayoutStatus, NamedSchedule, RetentionPreview, ScheduleTarget, ApiToken, SessionDevice, StepUpError, EgressAudit, fmtBytes, fmtAgo } from "../api";
import { Button, Card, Input, Label, Select, Modal } from "../components/ui";
import { useToast } from "../components/Toast";
import { THEMES, ThemeId, getTheme, setTheme } from "../theme";
import KeyEscrowModal from "../components/KeyEscrowModal";
import KeyRotateModal from "../components/KeyRotateModal";
import AddDestinationModal from "../components/AddDestinationModal";
import TwoFactorCard from "../components/TwoFactorCard";
import NotificationsCard from "../components/NotificationsCard";
import ClustersCard from "../components/ClustersCard";
import { SettingsSaveProvider, useRegisterSaver } from "../components/SettingsSave";
import StepUpPrompt from "../components/StepUpPrompt";
import { usePoll } from "../hooks/usePoll";

// Settings options that a single global Save persists (deferred, not instant).
const ENC_KEYS = ["manifest.encrypt", "verify.deep", "scrub.interval_days", "drill.interval_days", "drill.scope", "drill.per_cycle"];
// Performance & tuning knobs, formerly env-only, now settable in-app (F15).
const TUNE_KEYS = ["db.ready_timeout_seconds", "schedule.jitter_seconds", "upload.max_mbps", "backup.bind_skip_gib", "scrub.per_cycle", "alert.dest_full_pct", "alert.forecast_days", "backup.sidecar_image", "backup.max_concurrent", "backup.max_concurrent_per_node", "restore.health_timeout_seconds", "restore.test_clone_ttl_hours", "drill.boot_wait_seconds", "critical.rpo_min_seconds", "critical.tick_seconds", "critical.fail_limit", "alert.anomaly_factor", "retention.autosnap_keep", "backup.autotune_compression", "backup.synthetic_full", "tripwire.enabled", "tripwire.min_files", "tripwire.changed_pct", "tripwire.deleted_pct", "index.always", "backup.fail_on_corrupt_db"];

function destIcon(type: string) {
  if (type === "webdav" || type === "nextcloud") return Cloud;
  if (type === "s3") return Database;
  if (type === "synology") return Server;
  return FolderTree;
}

// AdoptSkipList (F102): what a Scan & adopt did NOT take, and why.
//
// "42 adopted, 7 skipped" is unactionable — after a key rotation or a
// two-instance mix-up the operator cannot tell whether those 7 are harmless
// duplicates or backups they can no longer read. Foreign-key skips lead, because
// they are the only ones that usually need a decision.
function AdoptSkipList({ skips, onDismiss }: { skips?: AdoptSkip[]; onDismiss: () => void }) {
  if (!skips || skips.length === 0) return null;
  const foreign = skips.filter((s) => s.reason === "foreign key");
  const rest = skips.filter((s) => s.reason !== "foreign key");
  const ordered = [...foreign, ...rest];
  const label = (reason: string) => {
    switch (reason) {
      case "foreign key": return "encrypted with a different key";
      case "already in catalog": return "already in the catalog";
      case "unreadable manifest": return "manifest couldn't be read";
      case "archive missing": return "archive file missing";
      case "catalog error": return "couldn't be written to the catalog";
      default: return reason;
    }
  };
  return (
    <div className="mt-3 rounded border border-outline-variant bg-surface-lowest p-3">
      <div className="mb-1 flex flex-wrap items-center gap-x-2 gap-y-1">
        <span className="text-xs font-medium">Skipped archives</span>
        <span className="text-[11px] text-on-surface-variant">{skips.length}</span>
        <button onClick={onDismiss} className="ml-auto text-[11px] text-on-surface-variant underline hover:text-on-surface">Dismiss</button>
      </div>
      {foreign.length > 0 && (
        <p className="mb-2 text-[11px] text-warning">
          {foreign.length} archive(s) belong to a different master key — re-import that key to adopt them, or check whether another DockBack writes to this destination.
        </p>
      )}
      <div className="max-h-56 space-y-1 overflow-y-auto">
        {ordered.map((sk, i) => (
          <div key={`${sk.key}-${i}`} className="rounded bg-surface-high/40 px-2 py-1 text-[11px]">
            <div className="break-all font-mono text-on-surface">{sk.key}</div>
            <div className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-on-surface-variant">
              <span className={sk.reason === "foreign key" ? "text-warning" : ""}>{label(sk.reason)}</span>
              {sk.key_fingerprint && <span className="break-all font-mono">key {sk.key_fingerprint}</span>}
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}

// DestForecastLine shows a destination's capacity trend + projected fill-up
// (PLAN §9.13), fed from real recorded history. Quiet until there's enough data.
function DestForecastLine({ d }: { d: Destination }) {
  if (d.history_points < 2) {
    return <div className="mt-2 text-[11px] text-on-surface-variant">Fill-up forecast: gathering history…</div>;
  }
  const growth = d.growth_bytes_per_month;
  if (d.days_to_full >= 0) {
    const when = new Date(d.fill_date * 1000).toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
    const tone = d.days_to_full <= 7 ? "text-error" : d.days_to_full <= 30 ? "text-warning" : "text-on-surface-variant";
    return <div className={`mt-2 text-[11px] ${tone}`}>Fills in ~{d.days_to_full}d (≈ {when}) · +{fmtBytes(growth)}/mo</div>;
  }
  if (growth > 0) return <div className="mt-2 text-[11px] text-on-surface-variant">Growing ≈ {fmtBytes(growth)}/mo</div>;
  return <div className="mt-2 text-[11px] text-on-surface-variant">Usage stable</div>;
}
const emptyPolicy: Policy = { destinations: [], generations: 3, autoprune: false, autoclean_missing_days: 0, schedule: { enabled: false, kind: "weekly", time: "03:00", weekday: 0, monthday: 1, cron: "0 3 * * 0", targets: [] }, prune_schedule: { enabled: false, kind: "weekly", time: "03:00", weekday: 0, monthday: 1, cron: "0 3 * * 0", targets: [] } };

// KeyBackupButton opens the encryption-key recovery flow on demand (re-download
// the recovery sheet at any time) — PLAN §9.2.
function KeyBackupButton() {
  const [open, setOpen] = useState(false);
  return (
    <>
      <Button variant="secondary" onClick={() => setOpen(true)}><KeyRound size={15} /> Back up encryption key</Button>
      <KeyEscrowModal open={open} onClose={() => setOpen(false)} />
    </>
  );
}

// KeyRotateButton opens the supervised master-key rotation flow (F16). On success
// it refreshes settings so the displayed key fingerprint updates immediately.
function KeyRotateButton({ onRotated }: { onRotated?: () => void }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <Button variant="secondary" onClick={() => setOpen(true)}><RefreshCw size={15} /> Rotate encryption key</Button>
      <KeyRotateModal open={open} onClose={() => setOpen(false)} onRotated={() => onRotated?.()} />
    </>
  );
}

// AppearanceCard lets the operator switch the UI theme at any time. The choice
// is applied instantly and persisted per-browser (PLAN §5).
function AppearanceCard() {
  const [theme, setActive] = useState<ThemeId>(getTheme());
  const choose = (id: ThemeId) => { setTheme(id); setActive(id); };
  return (
    <Card className="mb-5 p-5">
      <div className="mb-1 flex items-center gap-2 text-lg font-semibold"><Palette size={18} className="text-primary" /> Appearance</div>
      <p className="mb-4 text-sm text-on-surface-variant">Choose a theme. It applies instantly and is remembered on this device.</p>
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
        {THEMES.map((t) => {
          const active = t.id === theme;
          return (
            <button
              key={t.id}
              onClick={() => choose(t.id)}
              aria-pressed={active}
              className={`group relative overflow-hidden rounded-lg border p-3 text-left transition-all ${
                active ? "border-primary ring-2 ring-primary/40" : "border-outline-variant hover:border-primary/50"
              }`}
            >
              {/* Mini preview using the theme's own swatch colors */}
              <div className="mb-3 flex h-16 overflow-hidden rounded-md" style={{ background: t.swatch.bg }}>
                <div className="m-2 flex flex-1 flex-col justify-between rounded" style={{ background: t.swatch.card, border: `1px solid ${t.swatch.accent}33` }}>
                  <div className="mx-2 mt-2 h-1.5 w-1/2 rounded-full" style={{ background: t.swatch.accent }} />
                  <div className="mx-2 mb-2 h-1 w-3/4 rounded-full" style={{ background: t.swatch.text, opacity: 0.5 }} />
                </div>
              </div>
              <div className="flex items-center justify-between">
                <span className="text-sm font-semibold text-on-surface">{t.name}</span>
                {active && <span className="flex h-5 w-5 items-center justify-center rounded-full bg-primary text-on-primary"><Check size={13} /></span>}
              </div>
              <p className="mt-0.5 text-xs text-on-surface-variant">{t.description}</p>
            </button>
          );
        })}
      </div>
    </Card>
  );
}

// Settings groups, rendered as top tabs. ONLY the grouping changed — every card's
// content is identical to before; each just lives under the tab that fits it.
type SettingsTab = "general" | "backups" | "clusters" | "destinations" | "notifications" | "security" | "advanced";
const SETTINGS_TABS: { id: SettingsTab; label: string; icon: ElementType }[] = [
  { id: "backups", label: "Backups", icon: History },
  { id: "clusters", label: "Clusters", icon: Layers }, // F104
  { id: "destinations", label: "Destinations", icon: HardDrive },
  { id: "notifications", label: "Notifications", icon: Bell },
  { id: "security", label: "Security", icon: Lock },
  { id: "advanced", label: "Advanced", icon: Gauge },
  { id: "general", label: "General", icon: Palette },
];
// Open the tab named by ?tab=, or the Backups tab for the legacy #schedule anchor.
function settingsInitialTab(): SettingsTab {
  const q = new URLSearchParams(window.location.search).get("tab");
  if (q && SETTINGS_TABS.some((t) => t.id === q)) return q as SettingsTab;
  if (window.location.hash === "#schedule") return "backups";
  // F223: the recovery wizard's step 1 lands here. The card lives on Advanced,
  // so the anchor has to choose the tab as well as the scroll position — an
  // anchor pointing at something that is not in the DOM is a dead link.
  if (window.location.hash === "#app-backup") return "advanced";
  return "backups"; // open on a functional tab, not the cosmetic one
}

export default function Settings() {
  // The provider hosts the single global Save bar + unsaved-changes navigation
  // guard; every savable section registers with it (replaces per-section Saves).
  return (
    <SettingsSaveProvider>
      <SettingsInner />
    </SettingsSaveProvider>
  );
}

function SettingsInner() {
  const toast = useToast();
  const [tab, setTab] = useState<SettingsTab>(settingsInitialTab);
  // Legacy deep link: /settings#schedule lands on the Backups tab and scrolls to the
  // Schedules card (which is only in the DOM while that tab is active).
  useEffect(() => {
    if (tab === "backups" && window.location.hash === "#schedule") {
      document.getElementById("schedule")?.scrollIntoView({ block: "start" });
    }
    // F223: same for the app-backup card, which is only in the DOM on Advanced.
    if (tab === "advanced" && window.location.hash === "#app-backup") {
      document.getElementById("app-backup")?.scrollIntoView({ block: "start" });
    }
  }, [tab]);
  const [s, setS] = useState<Record<string, string>>({});
  const [sBaseline, setSBaseline] = useState<Record<string, string>>({}); // last-saved encryption opts
  // F39: egress allow-list — self-contained save + host test (not part of the
  // encryption/tuning auto-savers, since it applies live via its own control). The
  // textarea is one-per-line; the API stores/returns a comma-separated list.
  const egressBaseline = sBaseline["security.egress_allow"] ?? "";
  const minPasswordLen = useMemo(() => minPasswordLength(sBaseline["security.min_password_len"]), [sBaseline]);
  const [egressText, setEgressText] = useState("");
  useEffect(() => { setEgressText(egressBaseline.split(",").filter(Boolean).join("\n")); }, [egressBaseline]);
  const egressNormalized = egressText.split(/[\n,]+/).map((x) => x.trim()).filter(Boolean).join(",");
  const egressDirty = egressNormalized !== egressBaseline;
  const [egressSaving, setEgressSaving] = useState(false);
  const [egHost, setEgHost] = useState("");
  const [egTest, setEgTest] = useState<{ ok: boolean; error?: string } | null>(null);
  const [egTesting, setEgTesting] = useState(false);
  // F54: suggested allow-list hosts from what's already configured.
  const [egSuggest, setEgSuggest] = useState<{ host: string; source: string; allowed_now: boolean }[] | null>(null);
  const [egSuggestBusy, setEgSuggestBusy] = useState(false);
  const loadEgressSuggestions = async () => {
    setEgSuggestBusy(true);
    try { setEgSuggest(await api.egressSuggestions()); }
    catch (e) { toast.error(`Couldn't load suggestions: ${(e as Error).message}`); }
    finally { setEgSuggestBusy(false); }
  };
  // Append a host to the textarea only if it isn't already listed (case-insensitive).
  const addEgressHost = (host: string) => {
    setEgressText((cur) => {
      const has = cur.split(/[\n,]+/).map((x) => x.trim().toLowerCase()).includes(host.toLowerCase());
      if (has) return cur;
      const sep = cur && !cur.endsWith("\n") ? "\n" : "";
      return cur + sep + host;
    });
  };
  const egressListedHosts = new Set(egressText.split(/[\n,]+/).map((x) => x.trim().toLowerCase()).filter(Boolean));
  // F207: audit mode — the allow-list is evaluated and observed, never acted on,
  // so an operator can see what enforcement would break before it does.
  const [egAudit, setEgAudit] = useState<EgressAudit | null>(null);
  const [egAuditBusy, setEgAuditBusy] = useState(false);
  const loadEgressAudit = useCallback(async () => {
    try { setEgAudit(await api.egressAudit()); }
    catch { /* the panel simply stays as it was */ }
  }, []);
  useEffect(() => { if (tab === "security") loadEgressAudit(); }, [tab, loadEgressAudit]);
  const toggleEgressAudit = async (on: boolean) => {
    setEgAuditBusy(true);
    try {
      await api.setSettings({ "security.egress_audit": on ? "true" : "false" });
      setSBaseline((b) => ({ ...b, "security.egress_audit": on ? "true" : "false" }));
      await loadEgressAudit();
      toast.success(on
        ? "Audit mode on — the allow-list is being observed, not enforced"
        : "Audit mode off — the allow-list is enforced again");
    } catch (e) {
      toast.error(`Couldn't change audit mode: ${(e as Error).message}`);
    } finally { setEgAuditBusy(false); }
  };
  const clearEgressAudit = async () => {
    setEgAuditBusy(true);
    try { await api.clearEgressAudit(); await loadEgressAudit(); }
    catch (e) { toast.error(`Couldn't clear the audit: ${(e as Error).message}`); }
    finally { setEgAuditBusy(false); }
  };
  // Only hosts that belong to a CONFIGURED endpoint are offered in bulk. An
  // observation can arrive from a redirect or a DNS rebind — which is what the
  // dial-time guard exists to catch — so one-clicking every observed host into
  // the allow-list would let a redirect write itself into the security policy.
  const egAuditAddable = (egAudit?.entries || []).filter((e) => e.configured && !e.allowed_now && !egressListedHosts.has(e.host));
  const addAllAuditHosts = () => {
    egAuditAddable.forEach((e) => addEgressHost(e.host));
  };
  // F45: API tokens for automation.
  const [tokens, setTokens] = useState<ApiToken[]>([]);
  // metricsLocked (F65) mirrors the server's metrics_locked flag — kept in its
  // own state (not `s`) so refreshing it never clobbers unsaved settings edits.
  const [metricsLocked, setMetricsLocked] = useState<boolean | null>(null);
  const loadTokens = () =>
    Promise.all([api.listTokens(), api.getSettings()])
      .then(([t, v]) => { setTokens(t || []); setMetricsLocked(v["metrics_locked"] === "true"); })
      .catch(() => setTokens([]));
  const [tokModalOpen, setTokModalOpen] = useState(false);
  const [tokName, setTokName] = useState("");
  const [tokScopes, setTokScopes] = useState<Set<string>>(new Set(["read"]));
  const [tokTTL, setTokTTL] = useState(0); // F65: days until expiry, 0 = never
  // F201: optional source pin. Same free-text normalization as the egress list —
  // newlines or commas, whichever the operator pastes.
  const [tokCidrs, setTokCidrs] = useState("");
  const tokCidrList = tokCidrs.split(/[\n,]+/).map((x) => x.trim()).filter(Boolean);
  const [tokBusy, setTokBusy] = useState(false);
  const [newToken, setNewToken] = useState(""); // plaintext shown once after creation
  const [tokCopied, setTokCopied] = useState(false);
  // F64: minting a token is step-up gated — when the server asks, show an inline
  // password (+ TOTP) prompt in the modal and retry with the credentials.
  const [tokStepUp, setTokStepUp] = useState<{ totp: boolean; err: string } | null>(null);
  const toggleTokScope = (sc: string) => setTokScopes((prev) => { const n = new Set(prev); n.has(sc) ? n.delete(sc) : n.add(sc); return n; });
  const openTokenModal = () => { setTokName(""); setTokCidrs(""); setTokScopes(new Set(["read"])); setTokTTL(0); setNewToken(""); setTokCopied(false); setTokStepUp(null); setTokModalOpen(true); };
  const createToken = async (pw?: string, code?: string) => {
    if (!tokName.trim() || tokScopes.size === 0) return;
    setTokBusy(true);
    try {
      const r = await api.createToken(tokName.trim(), [...tokScopes], tokTTL, pw ? { password: pw, code } : undefined, tokCidrList);
      setTokStepUp(null);
      setNewToken(r.token);
      loadTokens();
    } catch (e) {
      if (e instanceof StepUpError) setTokStepUp({ totp: e.totp_required, err: pw ? e.message : "" });
      else toast.error(`Couldn't create token: ${(e as Error).message}`);
    } finally { setTokBusy(false); }
  };
  const deleteToken = async (t: ApiToken) => {
    if (!confirm(`Revoke token "${t.name}"? Any script or monitor using it will immediately stop working (401).`)) return;
    try { await api.deleteToken(t.id); toast.success("Token revoked"); loadTokens(); }
    catch (e) { toast.error(`Couldn't revoke token: ${(e as Error).message}`); }
  };
  useEffect(() => { loadTokens(); /* eslint-disable-next-line react-hooks/exhaustive-deps */ }, []);
  // F203 — sign-in policy. The session lifetime, idle window and password
  // minimum were compile-time constants; they are now settings, so the values
  // shown here are the EFFECTIVE ones (the in-app override when set, otherwise
  // the environment default), already clamped by the server.
  //
  // Saved with its own button rather than the shared save bar because it is
  // step-up gated: the request may come back asking for the password, and that
  // exchange belongs beside the fields that triggered it.
  const [polTTL, setPolTTL] = useState("12");
  const [polIdle, setPolIdle] = useState("30");
  const [polMinPw, setPolMinPw] = useState(String(PASSWORD_LEN_FLOOR));
  useEffect(() => {
    setPolTTL(sBaseline["security.session_ttl_hours"] ?? "12");
    setPolIdle(sBaseline["security.session_idle_minutes"] ?? "30");
    setPolMinPw(sBaseline["security.min_password_len"] ?? String(PASSWORD_LEN_FLOOR));
  }, [sBaseline]);
  const polPatch: Record<string, string> = {
    "security.session_ttl_hours": polTTL,
    "security.session_idle_minutes": polIdle,
    "security.min_password_len": polMinPw,
  };
  const polDirty = Object.entries(polPatch).some(([k, v]) => v !== (sBaseline[k] ?? ""));
  const [polBusy, setPolBusy] = useState(false);
  const [polStepUp, setPolStepUp] = useState<{ totp: boolean; err: string } | null>(null);
  const saveSignInPolicy = async (pw = "", code = "") => {
    setPolBusy(true);
    try {
      const patch = { ...polPatch };
      // The credentials ride along in the same request under reserved keys and
      // are stripped server-side before validation — never stored as settings.
      if (pw) { patch["__step_up_password"] = pw; patch["__step_up_code"] = code; }
      await api.setSettings(patch);
      // Re-read rather than assuming: the server clamps, so what was saved may
      // not be what was typed, and the field must show what is enforced.
      const v = await api.getSettings();
      setS(v); setSBaseline(v);
      setPolStepUp(null);
      toast.success("Sign-in policy saved");
    } catch (e) {
      if (e instanceof StepUpError) setPolStepUp({ totp: e.totp_required, err: pw ? e.message : "" });
      else toast.error(`Couldn't save sign-in policy: ${(e as Error).message}`);
    } finally { setPolBusy(false); }
  };

  const saveEgress = async () => {
    setEgressSaving(true);
    try {
      await api.setSettings({ "security.egress_allow": egressNormalized });
      setSBaseline((b) => ({ ...b, "security.egress_allow": egressNormalized }));
      setEgTest(null);
      toast.success(egressNormalized ? "Egress allow-list applied" : "Egress allow-list cleared — using the environment default");
    } catch (e) {
      toast.error(`Couldn't save allow-list: ${(e as Error).message}`);
    } finally {
      setEgressSaving(false);
    }
  };
  const testEgress = async () => {
    if (!egHost.trim()) return;
    setEgTesting(true); setEgTest(null);
    try { setEgTest(await api.egressTest(egHost.trim())); }
    catch (e) { setEgTest({ ok: false, error: (e as Error).message }); }
    finally { setEgTesting(false); }
  };
  const [dests, setDests] = useState<Destination[]>([]);
  const [pendingDest, setPendingDest] = useState<Set<string>>(new Set()); // soft-removed destinations (undo)
  const [policy, setPolicy] = useState<Policy>(emptyPolicy);
  const [policyBaseline, setPolicyBaseline] = useState<Policy>(emptyPolicy); // last-saved policy
  const [nextRun, setNextRun] = useState(0);
  const [pruneNextRun, setPruneNextRun] = useState(0);
  const [lastRun, setLastRun] = useState(0);
  const [caughtUp, setCaughtUp] = useState(false);
  // Named backup schedules (F6): a list of independent schedules replacing the
  // single global one. Drafts (not yet saved) carry id "".
  const [scheds, setScheds] = useState<NamedSchedule[]>([]);
  const [schedsBaseline, setSchedsBaseline] = useState<NamedSchedule[]>([]);
  const [runningId, setRunningId] = useState("");
  const loadScheds = () => api.schedules().then((r) => { setScheds(r); setSchedsBaseline(r); return r; }).catch(() => [] as NamedSchedule[]);
  const [nodes, setNodes] = useState<Node[]>([]);
  const [preview, setPreview] = useState<RetentionPreview | null>(null);
  // F77: layout-migration progress (polled while the backups tab is visible).
  const [mig, setMig] = useState<MigrateLayoutStatus | null>(null);
  const [migStarting, setMigStarting] = useState(false);
  usePoll(() => { if (tab === "backups") api.migrateLayoutStatus().then(setMig).catch(() => {}); }, 5000, [tab]);
  const [pruning, setPruning] = useState(false);
  const [addOpen, setAddOpen] = useState(false);
  const [editDest, setEditDest] = useState<Destination | null>(null);
  const [runMsg, setRunMsg] = useState("");
  const [running, setRunning] = useState(false);
  // Account password change.
  const [curPw, setCurPw] = useState("");
  const [newPw, setNewPw] = useState("");
  const [confPw, setConfPw] = useState("");
  const [pwBusy, setPwBusy] = useState(false);
  const [pwMsg, setPwMsg] = useState<{ ok: boolean; text: string } | null>(null);
  // Sign out other sessions/devices.
  const [soBusy, setSoBusy] = useState(false);
  const [soMsg, setSoMsg] = useState<{ ok: boolean; text: string } | null>(null);
  // Application backup & restore.
  const [abInfo, setAbInfo] = useState<{ key_fingerprint: string; db_bytes: number; last_drill_at: number; last_drill_ok: boolean; last_drill_detail: string; drill_interval_days: number } | null>(null);
  const [drilling, setDrilling] = useState(false);
  const drillAppBackup = async () => {
    setDrilling(true); setAbMsg(null);
    try {
      const r = await api.appBackupDrill();
      setAbMsg({ ok: r.ok, text: r.ok ? "Application backup proven — it restores cleanly." : `App-backup drill failed: ${r.detail}` });
      api.appBackupInfo().then(setAbInfo).catch(() => {});
    } catch (e) { setAbMsg({ ok: false, text: (e as Error).message }); }
    finally { setDrilling(false); }
  };
  const [abList, setAbList] = useState<AppBackup[]>([]);
  const [abBusy, setAbBusy] = useState(false);
  const [abMsg, setAbMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const restoreRef = useRef<HTMLInputElement>(null);
  const loadAppBackups = () => api.appBackupList().then(setAbList).catch(() => {});
  // App-backup external destinations.
  const [appDests, setAppDests] = useState<AppDest[]>([]);
  const [addAppOpen, setAddAppOpen] = useState(false);
  const [extBusy, setExtBusy] = useState(false);
  const [extMsg, setExtMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const [browse, setBrowse] = useState<{ id: string; items: { key: string; name: string }[] } | null>(null);
  const loadAppDests = () => api.appDestinations().then(setAppDests).catch(() => {});
  // Automatic application backup schedule (F4).
  const emptyAbSched: AppBackupSchedule = { enabled: false, kind: "weekly", time: "04:00", weekday: 0, monthday: 1, keep: 7, push_external: false };
  const [abSched, setAbSched] = useState<AppBackupSchedule>(emptyAbSched);
  const [abSchedBaseline, setAbSchedBaseline] = useState<AppBackupSchedule>(emptyAbSched);
  const [abNextRun, setAbNextRun] = useState(0);
  const loadAbSched = () => api.getAppBackupSchedule().then((r) => { setAbSched(r.schedule); setAbSchedBaseline(r.schedule); setAbNextRun(r.next_run); }).catch(() => {});
  const setAbSchedField = (patch: Partial<AppBackupSchedule>) => setAbSched((a) => ({ ...a, ...patch }));

  const loadDests = () => api.destinations().then(setDests).catch(() => setDests([]));
  // Destinations minus any pending soft-removal, so the 30s poll can't resurrect
  // a card the user just removed within its undo window (A3).
  const shownDests = dests.filter((d) => !pendingDest.has(d.id));
  const loadPolicy = () => api.getPolicy().then((r) => { setPolicy(r.policy); setPolicyBaseline(r.policy); setNextRun(r.next_run); setPruneNextRun(r.prune_next_run || 0); setLastRun(r.last_run || 0); setCaughtUp(!!r.last_caught_up); }).catch(() => {});
  useEffect(() => {
    api.getSettings().then((v) => { setS(v); setSBaseline(v); }).catch(() => {});
    api.nodes().then(setNodes).catch(() => {});
    loadDests(); loadPolicy(); loadScheds();
    api.appBackupInfo().then(setAbInfo).catch(() => {});
    loadAppBackups();
    loadAppDests();
    loadAbSched();
    // Gentle refresh so a destination going down (or recovering) surfaces on its
    // status dot without a manual reload. Probes are bounded + concurrent server-side.
  }, []);
  usePoll(loadDests, 30000);

  // Deep-link from the container page's "Schedule in Settings" shortcut.
  useEffect(() => {
    if (window.location.hash === "#schedule") {
      setTimeout(() => document.getElementById("schedule")?.scrollIntoView({ behavior: "smooth", block: "start" }), 150);
    }
  }, []);

  // Stable savers (read latest state via refs) so the global save bar only
  // re-registers on dirty transitions, not on every keystroke.
  const policyRef = useRef(policy); policyRef.current = policy;
  const sRef = useRef(s); sRef.current = s;
  const abSchedRef = useRef(abSched); abSchedRef.current = abSched;
  const schedsRef = useRef(scheds); schedsRef.current = scheds;
  const schedsBaseRef = useRef(schedsBaseline); schedsBaseRef.current = schedsBaseline;
  // Baseline refs for the Discard handlers (revert each section to its last-saved state).
  const policyBaseRef = useRef(policyBaseline); policyBaseRef.current = policyBaseline;
  const sBaseRef = useRef(sBaseline); sBaseRef.current = sBaseline;
  const abSchedBaseRef = useRef(abSchedBaseline); abSchedBaseRef.current = abSchedBaseline;

  const savePolicy = useCallback(async () => {
    const r = await api.setPolicy(policyRef.current);
    setPolicy(r.policy); setPolicyBaseline(r.policy); setNextRun(r.next_run); setPruneNextRun(r.prune_next_run || 0);
    setPreview(null); // policy changed — any prior preview is stale
  }, []);
  const saveEncryption = useCallback(async () => {
    const patch: Record<string, string> = {};
    for (const k of ENC_KEYS) patch[k] = sRef.current[k] ?? "";
    await api.setSettings(patch);
    setSBaseline((prev) => ({ ...prev, ...patch }));
  }, []);

  const saveAbSched = useCallback(async () => {
    const r = await api.setAppBackupSchedule(abSchedRef.current);
    setAbSched(r.schedule); setAbSchedBaseline(r.schedule); setAbNextRun(r.next_run);
  }, []);

  // Performance & tuning knobs (F15) — persisted through /api/settings, validated
  // and clamped server-side, some applied live and some on next restart.
  const saveTuning = useCallback(async () => {
    const patch: Record<string, string> = {};
    for (const k of TUNE_KEYS) patch[k] = sRef.current[k] ?? "";
    await api.setSettings(patch);
    setSBaseline((prev) => ({ ...prev, ...patch }));
  }, []);

  // Persist the schedule list as a diff against the last-saved baseline: rows
  // gone from the list are deleted, drafts (id "") are created, and changed rows
  // are updated. Then reload so ids/next-run reflect the server.
  const persistScheds = useCallback(async (): Promise<NamedSchedule[]> => {
    const cur = schedsRef.current, base = schedsBaseRef.current;
    const curIds = new Set(cur.filter((x) => x.id).map((x) => x.id));
    for (const b of base) if (b.id && !curIds.has(b.id)) await api.deleteSchedule(b.id);
    for (const sc of cur) {
      if (!sc.id) { await api.addSchedule(sc); continue; }
      const b = base.find((x) => x.id === sc.id);
      if (!b || JSON.stringify(sc) !== JSON.stringify(b)) await api.updateSchedule(sc.id, sc);
    }
    return await loadScheds();
  }, []);
  const saveScheds = useCallback(async () => { await persistScheds(); }, [persistScheds]);

  // Stable Discard handlers — revert each section to its last-saved baseline. The
  // two settings-map sections (encryption, tuning) only reset THEIR keys, so
  // discarding one never touches the other's edits.
  const resetPolicy = useCallback(() => setPolicy(policyBaseRef.current), []);
  const resetAbSched = useCallback(() => setAbSched(abSchedBaseRef.current), []);
  const resetScheds = useCallback(() => setScheds(schedsBaseRef.current), []);
  const resetEncryption = useCallback(() => setS((prev) => { const n = { ...prev }; for (const k of ENC_KEYS) n[k] = sBaseRef.current[k] ?? ""; return n; }), []);
  const resetTuning = useCallback(() => setS((prev) => { const n = { ...prev }; for (const k of TUNE_KEYS) n[k] = sBaseRef.current[k] ?? ""; return n; }), []);

  const policyDirty = JSON.stringify(policy) !== JSON.stringify(policyBaseline);
  const encDirty = ENC_KEYS.some((k) => (s[k] ?? "") !== (sBaseline[k] ?? ""));
  const abSchedDirty = JSON.stringify(abSched) !== JSON.stringify(abSchedBaseline);
  const schedsDirty = JSON.stringify(scheds) !== JSON.stringify(schedsBaseline);
  const tuningDirty = TUNE_KEYS.some((k) => (s[k] ?? "") !== (sBaseline[k] ?? ""));
  useRegisterSaver("policy", policyDirty, savePolicy, resetPolicy);
  useRegisterSaver("encryption", encDirty, saveEncryption, resetEncryption);
  useRegisterSaver("appbackup-schedule", abSchedDirty, saveAbSched, resetAbSched);
  useRegisterSaver("schedules", schedsDirty, saveScheds, resetScheds);
  useRegisterSaver("tuning", tuningDirty, saveTuning, resetTuning);

  // Dry-run preview of what retention would prune (PLAN §4.6). Persists the
  // on-screen policy first (via the shared saver) so the preview is accurate.
  const previewPrune = async () => {
    setPruning(true);
    try { await savePolicy(); setPreview(await api.retentionPreview()); }
    catch (e) { alert((e as Error).message); }
    finally { setPruning(false); }
  };
  const runPrune = async () => {
    if (!preview || !confirm(`Permanently delete ${preview.total_prune} backup(s) (freeing ${fmtBytes(preview.total_prune_bytes)}) from all their locations? This cannot be undone.`)) return;
    setPruning(true);
    try { await api.retentionPrune(); setPreview(await api.retentionPreview()); }
    catch (e) { alert((e as Error).message); }
    finally { setPruning(false); }
  };
  // F20: scan a destination for orphaned .dback archives and re-import them into the
  // catalog (recovery after a DB loss). Long-running, so it shows a spinner.
  const [adoptingId, setAdoptingId] = useState<string | null>(null);
  // F102: the per-archive skip report, kept per destination so it stays visible
  // under the card that produced it instead of vanishing with the toast.
  const [adoptSkips, setAdoptSkips] = useState<Record<string, AdoptSkip[]>>({});
  const adoptDest = async (d: Destination) => {
    setAdoptingId(d.id);
    try {
      const r = await api.adoptDestination(d.id);
      setAdoptSkips((m) => ({ ...m, [d.id]: r.skips || [] }));
      if (r.adopted > 0) toast.success(`${r.adopted} backup(s) adopted, ${r.skipped} skipped`);
      else toast.info(`No new backups found — ${r.skipped} already in the catalog or not for this key`);
    } catch (e) {
      toast.error(`Scan failed: ${(e as Error).message}`);
    } finally {
      setAdoptingId(null);
    }
  };
  // F51: destination backfill — mirror all existing history missing from a dest.
  type BackfillSt = { total: number; done: number; failed: number; running: boolean };
  const [backfills, setBackfills] = useState<Record<string, BackfillSt>>({});
  const pollBackfill = async (id: string) => {
    try { const st = await api.backfillStatus(id); setBackfills((p) => ({ ...p, [id]: st })); return st.running; }
    catch { return false; }
  };
  // Hydrate statuses when the dest list loads, so an in-progress backfill keeps showing.
  useEffect(() => { dests.forEach((d) => pollBackfill(d.id)); /* eslint-disable-next-line react-hooks/exhaustive-deps */ }, [dests.length]);
  // Poll running backfills every 5s.
  usePoll(() => { Object.entries(backfills).forEach(([id, b]) => { if (b.running) pollBackfill(id); }); },
    Object.values(backfills).some((b) => b.running) ? 5000 : null);
  const startBackfill = async (d: Destination) => {
    if (!confirm(`Backfill “${d.name}”?\n\nCopies every existing backup that has no healthy copy here. Runs one upload at a time and honors this destination's upload window.`)) return;
    try {
      const r = await api.backfillDestination(d.id);
      if (r.candidates === 0) { toast.success(`${d.name} already has every backup — nothing to backfill.`); return; }
      setBackfills((p) => ({ ...p, [d.id]: { total: r.candidates, done: 0, failed: 0, running: true } }));
      toast.success(`Backfill started — ${r.candidates} backup(s) queued.`);
    } catch (e) {
      const msg = (e as Error).message;
      toast.error(/already running/.test(msg) ? "A backfill is already running for this destination." : `Couldn't start backfill: ${msg}`);
    }
  };
  // Soft remove with a 5s Undo (A3): the destination card hides immediately and
  // deleteDestination is deferred. Existing backups mirrored there are untouched.
  const removeDest = (d: Destination) => {
    setPendingDest((p) => new Set(p).add(d.id));
    toast.undo({
      message: `Removed destination "${d.name}"`,
      onUndo: () => setPendingDest((p) => { const n = new Set(p); n.delete(d.id); return n; }),
      onCommit: async () => {
        try { await api.deleteDestination(d.id); }
        catch (e) { toast.error(`Couldn't remove destination: ${(e as Error).message}`); }
        finally { setPendingDest((p) => { const n = new Set(p); n.delete(d.id); return n; }); loadDests(); }
      },
    });
  };

  const changePw = async () => {
    setPwMsg(null);
    if (newPw.length < minPasswordLen) { setPwMsg({ ok: false, text: `New password must be at least ${minPasswordLen} characters.` }); return; }
    if (newPw !== confPw) { setPwMsg({ ok: false, text: "New passwords don't match." }); return; }
    setPwBusy(true);
    try {
      const r = await api.changePassword(curPw, newPw);
      setCurPw(""); setNewPw(""); setConfPw("");
      const extra = r.revoked_other_sessions > 0 ? ` ${r.revoked_other_sessions} other session(s) signed out.` : "";
      setPwMsg({ ok: true, text: "Password changed." + extra });
    } catch (e) { setPwMsg({ ok: false, text: (e as Error).message }); }
    finally { setPwBusy(false); }
  };

  // F202: the signed-in devices. Loaded when the security tab is shown rather
  // than on every Settings mount, so opening Settings for an unrelated reason
  // costs nothing.
  const [devices, setDevices] = useState<SessionDevice[] | null>(null);
  const [devBusy, setDevBusy] = useState("");
  const loadDevices = useCallback(() => {
    api.listSessions().then((r) => setDevices(r.sessions)).catch(() => setDevices([]));
  }, []);
  useEffect(() => { if (tab === "security") loadDevices(); }, [tab, loadDevices]);
  const revokeDevice = async (d: SessionDevice) => {
    setDevBusy(d.id); setSoMsg(null);
    try {
      await api.revokeSession(d.id);
      setSoMsg({ ok: true, text: `Signed out ${d.device}${d.ip ? ` (${d.ip})` : ""}.` });
      loadDevices();
    } catch (e) { setSoMsg({ ok: false, text: (e as Error).message }); }
    finally { setDevBusy(""); }
  };

  const signOutOthers = async () => {
    setSoMsg(null); setSoBusy(true);
    try {
      const r = await api.revokeOtherSessions();
      setSoMsg({ ok: true, text: r.revoked > 0 ? `Signed out ${r.revoked} other session(s).` : "No other active sessions." });
    } catch (e) { setSoMsg({ ok: false, text: (e as Error).message }); }
    finally { setSoBusy(false); }
  };

  // F228: how many sessions are NOT this browser. One session is the normal
  // state and says nothing; two or more is the state worth noticing, and it is
  // the only one this page marks.
  const otherDevices = (devices || []).filter((d) => !d.current).length;

  // "expires in 11h" — the one thing SessionDevice already carried that the old
  // footnote-sized layout had nowhere to put.
  const fmtUntil = (ts: number) => {
    const s = Math.max(0, ts - Math.floor(Date.now() / 1000));
    if (s < 60) return "under a minute";
    if (s < 3600) return `${Math.floor(s / 60)}m`;
    if (s < 86400) return `${Math.floor(s / 3600)}h`;
    return `${Math.floor(s / 86400)}d`;
  };

  const fmtWhen = (ts: number) => new Date(ts * 1000).toLocaleString();
  const RESTORE_WARN =
    "This REPLACES all current DockBack data — nodes, the backup catalog, settings, " +
    "destinations and 2FA — with the backup's contents, then restarts the app. You'll need " +
    "to sign in again. The same encryption key the backup was made with is required.";

  const createBackup = async () => {
    setAbBusy(true); setAbMsg(null);
    try {
      await api.appBackupCreate();
      setAbMsg({ ok: true, text: "Backup created." });
      loadAppBackups();
      api.appBackupInfo().then(setAbInfo).catch(() => {});
    } catch (err) { setAbMsg({ ok: false, text: (err as Error).message }); }
    finally { setAbBusy(false); }
  };

  // F199: the control-plane archive is step-up gated like the backup exports.
  // It is encrypted with the master key, so on its own it opens nothing — but it
  // is every sealed credential the app holds in one file, and whoever takes it
  // now only needs the key later.
  // One prompt is shown at a time; `kind` says which flow is waiting for it and
  // carries what that flow needs to retry. Restoring is gated as well as
  // downloading (F199): it replaces every credential the app holds and restarts.
  type AbStepUp =
    | { kind: "download"; file: string; totp: boolean; err: string }
    | { kind: "restore-local"; file: string; totp: boolean; err: string }
    | { kind: "restore-upload"; upload: File; totp: boolean; err: string }
    | { kind: "restore-external"; destId: string; name: string; totp: boolean; err: string };
  const [abStepUp, setAbStepUp] = useState<AbStepUp | null>(null);

  // Every gated flow reacts to a refusal the same way: raise the prompt the
  // first time, and show the server's reason once a password has been tried.
  const stepUpRetry = (e: unknown, password: string | undefined, prompt: (totp: boolean, err: string) => AbStepUp, fail: (msg: string) => void) => {
    if (e instanceof StepUpError) { setAbStepUp(prompt(e.totp_required, password ? e.message : "")); return; }
    fail((e as Error).message);
  };

  const downloadBackup = async (b: AppBackup, password?: string, code?: string) => {
    try {
      const { ticket } = await api.appBackupExportGrant(password, code);
      setAbStepUp(null);
      const a = document.createElement("a");
      a.href = api.appBackupDownloadUrl(b.file, ticket); a.rel = "noopener";
      document.body.appendChild(a); a.click(); a.remove();
    } catch (e) {
      stepUpRetry(e, password, (totp, err) => ({ kind: "download", file: b.file, totp, err }),
        (msg) => setAbMsg({ ok: false, text: msg }));
    }
  };

  const deleteBackup = async (b: AppBackup) => {
    if (!confirm(`Delete the backup from ${fmtWhen(b.created_at)}? This cannot be undone.`)) return;
    try { await api.appBackupDelete(b.file); loadAppBackups(); }
    catch (err) { setAbMsg({ ok: false, text: (err as Error).message }); }
  };

  const afterRestoreStaged = () => {
    setAbMsg({ ok: true, text: "Restore staged — the app is restarting. Redirecting you to sign in…" });
    setTimeout(() => { window.location.href = "/login"; }, 6000);
  };

  const restoreLocal = async (b: AppBackup, password?: string, code?: string) => {
    if (!password && !confirm(`Restore from the backup taken ${fmtWhen(b.created_at)}?\n\n${RESTORE_WARN}`)) return;
    setAbBusy(true); setAbMsg(null);
    try { await api.appBackupRestoreLocal(b.file, { password, code }); setAbStepUp(null); afterRestoreStaged(); }
    catch (err) {
      stepUpRetry(err, password, (totp, msg) => ({ kind: "restore-local", file: b.file, totp, err: msg }),
        (msg) => setAbMsg({ ok: false, text: msg }));
      setAbBusy(false);
    }
  };

  // The chosen file is kept so the retry after the prompt uploads the same one —
  // an <input type="file"> cannot be re-populated programmatically.
  const restoreUpload = async (file: File, password?: string, code?: string) => {
    setAbBusy(true); setAbMsg(null);
    try { await api.appBackupRestore(file, { password, code }); setAbStepUp(null); afterRestoreStaged(); }
    catch (err) {
      stepUpRetry(err, password, (totp, msg) => ({ kind: "restore-upload", upload: file, totp, err: msg }),
        (msg) => setAbMsg({ ok: false, text: msg }));
      setAbBusy(false);
    }
  };

  const onRestoreFile = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    e.target.value = ""; // allow re-selecting the same file
    if (!file) return;
    if (!confirm(`Restore from "${file.name}"?\n\n${RESTORE_WARN}`)) return;
    await restoreUpload(file);
  };

  const backupExternal = async () => {
    setExtBusy(true); setExtMsg(null);
    try {
      const r = await api.appExternalBackup();
      const fails = Object.keys(r.failed || {}).length;
      setExtMsg(fails === 0
        ? { ok: true, text: `Uploaded to ${r.pushed.length} destination(s).` }
        : { ok: false, text: `Uploaded to ${r.pushed.length}; ${fails} failed (${Object.values(r.failed).join("; ")}).` });
      if (browse) browseDest({ id: browse.id } as AppDest, true);
    } catch (err) { setExtMsg({ ok: false, text: (err as Error).message }); }
    finally { setExtBusy(false); }
  };

  const toggleAppDest = async (d: AppDest, enabled: boolean) => {
    try { await api.toggleAppDestination(d.id, enabled); loadAppDests(); } catch (err) { setExtMsg({ ok: false, text: (err as Error).message }); }
  };

  const removeAppDest = async (d: AppDest) => {
    if (!confirm(`Remove external destination "${d.name}"? Backups already uploaded there are left in place.`)) return;
    try { await api.deleteAppDestination(d.id); if (browse?.id === d.id) setBrowse(null); loadAppDests(); }
    catch (err) { setExtMsg({ ok: false, text: (err as Error).message }); }
  };

  const browseDest = async (d: AppDest, force = false) => {
    if (!force && browse?.id === d.id) { setBrowse(null); return; }
    setExtMsg(null);
    try { const items = await api.appExternalList(d.id); setBrowse({ id: d.id, items }); }
    catch (err) { setExtMsg({ ok: false, text: (err as Error).message }); }
  };

  const restoreExternal = async (d: AppDest, name: string, password?: string, code?: string) => {
    if (!password && !confirm(`Restore "${name}" from "${d.name}"?\n\n${RESTORE_WARN}`)) return;
    setAbBusy(true); setExtMsg(null);
    try { await api.appExternalRestore(d.id, name, { password, code }); setAbStepUp(null); afterRestoreStaged(); }
    catch (err) {
      stepUpRetry(err, password, (totp, msg) => ({ kind: "restore-external", destId: d.id, name, totp, err: msg }),
        (msg) => setExtMsg({ ok: false, text: msg }));
      setAbBusy(false);
    }
  };

  // ---- Named schedules (F6) ----
  const blankSchedule = (n: number): NamedSchedule => ({
    id: "", name: `Schedule ${n}`, enabled: true, kind: "weekly", time: "03:00",
    weekday: 0, monthday: 1, cron: "0 3 * * 0", targets: [], include_stopped: false,
  });
  const addScheduleDraft = () => setScheds((list) => [...list, blankSchedule(list.length + 1)]);
  const patchSched = (idx: number, patch: Partial<NamedSchedule>) =>
    setScheds((list) => list.map((sc, i) => (i === idx ? { ...sc, ...patch } : sc)));
  const removeSched = (idx: number) => setScheds((list) => list.filter((_, i) => i !== idx));

  // Persist any edits first, then run this one schedule now. A draft (unsaved id)
  // is created by the save, so we re-read its id from the reloaded list by name.
  const runSchedNow = async (idx: number) => {
    const sc = schedsRef.current[idx];
    setRunningId(sc.id || `draft-${idx}`); setRunMsg("");
    try {
      const fresh = await persistScheds();
      const saved = fresh.find((x) => x.id === sc.id) || fresh.find((x) => x.name === sc.name);
      if (!saved?.id) throw new Error("could not resolve saved schedule");
      await api.runScheduleById(saved.id);
      const n = (sc.targets ?? []).length;
      const dest = sc.destinations_explicit ? ((sc.destinations ?? []).length ? "this schedule's destinations" : "local only") : "default destinations";
      setRunMsg(`"${sc.name}" started for ${n} target${n === 1 ? "" : "s"} → ${dest}. Watch Logs.`);
    } catch (e) { setRunMsg((e as Error).message); }
    finally { setRunningId(""); }
  };

  const toggleDefaultDest = (id: string, on: boolean) =>
    setPolicy((p) => ({ ...p, destinations: on ? [...p.destinations, id] : p.destinations.filter((x) => x !== id) }));

  return (
    <div>
      <h1 className="text-2xl font-bold">System Settings</h1>
      <p className="mb-6 mt-1 text-on-surface-variant">Default backup policy, schedule, encryption and storage.</p>

      {/* Grouped settings tabs — selecting a tab shows only that group's cards. */}
      <div role="tablist" className="mb-6 flex gap-1 overflow-x-auto border-b border-outline-variant">
        {SETTINGS_TABS.map((t) => {
          const Icon = t.icon;
          const active = tab === t.id;
          return (
            <button
              key={t.id}
              role="tab"
              aria-selected={active}
              onClick={() => setTab(t.id)}
              className={`relative flex items-center gap-2 whitespace-nowrap rounded-t-lg px-4 py-2.5 text-sm font-semibold transition-colors ${active ? "text-primary" : "text-on-surface-variant hover:bg-surface-high/40 hover:text-on-surface"}`}
            >
              <Icon size={16} /> {t.label}
              {active && <span className="absolute inset-x-2 -bottom-px h-0.5 rounded bg-primary" />}
            </button>
          );
        })}
      </div>

      {/* Each card renders only when its tab (selected above) is active. Card
          contents are unchanged — this container just stacks them full-width. */}
      <div className="space-y-5">
        {/* Appearance / theme picker (PLAN §5) */}
        {tab === "general" && <AppearanceCard />}

        {/* Cluster registry + the cluster tier of the policy chain (F104). */}
        {tab === "clusters" && <ClustersCard dests={dests} />}

        {/* Backup Policy */}
        {tab === "backups" && (
        <Card className="p-5">
          <div className="mb-4 flex items-center gap-2 text-lg font-semibold"><History size={18} className="text-primary" /> Backup Policy</div>

          <Label>Default destinations</Label>
          <div className="mb-4 space-y-2">
            <div className="flex items-center gap-3 rounded border border-outline-variant bg-surface-lowest px-3 py-2 text-sm text-on-surface-variant">
              <input type="checkbox" checked disabled /> <HardDrive size={15} /> Local <span className="ml-auto text-xs">always</span>
            </div>
            {shownDests.map((d) => (
              <label key={d.id} className="flex cursor-pointer items-center gap-3 rounded border border-outline-variant bg-surface-lowest px-3 py-2 text-sm">
                <input type="checkbox" checked={policy.destinations.includes(d.id)} onChange={(e) => toggleDefaultDest(d.id, e.target.checked)} />
                <CloudUpload size={15} className="text-secondary" /> {d.name}
                <span className="ml-auto text-xs uppercase text-on-surface-variant">{d.type}</span>
              </label>
            ))}
            {dests.length === 0 && <div className="text-xs text-on-surface-variant">No external destinations yet — add one below.</div>}
          </div>

          <Label>Copies to keep (per container)</Label>
          <Input type="number" min={0} value={policy.generations} onChange={(e) => setPolicy({ ...policy, generations: parseInt(e.target.value || "0", 10) })} />

          <Label>GFS retention (keep newest per period)</Label>
          <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
            <div>
              <Input type="number" min={0} value={policy.keep_daily ?? 0} onChange={(e) => setPolicy({ ...policy, keep_daily: parseInt(e.target.value || "0", 10) })} />
              <span className="mt-0.5 block text-center text-xs text-on-surface-variant">daily</span>
            </div>
            <div>
              <Input type="number" min={0} value={policy.keep_weekly ?? 0} onChange={(e) => setPolicy({ ...policy, keep_weekly: parseInt(e.target.value || "0", 10) })} />
              <span className="mt-0.5 block text-center text-xs text-on-surface-variant">weekly</span>
            </div>
            <div>
              <Input type="number" min={0} value={policy.keep_monthly ?? 0} onChange={(e) => setPolicy({ ...policy, keep_monthly: parseInt(e.target.value || "0", 10) })} />
              <span className="mt-0.5 block text-center text-xs text-on-surface-variant">monthly</span>
            </div>
            <div>
              <Input type="number" min={0} value={policy.keep_yearly ?? 0} onChange={(e) => setPolicy({ ...policy, keep_yearly: parseInt(e.target.value || "0", 10) })} />
              <span className="mt-0.5 block text-center text-xs text-on-surface-variant">yearly</span>
            </div>
          </div>
          <p className="mt-1 text-xs italic text-on-surface-variant">Keeps the newest backup of each of the most recent N days / weeks / months (0 = off). Combined with “copies to keep”; the newest backup is always kept.</p>

          <Label>Keep newest auto-snapshots</Label>
          <Input type="number" min={0} max={50} className="max-w-[8rem]"
            value={s["retention.autosnap_keep"] ?? "3"}
            onChange={(e) => setS({ ...s, "retention.autosnap_keep": String(Math.min(50, Math.max(0, parseInt(e.target.value || "0", 10)))) })} />
          <p className="mt-1 text-xs italic text-on-surface-variant">Pre-change, crash and pre-restore snapshots (labels starting with “auto:”) keep this many per target, separate from the rules above — so a flappy container, an update night, or repeated restores can’t crowd out your scheduled history. 0 = treat them like normal backups.</p>

          <label className="my-3 flex items-center gap-3 text-sm">
            <input type="checkbox" checked={policy.autoprune} onChange={(e) => setPolicy({ ...policy, autoprune: e.target.checked })} />
            Auto-prune after each backup (applies this policy automatically on every location)
          </label>

          {(() => {
            const ps = policy.prune_schedule ?? emptyPolicy.prune_schedule!;
            const setPrune = (patch: Partial<Schedule>) => setPolicy({ ...policy, prune_schedule: { ...ps, ...patch } });
            return (
              <div className="mb-3 rounded border border-outline-variant/50 bg-surface-lowest p-3">
                <label className="flex items-center gap-2 text-sm font-medium">
                  <input type="checkbox" checked={ps.enabled} onChange={(e) => setPrune({ enabled: e.target.checked })} />
                  Prune on a schedule
                </label>
                <p className="mb-2 mt-1 text-xs text-on-surface-variant">Runs the retention policy fleet-wide on this cadence, even for containers not currently being backed up.</p>
                {ps.enabled && (
                  <>
                    <div className="grid grid-cols-1 gap-2 sm:grid-cols-3">
                      <div>
                        <Label>Frequency</Label>
                        <Select value={ps.kind} onChange={(e) => setPrune({ kind: e.target.value })}>
                          <option value="daily">Daily</option>
                          <option value="weekly">Weekly</option>
                          <option value="monthly">Monthly</option>
                        </Select>
                      </div>
                      <div><Label>Time</Label><Input type="time" value={ps.time} onChange={(e) => setPrune({ time: e.target.value })} /></div>
                      {ps.kind === "weekly" && (
                        <div>
                          <Label>Day of week</Label>
                          <Select value={ps.weekday} onChange={(e) => setPrune({ weekday: parseInt(e.target.value, 10) })}>
                            {WEEKDAYS.map((d, i) => <option key={i} value={i}>{d}</option>)}
                          </Select>
                        </div>
                      )}
                      {ps.kind === "monthly" && (
                        <div><Label>Day of month (1–28)</Label><Input type="number" min={1} max={28} value={ps.monthday} onChange={(e) => setPrune({ monthday: parseInt(e.target.value || "1", 10) })} /></div>
                      )}
                    </div>
                    <div className="mt-2 text-xs text-on-surface-variant">Next prune: <span className="text-on-surface">{pruneNextRun ? new Date(pruneNextRun * 1000).toLocaleString() : "—"}</span></div>
                  </>
                )}
              </div>
            );
          })()}

          <div className="flex flex-wrap gap-2">
            <Button onClick={previewPrune} disabled={pruning}>{pruning ? "…" : "Preview pruning (dry run)"}</Button>
          </div>

          {preview && (
            <div className="mt-3 rounded border border-outline-variant bg-surface-lowest p-3 text-sm">
              {preview.total_prune === 0 ? (
                <div className="text-success">Nothing to prune — current backups fit the policy.</div>
              ) : (
                <>
                  <div className="mb-2 text-warning">
                    Would delete <b>{preview.total_prune}</b> backup(s), freeing <b>{fmtBytes(preview.total_prune_bytes)}</b>, across {preview.targets.filter((t) => t.prune > 0).length} container(s).
                  </div>
                  <div className="max-h-40 overflow-y-auto text-xs">
                    {preview.targets.filter((t) => t.prune > 0).map((t, i) => {
                      const pins = t.items.filter((it) => it.pinned).length;
                      const autoPruned = t.items.filter((it) => it.action === "prune" && (it.label || "").startsWith("auto:")).length;
                      return (
                        <div key={i} className="flex flex-wrap items-center justify-between gap-x-2 border-b border-outline-variant/30 py-1">
                          <span className="flex items-center gap-1.5 font-mono">
                            {t.node_name || t.node_id} · {t.target}
                            {autoPruned > 0 && <span className="rounded-full bg-secondary/15 px-1.5 py-0.5 font-sans text-[10px] font-semibold text-secondary" title="automatic pre-change, crash and pre-restore snapshots pruned by their own budget">{autoPruned} auto</span>}
                          </span>
                          <span className="text-on-surface-variant">keep {t.keep}{pins > 0 ? ` (${pins} pinned)` : ""} · prune {t.prune} ({fmtBytes(t.prune_bytes)})</span>
                        </div>
                      );
                    })}
                  </div>
                  <Button variant="danger" className="mt-3" onClick={runPrune} disabled={pruning}>{pruning ? "Pruning…" : `Prune now (delete ${preview.total_prune})`}</Button>
                </>
              )}
            </div>
          )}

          {/* F77: one-shot migration of legacy flat-layout archives into the
              per-stack folder layout, on every location. Safe to re-run. */}
          <div className="mt-4 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <div className="flex items-center gap-2 font-medium"><FolderTree size={15} className="text-primary" /> Organize existing backups</div>
              <Button
                variant="secondary"
                disabled={mig?.running || migStarting || (mig ? !mig.running && mig.remaining === 0 : false)}
                onClick={async () => {
                  setMigStarting(true);
                  try { await api.migrateLayout(); setMig(await api.migrateLayoutStatus()); }
                  catch (e) { toast.error(`Couldn't start: ${(e as Error).message}`); }
                  finally { setMigStarting(false); }
                }}
              >
                {mig?.running || migStarting ? <Loader2 size={15} className="animate-spin" /> : <FolderTree size={15} />} {mig?.running ? "Organizing…" : "Organize now"}
              </Button>
            </div>
            <p className="mt-1 text-xs text-on-surface-variant">
              Moves old archives into the per-stack folder layout on every destination. Safe to re-run. Copies are verified before anything is deleted; immutable (WORM) copies can't be moved and are skipped — they age out naturally.
            </p>
            {mig && (
              <p className="mt-1.5 text-xs tnum text-on-surface-variant">
                {mig.running
                  ? <>Working — {mig.moved} moved · {mig.skipped_worm} skipped (immutable) · {mig.failed} failed · {mig.remaining} remaining</>
                  : mig.remaining > 0
                    ? <>{mig.remaining} backup{mig.remaining === 1 ? "" : "s"} still use the old flat layout.</>
                    : mig.total > 0
                      ? <>Done — {mig.moved} moved · {mig.skipped_worm} skipped (immutable) · {mig.failed} failed.</>
                      : <>All backups already use the per-stack layout.</>}
              </p>
            )}
          </div>
        </Card>
        )}

        {/* Encryption */}
        {tab === "security" && (
        <Card className="p-5">
          <div className="mb-4 flex items-center gap-2 text-lg font-semibold"><KeyRound size={18} className="text-primary" /> Encryption</div>
          <dl className="space-y-2 text-sm">
            <div className="flex justify-between"><dt className="text-on-surface-variant">Algorithm</dt><dd>AES-256-GCM (chunked)</dd></div>
            <div className="flex justify-between"><dt className="text-on-surface-variant">Key fingerprint</dt><dd className="font-mono text-xs">{s["key_fingerprint"] || "—"}</dd></div>
            <div className="flex items-center gap-2"><HardDrive size={15} className="text-on-surface-variant" /><span className="font-mono text-xs">{s["storage"] || "—"}</span></div>
          </dl>
          <div className="mt-4 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm">
            <div className="flex flex-wrap gap-2">
              <KeyBackupButton />
              <KeyRotateButton onRotated={() => api.getSettings().then((v) => { setS(v); setSBaseline(v); }).catch(() => {})} />
            </div>
            <p className="mt-1.5 text-xs text-on-surface-variant">Lose this key and every backup is permanently unrecoverable. Save it offline. <b>Rotate</b> swaps in a new key and re-wraps existing backups in place — back up the new key first.</p>
          </div>
          <label className="mt-4 flex cursor-pointer items-start gap-3 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm">
            <input
              type="checkbox"
              className="mt-0.5"
              checked={s["manifest.encrypt"] === "true"}
              onChange={(e) => setS({ ...s, "manifest.encrypt": e.target.checked ? "true" : "false" })}
            />
            <span>
              Encrypt backup manifests
              <span className="mt-0.5 block text-xs text-on-surface-variant">Hides volume/DB names and image digests in the copy stored beside each archive. Off keeps a readable, signed manifest for hand-restore. Applies to new backups.</span>
            </span>
          </label>
          <label className="mt-2 flex cursor-pointer items-start gap-3 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm">
            <input
              type="checkbox"
              className="mt-0.5"
              checked={s["verify.deep"] === "true"}
              onChange={(e) => setS({ ...s, "verify.deep": e.target.checked ? "true" : "false" })}
            />
            <span>
              Deep verification (test-restore DB dumps)
              <span className="mt-0.5 block text-xs text-on-surface-variant">Beyond the always-on integrity check, re-imports each <b>database dump</b> into a throwaway, isolated container and runs a sanity query — proving the dump actually restores. Applies to <b>database containers</b>; app/file-volume backups have no dump to test-restore (the verification report shows it as <i>nothing to test-restore</i>). Heavier (spins a DB container per dump); a failed test-restore marks the backup Unverified.</span>
            </span>
          </label>
          <div className="mt-2 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm">
            <div className="flex items-center justify-between gap-3">
              <span className="font-medium">Periodic re-verification (scrub)</span>
              <span className="flex items-center gap-1.5">
                <Input type="number" className="!w-20 text-right"
                  value={s["scrub.interval_days"] ?? "0"}
                  onChange={(e) => setS({ ...s, "scrub.interval_days": String(Math.max(0, parseInt(e.target.value || "0", 10))) })} />
                <span className="text-xs text-on-surface-variant">days</span>
              </span>
            </div>
            <p className="mt-1 text-xs text-on-surface-variant">Re-reads stored backups on this cadence and re-checks their integrity (ciphertext hash + decrypt) — catching <b>bit-rot</b> or a destination that silently went bad. A backup that no longer verifies is flagged and alerts. <b>0 = off.</b> You can also <b>Verify now</b> on any backup.</p>
          </div>
          <div className="mt-2 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm">
            <div className="flex items-center justify-between gap-3">
              <span className="font-medium">Restore drills</span>
              <span className="flex items-center gap-1.5">
                <Input type="number" className="!w-20 text-right"
                  value={s["drill.interval_days"] ?? "0"}
                  onChange={(e) => setS({ ...s, "drill.interval_days": String(Math.max(0, parseInt(e.target.value || "0", 10))) })} />
                <span className="text-xs text-on-surface-variant">days</span>
              </span>
            </div>
            <div className="mt-3 grid grid-cols-1 gap-3 sm:grid-cols-2">
              <div>
                <Label>Drill coverage</Label>
                <Select value={s["drill.scope"] || "newest"} onChange={(e) => setS({ ...s, "drill.scope": e.target.value })}>
                  <option value="newest">Newest backup only</option>
                  <option value="newest_per_week">Newest of each week</option>
                </Select>
              </div>
              <div>
                <Label>Drills per cycle</Label>
                <Input type="number" min={1} max={5} className="!w-20 text-right"
                  value={s["drill.per_cycle"] || "1"}
                  onChange={(e) => setS({ ...s, "drill.per_cycle": String(Math.min(5, Math.max(1, parseInt(e.target.value || "1", 10)))) })} />
              </div>
            </div>
            <p className="mt-2 text-xs text-on-surface-variant">On this cadence, DockBack test-restores backups into an isolated sandbox (throwaway database + archive extract) and records a <b>pass/fail</b> — real proof the data restores, not just that it decrypts. <b>Coverage</b> chooses whether to drill only each container's newest backup or also the newest of each week (proving older generations still restore); <b>drills per cycle</b> (1–5) tunes throughput to your hardware. Runs spread load across cycles. <b>0 days = off.</b> Per-backup results and a <b>Run drill</b> button live on the <b>Backups</b> page.</p>
          </div>
          <div className="mt-4 flex items-start gap-2 rounded bg-warning/10 px-3 py-2 text-sm text-warning">
            <ShieldAlert size={16} className="mt-0.5 shrink-0" />
            Back up your encryption key offline. If it is lost, every backup becomes unrecoverable.
          </div>
        </Card>
        )}

        {/* Egress allow-list (F39) — the outbound default-deny control, live-editable. */}
        {tab === "security" && (
        <Card className="p-5">
          <div className="mb-1 flex flex-wrap items-center gap-2 text-lg font-semibold">
            <Lock size={18} className="text-primary" /> Egress allow-list
            {/* F207: while auditing, the list is NOT being enforced. Saying
                "Active" here would be untrue in the one state where it matters. */}
            {egAudit?.audit_mode && egressNormalized
              ? <span className="rounded-full bg-warning/15 px-2 py-0.5 text-xs font-medium text-warning">Audit mode — observed, not enforced</span>
              : egressNormalized
              ? <span className="rounded-full bg-success/15 px-2 py-0.5 text-xs font-medium text-success">Active — outbound hosts not listed are refused</span>
              : <span className="rounded-full bg-surface-highest px-2 py-0.5 text-xs font-medium text-on-surface-variant">Disabled — any operator-configured host is allowed</span>}
          </div>
          <p className="mb-3 text-xs text-on-surface-variant">
            A belt-and-suspenders control: when set, DockBack refuses any outbound connection (backup destinations, notification endpoints) to a host not on this list — enforced when a destination is added and again at the real TCP dial. Leave empty to allow every operator-configured host. Edits apply <b>live</b>, no restart. The <code className="font-mono">DOCKBACK_EGRESS_ALLOW</code> environment value is the boot default; a value here overrides it.
          </p>
          <textarea
            value={egressText}
            onChange={(e) => setEgressText(e.target.value)}
            rows={5}
            spellCheck={false}
            placeholder={"One per line: example.com, *.example.com, 203.0.113.10, 203.0.113.0/24"}
            className="w-full rounded border border-outline-variant bg-surface-lowest px-3 py-2 font-mono text-xs outline-none focus:border-primary"
          />
          <p className="mt-1 text-xs text-on-surface-variant">Accepted forms — one per line or comma-separated: <span className="font-mono">example.com</span> (exact host), <span className="font-mono">*.example.com</span> (domain + subdomains), <span className="font-mono">203.0.113.10</span> (IP), <span className="font-mono">203.0.113.0/24</span> (CIDR). A pasted URL or host:port is tolerated.</p>
          <div className="mt-3 flex flex-wrap items-center gap-2">
            <Button variant="primary" onClick={saveEgress} disabled={egressSaving || !egressDirty}>
              {egressSaving ? <Loader2 size={15} className="animate-spin" /> : <Check size={15} />} {egressNormalized ? "Apply allow-list" : "Clear allow-list"}
            </Button>
            <Button variant="secondary" onClick={loadEgressSuggestions} disabled={egSuggestBusy}>
              {egSuggestBusy ? <Loader2 size={15} className="animate-spin" /> : <ScanSearch size={15} />} Suggest from current config
            </Button>
          </div>
          {egSuggest && (
            <div className="mt-3 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5">
              <div className="mb-1.5 text-xs font-medium text-on-surface-variant">
                Hosts DockBack is configured to reach {egSuggest.length > 0 ? <>({egSuggest.length})</> : null}
              </div>
              {egSuggest.length === 0 ? (
                <p className="text-xs text-on-surface-variant">No outbound destinations, notification endpoints, or remote nodes are configured yet.</p>
              ) : (
                <ul className="flex flex-col gap-1.5">
                  {egSuggest.map((sug) => {
                    const listed = egressListedHosts.has(sug.host.toLowerCase());
                    return (
                      <li key={sug.host} className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs">
                        <span className="font-mono text-on-surface">{sug.host}</span>
                        <span className="rounded-full bg-surface-highest px-1.5 py-0.5 text-[11px] text-on-surface-variant">{sug.source}</span>
                        {sug.allowed_now
                          ? <span className="flex items-center gap-1 text-success"><Check size={13} /> already allowed</span>
                          : <span className="flex items-center gap-1 text-warning"><AlertTriangle size={13} /> would be blocked by the current list</span>}
                        <Button variant="ghost" className="ml-auto h-7 px-2 py-0 text-xs" onClick={() => addEgressHost(sug.host)} disabled={listed}>
                          {listed ? <><Check size={13} /> Added</> : <><Plus size={13} /> Add</>}
                        </Button>
                      </li>
                    );
                  })}
                </ul>
              )}
              <p className="mt-2 text-xs text-on-surface-variant">Adding a host only edits the box above — nothing is saved until you <b>Apply</b>. The allowed / blocked status is checked against the <b>currently applied</b> policy.</p>
            </div>
          )}
          <div className="mt-4 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5">
            <div className="mb-1 text-xs font-medium text-on-surface-variant">Test a host</div>
            <div className="flex flex-wrap items-center gap-2">
              <input value={egHost} onChange={(e) => { setEgHost(e.target.value); setEgTest(null); }}
                onKeyDown={(e) => { if (e.key === "Enter") testEgress(); }}
                placeholder="example.com or https://example.com"
                className="min-w-[220px] flex-1 rounded border border-outline-variant bg-surface px-3 py-1.5 text-sm outline-none focus:border-primary" />
              <Button variant="secondary" onClick={testEgress} disabled={egTesting || !egHost.trim()}>
                {egTesting ? <Loader2 size={15} className="animate-spin" /> : <Search size={15} />} Test
              </Button>
              {egTest && (egTest.ok
                ? <span className="flex items-center gap-1 text-sm text-success"><Check size={15} /> Allowed</span>
                : <span className="flex items-center gap-1 text-sm text-error"><X size={15} /> Refused</span>)}
            </div>
            {egTest && !egTest.ok && egTest.error && <p className="mt-1.5 break-words text-xs text-error">{egTest.error}</p>}
            <p className="mt-1.5 text-xs text-on-surface-variant">Tests against the <b>currently applied</b> policy — apply your edits first to test them.</p>
          </div>

          {/* F207 audit mode: find out what the list would break BEFORE it does.
              Turning a default-deny list on is otherwise all-or-nothing, and the
              failure is silent — a destination nobody listed stops working at the
              next scheduled backup, hours later. */}
          <div className="mt-4 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5">
            <label className="flex cursor-pointer items-start gap-3 text-sm">
              <input type="checkbox" className="mt-0.5" checked={!!egAudit?.audit_mode} disabled={egAuditBusy}
                onChange={(e) => toggleEgressAudit(e.target.checked)} />
              <span className="min-w-0 flex-1">
                <span className="flex flex-wrap items-center gap-x-2 gap-y-1">
                  <span className="font-medium">Audit mode &mdash; observe, don&rsquo;t block</span>
                  {egAudit?.audit_mode && <span className="rounded-full bg-warning/15 px-2 py-0.5 text-[11px] font-medium text-warning">not enforcing</span>}
                </span>
                <span className="mt-0.5 block text-xs text-on-surface-variant">
                  The allow-list is still evaluated, but a host it would refuse is <b>recorded and allowed through</b> instead of being blocked. Run a backup and let your notifications fire, then turn this off once the list below is empty or accounted for. <b>Test a host</b> and the suggestions above keep telling you the real verdict while this is on.
                </span>
              </span>
            </label>

            {egAudit?.audit_mode && (
              <div className="mt-3 border-t border-outline-variant/40 pt-3">
                <div className="mb-1.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs font-medium text-on-surface-variant">
                  <span>Would be blocked {egAudit.entries.length > 0 ? <>({egAudit.entries.length})</> : null}</span>
                  {egAudit.truncated && <span className="text-warning">list full &mdash; clear it and audit again</span>}
                </div>
                {egAudit.entries.length === 0 ? (
                  <p className="text-xs text-on-surface-variant">Nothing yet. Every outbound connection so far would have been allowed &mdash; run a backup and send a test notification to exercise the rest.</p>
                ) : (
                  <ul className="flex flex-col gap-1.5">
                    {egAudit.entries.map((e) => (
                      <li key={e.host} className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs">
                        <span className="min-w-0 break-all font-mono text-on-surface">{e.host}</span>
                        {e.source
                          ? <span className="shrink-0 rounded-full bg-surface-highest px-1.5 py-0.5 text-[11px] text-on-surface-variant">{e.source}</span>
                          : <span className="shrink-0 rounded-full bg-warning/15 px-1.5 py-0.5 text-[11px] text-warning">not a configured endpoint</span>}
                        <span className="shrink-0 text-on-surface-variant">{e.count}&times;</span>
                        {e.allowed_now
                          ? <span className="flex shrink-0 items-center gap-1 text-success"><Check size={13} /> now allowed</span>
                          : <Button variant="ghost" className="ml-auto h-7 shrink-0 px-2 py-0 text-xs"
                              disabled={egressListedHosts.has(e.host)}
                              onClick={() => addEgressHost(e.host)}>
                              {egressListedHosts.has(e.host) ? <><Check size={13} /> Added</> : <><Plus size={13} /> Add</>}
                            </Button>}
                      </li>
                    ))}
                  </ul>
                )}
                {(egAudit.unconfigured > 0) && (
                  <p className="mt-2 flex items-start gap-1.5 rounded bg-warning/10 px-2 py-1.5 text-xs text-warning">
                    <AlertTriangle size={13} className="mt-0.5 shrink-0" />
                    <span className="min-w-0">
                      {egAudit.unconfigured === 1 ? "One host above is" : `${egAudit.unconfigured} hosts above are`} not used by any destination, notification channel or node you configured. That can mean a redirect or a DNS change &mdash; exactly what this list exists to catch. Look at {egAudit.unconfigured === 1 ? "it" : "them"} before adding {egAudit.unconfigured === 1 ? "it" : "them"}.
                    </span>
                  </p>
                )}
                <div className="mt-3 flex flex-wrap items-center gap-2">
                  <Button variant="secondary" className="h-8 px-2.5 py-0 text-xs" disabled={egAuditAddable.length === 0} onClick={addAllAuditHosts}>
                    <Plus size={13} /> Add all configured ({egAuditAddable.length})
                  </Button>
                  <Button variant="ghost" className="h-8 px-2.5 py-0 text-xs" disabled={egAuditBusy || egAudit.entries.length === 0} onClick={clearEgressAudit}>
                    {egAuditBusy ? <Loader2 size={13} className="animate-spin" /> : <X size={13} />} Clear and start again
                  </Button>
                  <Button variant="ghost" className="h-8 px-2.5 py-0 text-xs" onClick={loadEgressAudit}>
                    <RefreshCw size={13} /> Refresh
                  </Button>
                </div>
                <p className="mt-1.5 text-[11px] text-outline">
                  <b>Add all configured</b> only takes hosts that belong to something you set up, and only edits the box above &mdash; nothing is saved until you <b>Apply</b>. Anything not recognised has to be added on its own, deliberately.
                </p>
              </div>
            )}
          </div>
        </Card>
        )}

        {/* API tokens for automation (F45) — scoped, hashed-at-rest bearer tokens. */}
        {tab === "security" && (
        <Card className="p-5">
          <div className="mb-1 flex flex-wrap items-center justify-between gap-2">
            <div className="flex items-center gap-2 text-lg font-semibold"><KeyRound size={18} className="text-primary" /> API tokens</div>
            <Button variant="primary" onClick={openTokenModal}><Plus size={15} /> Create token</Button>
          </div>
          <p className="mb-3 text-xs text-on-surface-variant">
            Named, scoped bearer tokens for automation — trigger a backup before a risky deploy, poll status from a script, or feed an external dashboard. Send them as <code className="font-mono">Authorization: Bearer dback_…</code>. Tokens are shown once, stored only as a hash, and every action they take is attributed to <span className="font-mono">token:&lt;name&gt;</span> in the audit trail. A <b>metrics</b> token locks the otherwise-open <code className="font-mono">/metrics</code> endpoint.
          </p>
          {/* F65: the CURRENT exposure state of /metrics, from the server. */}
          {metricsLocked !== null && (
            <p className={`mb-3 flex items-center gap-1.5 text-xs ${metricsLocked ? "text-on-surface-variant" : "text-warning"}`}>
              {metricsLocked
                ? <><Lock size={13} /> <span><code className="font-mono">/metrics</code> is locked — requests must present a metrics-scoped token.</span></>
                : <><AlertTriangle size={13} /> <span><code className="font-mono">/metrics</code> is open to anyone who can reach this server. Mint a metrics-scoped token to lock it.</span></>}
            </p>
          )}
          {tokens.length === 0 ? (
            <p className="text-sm text-on-surface-variant">No API tokens yet.</p>
          ) : (
            <ul className="flex flex-col divide-y divide-outline-variant/50 rounded border border-outline-variant/60">
              {tokens.map((t) => (
                <li key={t.id} className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2 text-sm">
                  <span className="font-medium text-on-surface">{t.name}</span>
                  <span className="flex flex-wrap gap-1">
                    {t.scopes.split(",").filter(Boolean).map((sc) => (
                      <span key={sc} className="rounded-full bg-surface-highest px-1.5 py-0.5 text-[11px] text-on-surface-variant">{sc}</span>
                    ))}
                  </span>
                  <span className="text-xs text-on-surface-variant">Last used {t.last_used ? fmtAgo(t.last_used) : "never"}</span>
                  {/* F65: expiry state. An expired token is still listed (and revocable) — it just no longer authenticates. */}
                  {t.expires_at > 0 && (Date.now() / 1000 > t.expires_at ? (
                    <span className="rounded-full bg-error/15 px-1.5 py-0.5 text-[11px] font-medium text-error" title={`Expired ${new Date(t.expires_at * 1000).toLocaleString()}. Requests using it are rejected (401); revoke it or mint a replacement.`}>expired</span>
                  ) : (
                    <span className="text-xs text-on-surface-variant" title={new Date(t.expires_at * 1000).toLocaleString()}>Expires {new Date(t.expires_at * 1000).toLocaleDateString()}</span>
                  ))}
                  {/* F201: the source pin, so a token's blast radius is visible in
                      the list rather than only at creation. break-all keeps a long
                      IPv6 range from widening the row. */}
                  {t.allowed_cidrs && (
                    <span className="min-w-0 break-all rounded-full bg-primary/10 px-1.5 py-0.5 font-mono text-[11px] text-primary"
                      title="This token is refused from any other source address">{t.allowed_cidrs}</span>
                  )}
                  <Button variant="danger" className="ml-auto h-7 px-2 py-0 text-xs" onClick={() => deleteToken(t)}><Trash2 size={13} /> Revoke</Button>
                </li>
              ))}
            </ul>
          )}
        </Card>
        )}

        <Modal
          open={tokModalOpen}
          onClose={() => setTokModalOpen(false)}
          title="Create API token"
          footer={
            newToken ? (
              <Button variant="primary" onClick={() => setTokModalOpen(false)}>Done</Button>
            ) : (
              <>
                <Button variant="secondary" onClick={() => setTokModalOpen(false)}>Cancel</Button>
                {/* While the step-up prompt is showing, its own Confirm button drives the retry. */}
                {!tokStepUp && (
                  <Button variant="primary" onClick={() => createToken()} disabled={tokBusy || !tokName.trim() || tokScopes.size === 0}>
                    {tokBusy ? <Loader2 size={15} className="animate-spin" /> : <Plus size={15} />} Create token
                  </Button>
                )}
              </>
            )
          }
        >
          {newToken ? (
            <div className="flex flex-col gap-3 text-sm">
              <p className="text-on-surface-variant">Copy it now — it is not shown again.</p>
              <div className="flex items-center gap-2 rounded border border-outline-variant bg-surface-lowest px-3 py-2">
                <code className="min-w-0 flex-1 break-all font-mono text-xs text-on-surface">{newToken}</code>
                <Button variant="secondary" className="shrink-0 h-8 px-2 py-0 text-xs" onClick={() => { navigator.clipboard?.writeText(newToken).then(() => setTokCopied(true)).catch(() => {}); }}>
                  {tokCopied ? <Check size={14} /> : <Copy size={14} />} {tokCopied ? "Copied" : "Copy"}
                </Button>
              </div>
              <p className="text-xs text-on-surface-variant">Use it as <code className="font-mono break-all">Authorization: Bearer {newToken.slice(0, 12)}…</code></p>
            </div>
          ) : (
            <div className="flex flex-col gap-3 text-sm">
              <div>
                <Label>Name</Label>
                <Input value={tokName} onChange={(e) => setTokName(e.target.value)} maxLength={64} placeholder="ci-deploy, uptime-monitor…" spellCheck={false} />
              </div>
              <div>
                <Label>Scopes</Label>
                <div className="flex flex-col gap-1.5">
                  {[
                    { key: "read", label: "Read-only", desc: "GET status/inventory (never downloads decrypted backup data)" },
                    { key: "backup", label: "Trigger backups", desc: "Read, plus start/verify/mirror backups (no config or restore)" },
                    { key: "metrics", label: "Metrics only", desc: "Only /metrics — and, once one exists, /metrics requires it" },
                  ].map((sc) => (
                    <label key={sc.key} className="flex cursor-pointer items-start gap-2 rounded border border-outline-variant/60 px-3 py-2">
                      <input type="checkbox" className="mt-0.5" checked={tokScopes.has(sc.key)} onChange={() => toggleTokScope(sc.key)} />
                      <span>
                        <span className="font-medium text-on-surface">{sc.label}</span>
                        <span className="mt-0.5 block text-xs text-on-surface-variant">{sc.desc}</span>
                      </span>
                    </label>
                  ))}
                </div>
              </div>
              {/* F65: optional expiry — a leaked or forgotten token dies on its own. */}
              <div>
                <Label>Expires</Label>
                <Select value={String(tokTTL)} onChange={(e) => setTokTTL(Number(e.target.value))}>
                  <option value="0">Never (until revoked)</option>
                  <option value="30">In 30 days</option>
                  <option value="90">In 90 days</option>
                  <option value="365">In 1 year</option>
                </Select>
                <p className="mt-1 text-xs text-on-surface-variant">After expiry the token stops authenticating (401) but stays listed here until you revoke it.</p>
              </div>
              {/* F201: optional source pin. Scope answers what a token may do; this
                  answers from where — so a leaked token is useless off the machine
                  it was minted for. */}
              <div>
                <Label>Allowed source addresses (optional)</Label>
                <textarea value={tokCidrs} onChange={(e) => setTokCidrs(e.target.value)} rows={2} spellCheck={false}
                  placeholder={"10.0.0.5\n10.168.1.0/24"}
                  className="w-full rounded border border-outline-variant bg-surface px-2 py-1.5 font-mono text-sm text-on-surface placeholder:text-on-surface-variant/50" />
                <p className="mt-1 text-xs text-on-surface-variant">
                  One IP or CIDR range per line. Leave blank to allow any address (how tokens have always worked). A token presented from anywhere else is refused before its scope is even considered, and the attempt is recorded in the audit trail.
                </p>
                {tokCidrList.length > 0 && (
                  <p className="mt-1 break-words text-xs text-primary">Will be usable only from: <span className="font-mono">{tokCidrList.join(", ")}</span></p>
                )}
              </div>
              {tokStepUp && (
                <StepUpPrompt totp={tokStepUp.totp} busy={tokBusy} error={tokStepUp.err} confirmLabel="Create token" onConfirm={(p, c) => createToken(p, c)} />
              )}
            </div>
          )}
        </Modal>

        {/* Performance & tuning (F15) — env-only knobs, now validated in-app */}
        {tab === "advanced" && (
        <Card className="p-5">
          <div className="mb-4 flex items-center gap-2 text-lg font-semibold"><Gauge size={18} className="text-primary" /> Performance &amp; tuning</div>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
            <div>
              <Label>Database ready timeout (seconds)</Label>
              <Input type="number" min={10} max={3600}
                value={s["db.ready_timeout_seconds"] || "300"}
                onChange={(e) => setS({ ...s, "db.ready_timeout_seconds": String(Math.min(3600, Math.max(10, parseInt(e.target.value || "300", 10)))) })} />
              <p className="mt-1 text-xs text-on-surface-variant">How long a restore/verify/drill waits for a throwaway database to accept connections. Raise it for slow NAS hardware. Applies immediately.</p>
            </div>
            <div>
              <Label>Schedule jitter (seconds)</Label>
              <Input type="number" min={0} max={3600}
                value={s["schedule.jitter_seconds"] || "0"}
                onChange={(e) => setS({ ...s, "schedule.jitter_seconds": String(Math.min(3600, Math.max(0, parseInt(e.target.value || "0", 10)))) })} />
              <p className="mt-1 text-xs text-on-surface-variant">Spreads the start of scheduled backups over a random window so a fleet-wide run doesn't thundering-herd. 0 = off. Applies to the next scheduled dispatch.</p>
            </div>
            <div>
              <Label>Max offsite upload (Mbit/s, 0 = unlimited)</Label>
              <Input type="number" min={0}
                value={s["upload.max_mbps"] || "0"}
                onChange={(e) => setS({ ...s, "upload.max_mbps": String(Math.max(0, parseInt(e.target.value || "0", 10))) })} />
              <p className="mt-1 text-xs text-on-surface-variant">Caps the total offsite upload rate across all concurrent backups. Applies immediately. (Per-destination caps are set on each destination.)</p>
            </div>
            <div>
              <Label>Large bind-mount threshold (GiB)</Label>
              <Input type="number" min={1} max={1024}
                value={s["backup.bind_skip_gib"] || "5"}
                onChange={(e) => setS({ ...s, "backup.bind_skip_gib": String(Math.min(1024, Math.max(1, parseInt(e.target.value || "5", 10)))) })} />
              <p className="mt-1 text-xs text-on-surface-variant">Bind mounts larger than this are excluded from a backup by default (they're usually media libraries managed separately). Named volumes are always included. A per-container override, set on the container, wins over this global value. Applies to the next backup / mount preview.</p>
            </div>
            <div>
              <Label>Scrub items per cycle</Label>
              <Input type="number" min={1} max={5}
                value={s["scrub.per_cycle"] || "3"}
                onChange={(e) => setS({ ...s, "scrub.per_cycle": String(Math.min(5, Math.max(1, parseInt(e.target.value || "3", 10)))) })} />
              <p className="mt-1 text-xs text-on-surface-variant">How many stored backups are re-verified each scrub cycle (every 30 min). Raise it to sweep a large catalog faster on capable hardware. Applies to the next cycle.</p>
            </div>
            <div>
              <Label>Destination "almost full" at (%)</Label>
              <Input type="number" min={50} max={99}
                value={s["alert.dest_full_pct"] || "90"}
                onChange={(e) => setS({ ...s, "alert.dest_full_pct": String(Math.min(99, Math.max(50, parseInt(e.target.value || "90", 10)))) })} />
              <p className="mt-1 text-xs text-on-surface-variant">The used percentage at which a destination raises the "almost full" alert. Lower it for tiny targets that need earlier warning. Applies to the next capacity check.</p>
            </div>
            <div>
              <Label>Warn when filling within (days)</Label>
              <Input type="number" min={1} max={3650}
                value={s["alert.forecast_days"] || "30"}
                onChange={(e) => setS({ ...s, "alert.forecast_days": String(Math.min(3650, Math.max(1, parseInt(e.target.value || "30", 10)))) })} />
              <p className="mt-1 text-xs text-on-surface-variant">The "filling up" forecast horizon: a destination projected to fill within this many days is flagged. Applies to the next capacity check.</p>
            </div>
            <div>
              <Label>Max concurrent backups</Label>
              <Input type="number" min={1} max={64}
                value={s["backup.max_concurrent"] || "3"}
                onChange={(e) => setS({ ...s, "backup.max_concurrent": String(Math.min(64, Math.max(1, parseInt(e.target.value || "3", 10)))) })} />
              <p className="mt-1 text-xs text-on-surface-variant">How many backups run at once across the whole fleet. Lower it to serialize on a small host. Applies immediately (running backups aren't interrupted).</p>
            </div>
            <div>
              <Label>Max concurrent per node</Label>
              <Input type="number" min={1} max={64}
                value={s["backup.max_concurrent_per_node"] || "2"}
                onChange={(e) => setS({ ...s, "backup.max_concurrent_per_node": String(Math.min(64, Math.max(1, parseInt(e.target.value || "2", 10)))) })} />
              <p className="mt-1 text-xs text-on-surface-variant">How many backups run at once on any single node, so one busy node can't hog the fleet. Applies immediately.</p>
            </div>
            <div>
              <Label>Restore health timeout (seconds)</Label>
              <Input type="number" min={30} max={3600}
                value={s["restore.health_timeout_seconds"] || "300"}
                onChange={(e) => setS({ ...s, "restore.health_timeout_seconds": String(Math.min(3600, Math.max(30, parseInt(e.target.value || "300", 10)))) })} />
              <p className="mt-1 text-xs text-on-surface-variant">How long a restore waits for the container to come up healthy before it auto-rolls-back to the safety snapshot. Raise it for a heavy app whose first boot runs a long migration. A per-container override can be set on the container.</p>
            </div>
            <div>
              <Label>Test-clone lifetime (hours)</Label>
              <Input type="number" min={1} max={168}
                value={s["restore.test_clone_ttl_hours"] || "24"}
                onChange={(e) => setS({ ...s, "restore.test_clone_ttl_hours": String(Math.min(168, Math.max(1, parseInt(e.target.value || "24", 10)))) })} />
              <p className="mt-1 text-xs text-on-surface-variant">How long a <b>Test restore</b> clone stays up before it is removed automatically, together with the volumes created for it. The clone carries its own expiry, so a clone already running keeps the lifetime it was given; this applies to the next one.</p>
            </div>
            <div>
              <Label>Drill boot wait (seconds)</Label>
              <Input type="number" min={10} max={600}
                value={s["drill.boot_wait_seconds"] || "45"}
                onChange={(e) => setS({ ...s, "drill.boot_wait_seconds": String(Math.min(600, Math.max(10, parseInt(e.target.value || "45", 10)))) })} />
              <p className="mt-1 text-xs text-on-surface-variant">How long a restore drill waits for the throwaway app to come up before judging it. Raise it for a slow-starting image. Applies to the next drill.</p>
            </div>
            <div>
              <Label>Critical-DB minimum RPO (seconds)</Label>
              <Input type="number" min={60} max={3600}
                value={s["critical.rpo_min_seconds"] || "300"}
                onChange={(e) => setS({ ...s, "critical.rpo_min_seconds": String(Math.min(3600, Math.max(60, parseInt(e.target.value || "300", 10)))) })} />
              <p className="mt-1 text-xs text-on-surface-variant">The smallest recovery-point objective a database can be assigned in the critical tier — a floor so aggressive re-dumps can't overload the DB they protect. Lower it (e.g. 120) for a 2-minute RPO.</p>
            </div>
            <div>
              <Label>Critical-DB check interval (seconds)</Label>
              <Input type="number" min={30} max={600}
                value={s["critical.tick_seconds"] || "60"}
                onChange={(e) => setS({ ...s, "critical.tick_seconds": String(Math.min(600, Math.max(30, parseInt(e.target.value || "60", 10)))) })} />
              <p className="mt-1 text-xs text-on-surface-variant">How often the low-RPO loop checks whether a critical database is due for a fresh dump. Applies on the next cycle.</p>
            </div>
            <div>
              <Label>Pause after N failed low-RPO backups</Label>
              <Input type="number" min={1} max={10}
                value={s["critical.fail_limit"] || "3"}
                onChange={(e) => setS({ ...s, "critical.fail_limit": String(Math.min(10, Math.max(1, parseInt(e.target.value || "3", 10)))) })} />
              <p className="mt-1 text-xs text-on-surface-variant">Circuit-breaker: after this many consecutive verification failures, a critical DB's automatic backups pause until one verifies again — so a DB that can't produce a trustworthy dump isn't backed up on a loop.</p>
            </div>
            <div>
              <Label>Backup drift alert factor (×)</Label>
              <Input type="number" min={2} max={100}
                value={s["alert.anomaly_factor"] || "3"}
                onChange={(e) => setS({ ...s, "alert.anomaly_factor": String(Math.min(100, Math.max(2, parseInt(e.target.value || "3", 10)))) })} />
              <p className="mt-1 text-xs text-on-surface-variant">Warn when a backup takes or grows more than this many times a container's recent usual — an early sign of a runaway volume or a slow disk. Compared against the median of the last 8 successful backups; a container with fewer than 3 never alerts.</p>
            </div>
            {/* F69: ransomware tripwire thresholds — mass-change detection on
                incremental deltas; a trip flags the backup suspect and freezes
                the container's retention until reviewed. */}
            <div>
              <Label>Tripwire: minimum files</Label>
              <Input type="number" min={10} max={100000}
                value={s["tripwire.min_files"] || "200"}
                onChange={(e) => setS({ ...s, "tripwire.min_files": String(Math.min(100000, Math.max(10, parseInt(e.target.value || "200", 10)))) })} />
              <p className="mt-1 text-xs text-on-surface-variant">Minimum changed or deleted files before the mass-change tripwire can fire — so small volumes never false-positive. Applies to the next incremental backup.</p>
            </div>
            <div>
              <Label>Tripwire: changed threshold (%)</Label>
              <Input type="number" min={10} max={100}
                value={s["tripwire.changed_pct"] || "60"}
                onChange={(e) => setS({ ...s, "tripwire.changed_pct": String(Math.min(100, Math.max(10, parseInt(e.target.value || "60", 10)))) })} />
              <p className="mt-1 text-xs text-on-surface-variant">Flag a backup as suspect when more than this share of the previously-indexed files changed in a single run — the signature of mass encryption.</p>
            </div>
            <div>
              <Label>Tripwire: deleted threshold (%)</Label>
              <Input type="number" min={10} max={100}
                value={s["tripwire.deleted_pct"] || "40"}
                onChange={(e) => setS({ ...s, "tripwire.deleted_pct": String(Math.min(100, Math.max(10, parseInt(e.target.value || "40", 10)))) })} />
              <p className="mt-1 text-xs text-on-surface-variant">Flag a backup as suspect when more than this share of the previously-indexed files were deleted in a single run — the signature of a wipe.</p>
            </div>
            <label className="flex cursor-pointer items-start gap-3 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm sm:col-span-3">
              <input
                type="checkbox"
                className="mt-0.5"
                checked={(s["tripwire.enabled"] ?? "true") !== "false"}
                onChange={(e) => setS({ ...s, "tripwire.enabled": e.target.checked ? "true" : "false" })}
              />
              <span>
                Ransomware tripwire: detect mass-change events on incremental backups
                <span className="mt-0.5 block text-xs text-on-surface-variant">For containers using <b>incremental backups</b>: when one run's delta touches an abnormal share of the volume (thresholds above), the backup still succeeds but is marked <b>suspect</b>, a critical alert fires, and <b>retention pruning is frozen</b> for that container — so clean pre-event backups can't be aged out while a schedule keeps capturing encrypted data. Clear the hold from the container's page after review. Containers on full (non-incremental) backups are not covered — there is no per-file diff to analyze.</span>
              </span>
            </label>
            {/* F70: universal file index — every backup stores its complete file
                listing, powering untruncated browse, file search and generation diff. */}
            <label className="flex cursor-pointer items-start gap-3 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm sm:col-span-3">
              <input
                type="checkbox"
                className="mt-0.5"
                checked={(s["index.always"] ?? "true") !== "false"}
                onChange={(e) => setS({ ...s, "index.always": e.target.checked ? "true" : "false" })}
              />
              <span>
                Store a file index in every backup (find a file, compare generations, browse without limits)
                <span className="mt-0.5 block text-xs text-on-surface-variant">Each backup includes a small compressed listing of every captured file (path, size, mtime) inside the encrypted archive. It powers <b>Find a file</b> across backup generations, <b>Compare with previous</b>, and full file browsing without decrypting the whole archive. Incremental backups always store it (it's their diff base); this toggle covers ordinary full backups. Adds one quick file scan per backup. Applies to the next backup.</span>
              </span>
            </label>
            {/* F85: synthetic fulls — the periodic full of an incremental chain is
                merged locally from the stored archives; the source sends only deltas. */}
            <label className="flex cursor-pointer items-start gap-3 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm sm:col-span-3">
              <input
                type="checkbox"
                className="mt-0.5"
                checked={(s["backup.synthetic_full"] ?? "true") !== "false"}
                onChange={(e) => setS({ ...s, "backup.synthetic_full": e.target.checked ? "true" : "false" })}
              />
              <span>
                Build periodic full backups locally from the incremental chain (no full re-read of the source)
                <span className="mt-0.5 block text-xs text-on-surface-variant">For containers using <b>incremental backups</b>: when the chain reaches its "full backup every N" boundary, DockBack captures only the changed files from the source and merges the stored chain into a brand-new, self-contained full archive locally — a 60 GB media library is re-read only when a backup actually changed it. The result is an ordinary full backup (restore, verify, and drills are unchanged). Any merge problem automatically falls back to a normal full capture from the source. Applies to the next boundary backup.</span>
              </span>
            </label>
            {/* F84: autotune — implicit-balanced runs over proven-incompressible
                selections execute as fast; explicit choices are never overridden. */}
            <label className="flex cursor-pointer items-start gap-3 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm sm:col-span-3">
              <input
                type="checkbox"
                className="mt-0.5"
                checked={(s["backup.autotune_compression"] ?? "true") !== "false"}
                onChange={(e) => setS({ ...s, "backup.autotune_compression": e.target.checked ? "true" : "false" })}
              />
              <span>
                Use fast compression automatically for incompressible data
                <span className="mt-0.5 block text-xs text-on-surface-variant">DockBack learns each container's real compression ratio from past runs. When a selection of 1 GiB or more has a learned ratio of 0.97 or higher (already-compressed media: photos, video, audiobooks, ebooks), a run that would use the default <b>Balanced</b> mode switches to <b>Fast</b> — typically 2–4× the throughput for about the same archive size. A compression mode you chose explicitly (per container, per stack run) is always respected. Applies to the next backup.</span>
              </span>
            </label>
            {/* F116: a database MEASURED as corrupt at capture. Default on —
                storing it as a green backup is the silent failure this ends. */}
            <label className="flex cursor-pointer items-start gap-3 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm sm:col-span-3">
              <input
                type="checkbox"
                className="mt-0.5"
                checked={(s["backup.fail_on_corrupt_db"] ?? "true") !== "false"}
                onChange={(e) => setS({ ...s, "backup.fail_on_corrupt_db": e.target.checked ? "true" : "false" })}
              />
              <span>
                Fail a backup when a database is corrupt
                <span className="mt-0.5 block text-xs text-on-surface-variant">Every SQLite database DockBack snapshots is checked with <span className="font-mono">PRAGMA integrity_check</span> as it is captured. When one comes back damaged, the backup <b>fails</b> — so you find out while the original is still there to repair, instead of during a recovery. The damage is at the source; a backup would preserve it, not fix it. Turn this off only if a database you don't care about (an abandoned cache, say) is blocking backups you do — the archive then records the damage instead of refusing. A database that merely could not be snapshotted (locked, in use) never fails a run; its raw file is captured as before.</span>
              </span>
            </label>
            <div className="sm:col-span-3">
              <Label>Volume sidecar image</Label>
              <Input type="text" spellCheck={false}
                value={s["backup.sidecar_image"] ?? ""}
                placeholder="alpine:3.20"
                onChange={(e) => setS({ ...s, "backup.sidecar_image": e.target.value })} />
              <p className="mt-1 text-xs text-on-surface-variant">Image used for volume copy/measurement. Point at a mirror for air-gapped nodes (e.g. <span className="font-mono">registry.internal/alpine:3.20</span>). Applies to the next backup.</p>
            </div>
          </div>
          <p className="mt-3 text-xs text-on-surface-variant">These were previously environment-only knobs. Values are validated and clamped when saved, and — including the concurrency caps — apply <b>immediately</b>, without a restart (a running backup is never interrupted; a lower cap just serializes new work as slots free).</p>
        </Card>
        )}

        {/* F228: signed-in devices, as their own thing.
            This list used to live INSIDE the Account Security card, below the
            password fields, behind a divider, introduced by the same small
            <Label> that titles "Current password" — so the page said it was a
            field of the password form. It also inherited that card's max-w-md
            column, which squeezed every device row into half the screen while
            the page had width going spare.

            It is the surface you come looking for when something feels wrong, so
            it now sits ABOVE the password card, at full width, with a heading
            and a live count. */}
        {tab === "security" && (
        <Card className="p-5">
          <div className="mb-4 flex flex-wrap items-center gap-2">
            <Laptop size={18} className={otherDevices > 0 ? "shrink-0 text-warning" : "shrink-0 text-primary"} />
            <h3 className="text-lg font-semibold">Signed-in devices</h3>
            {devices !== null && (
              <span className={`shrink-0 rounded px-2 py-0.5 text-xs font-semibold ${otherDevices > 0 ? "bg-warning/15 text-warning" : "bg-on-surface-variant/10 text-on-surface-variant"}`}>
                {devices.length}
              </span>
            )}
            {otherDevices > 0 && (
              <Button variant="secondary" className="ml-auto shrink-0" disabled={soBusy}
                onClick={async () => { await signOutOthers(); loadDevices(); }}>
                {soBusy ? <Loader2 size={15} className="animate-spin" /> : <LogOut size={15} />} Sign out the other {otherDevices}
              </Button>
            )}
          </div>

          {soMsg && <div className={`mb-3 rounded px-3 py-2 text-sm ${soMsg.ok ? "bg-success/10 text-success" : "bg-error/10 text-error"}`}>{soMsg.text}</div>}

          {/* The state line. Two or more sessions is the one worth flagging, and
              it names the fix — signing one out ends that session, changing the
              password ends all of them. */}
          {devices !== null && (otherDevices > 0 ? (
            <div className="mb-3 flex items-start gap-2.5 rounded border border-warning/40 bg-warning/10 px-3 py-2.5 text-sm text-warning">
              <AlertTriangle size={16} className="mt-0.5 shrink-0" />
              <p className="min-w-0 break-words">
                <b className="text-on-surface">{otherDevices === 1 ? "One other device is" : `${otherDevices} other devices are`} signed in.</b>{" "}
                If you do not recognise one, sign it out &mdash; then change your password, which ends every other session as well.
              </p>
            </div>
          ) : (
            <div className="mb-3 flex items-start gap-2.5 rounded border border-success/30 bg-success/[0.08] px-3 py-2.5 text-sm text-on-surface-variant">
              <ShieldCheck size={16} className="mt-0.5 shrink-0 text-success" />
              <p className="min-w-0 break-words">
                Only this browser is signed in. Sessions end after <b className="text-on-surface">{sBaseline["security.session_idle_minutes"] || "30"} minutes</b> of
                inactivity and <b className="text-on-surface">{sBaseline["security.session_ttl_hours"] || "12"} hours</b> in total, whichever comes first.
              </p>
            </div>
          ))}

          {devices === null ? (
            <div className="flex items-center gap-2 text-sm text-on-surface-variant"><Loader2 size={14} className="animate-spin" /> Loading signed-in devices…</div>
          ) : devices.length === 0 ? (
            <p className="text-sm text-on-surface-variant">No active sessions.</p>
          ) : (
            <ul className="divide-y divide-outline-variant/40 rounded border border-outline-variant/60">
              {devices.map((d) => (
                <li key={d.id} className={`flex flex-wrap items-center gap-x-3 gap-y-1.5 px-3.5 py-2.5 text-sm ${d.current ? "bg-primary/[0.05]" : ""}`}>
                  <span className="min-w-0 flex-1 basis-48">
                    <span className="block break-words font-semibold text-on-surface" title={d.user_agent || "No browser reported"}>{d.device}</span>
                    <span className="mt-0.5 block break-words text-xs text-on-surface-variant">
                      Signed in {fmtWhen(d.created_at)}{d.expires_at ? <> &middot; expires in {fmtUntil(d.expires_at)}</> : null}
                    </span>
                  </span>
                  {d.current && <span className="shrink-0 rounded-full bg-primary/15 px-2 py-0.5 text-[11px] font-medium text-primary">this device</span>}
                  {d.ip && <span className="shrink-0 font-mono text-xs text-on-surface-variant">{d.ip}</span>}
                  <span className="shrink-0 text-xs text-on-surface-variant">Active {fmtAgo(d.last_seen)}</span>
                  {!d.current && (
                    <Button variant="secondary" className="shrink-0" disabled={devBusy === d.id} onClick={() => revokeDevice(d)}>
                      {devBusy === d.id ? <Loader2 size={13} className="animate-spin" /> : <LogOut size={13} />} Sign out
                    </Button>
                  )}
                </li>
              ))}
            </ul>
          )}

          <p className="mt-3 break-words text-xs text-on-surface-variant">
            Signing out a device ends its session immediately. This browser stays signed in either way.
          </p>
        </Card>
        )}

        {/* Account Security */}
        {tab === "security" && (
        <Card className="p-5">
        <div className="mb-4 flex items-center gap-2 text-lg font-semibold"><Lock size={18} className="text-primary" /> Account Security</div>
        <div className="grid max-w-md gap-3">
          <div><Label>Current password</Label><Input type="password" value={curPw} onChange={(e) => setCurPw(e.target.value)} autoComplete="current-password" /></div>
          <div><Label>New password</Label><Input type="password" value={newPw} onChange={(e) => setNewPw(e.target.value)} autoComplete="new-password" placeholder={`at least ${minPasswordLen} characters`} /></div>
          <div><Label>Confirm new password</Label><Input type="password" value={confPw} onChange={(e) => setConfPw(e.target.value)} autoComplete="new-password" /></div>
          {pwMsg && <div className={`rounded px-3 py-2 text-sm ${pwMsg.ok ? "bg-success/10 text-success" : "bg-error/10 text-error"}`}>{pwMsg.text}</div>}
          <div>
            <Button variant="primary" disabled={pwBusy || !curPw || !newPw || !confPw} onClick={changePw}>
              {pwBusy ? <Loader2 size={15} className="animate-spin" /> : <Lock size={15} />} Change Password
            </Button>
          </div>
          <p className="text-xs text-on-surface-variant">
            Changing your password signs out all other sessions and devices{otherDevices > 0 ? <> &mdash; <b className="text-on-surface">{otherDevices} right now</b></> : null}. This browser stays signed in.
          </p>

        </div>
      </Card>
        )}

        {/* Sign-in policy (F203) — how long a session lasts and how long a
            password must be. Previously fixed at build time. */}
        {tab === "security" && (
        <Card className="p-5">
          <div className="mb-1 flex items-center gap-2 text-lg font-semibold"><Timer size={18} className="text-primary" /> Sign-in policy</div>
          <p className="mb-4 text-xs text-on-surface-variant">
            How long a signed-in session lasts, and the shortest password this instance accepts. The right answer depends on where DockBack is reachable from &mdash; an instance behind a VPN can reasonably hold a session longer than one on a shared workstation. Environment values (<code className="font-mono">DOCKBACK_SESSION_TTL_HOURS</code>, <code className="font-mono">DOCKBACK_SESSION_IDLE_MINUTES</code>, <code className="font-mono">DOCKBACK_MIN_PASSWORD_LEN</code>) set the boot default; a value here overrides it.
          </p>
          <div className="grid gap-4 sm:grid-cols-3">
            <div>
              <Label>Session lifetime</Label>
              <div className="flex flex-wrap items-baseline gap-1.5">
                <Input type="number" min={1} max={720} className="!w-24 text-right" value={polTTL}
                  onChange={(e) => setPolTTL(e.target.value)} />
                <span className="text-xs text-on-surface-variant">hours</span>
              </div>
              <p className="mt-1 text-xs text-on-surface-variant">Every session ends this long after sign-in, active or not. 1&ndash;720 hours.</p>
            </div>
            <div>
              <Label>Inactivity timeout</Label>
              <div className="flex flex-wrap items-baseline gap-1.5">
                <Input type="number" min={1} max={1440} className="!w-24 text-right" value={polIdle}
                  onChange={(e) => setPolIdle(e.target.value)} />
                <span className="text-xs text-on-surface-variant">minutes</span>
              </div>
              <p className="mt-1 text-xs text-on-surface-variant">Signed out after this long with no real interaction. Applies immediately, including to sessions already open.</p>
            </div>
            <div>
              <Label>Minimum password length</Label>
              <div className="flex flex-wrap items-baseline gap-1.5">
                <Input type="number" min={PASSWORD_LEN_FLOOR} max={PASSWORD_LEN_CEILING} className="!w-24 text-right" value={polMinPw}
                  onChange={(e) => setPolMinPw(e.target.value)} />
                <span className="text-xs text-on-surface-variant">characters</span>
              </div>
              <p className="mt-1 text-xs text-on-surface-variant">Enforced on every password change. Can be raised, never lowered below {PASSWORD_LEN_FLOOR}.</p>
            </div>
          </div>
          <p className="mt-3 text-xs text-on-surface-variant">
            Shortening either timeout takes effect for everyone; lengthening one applies to sessions created afterwards. Raising the password minimum does not invalidate the current password &mdash; it applies the next time one is set.
          </p>
          {polStepUp && (
            <div className="mt-3 max-w-md">
              <StepUpPrompt totp={polStepUp.totp} busy={polBusy} error={polStepUp.err} confirmLabel="Save policy"
                onConfirm={(pw, code) => saveSignInPolicy(pw, code)} />
            </div>
          )}
          <div className="mt-4">
            <Button variant="primary" disabled={polBusy || !polDirty} onClick={() => saveSignInPolicy()}>
              {polBusy && !polStepUp ? <Loader2 size={15} className="animate-spin" /> : <Check size={15} />} Save sign-in policy
            </Button>
          </div>
        </Card>
        )}

        {/* Notifications & Two-Factor hold their OWN in-progress edit state, so they
            stay MOUNTED and are only hidden when their tab is inactive — switching
            tabs never discards an unsaved edit. Notifications is placed first so it is
            the stack's first rendered child on its own tab (no stray leading gap). */}
        <div className={tab === "notifications" ? "" : "hidden"}><NotificationsCard /></div>
        <div className={tab === "security" ? "" : "hidden"}><TwoFactorCard /></div>
      </div>

      {/* Application Backup & Restore (the app's own state — PLAN §6.5/§9.3) */}
      {tab === "advanced" && (
      <Card id="app-backup" className="mt-5 p-5">
        <div className="mb-1 flex items-center gap-2 text-lg font-semibold"><DatabaseBackup size={18} className="text-primary" /> Application Backup &amp; Restore</div>
        <p className="mb-4 max-w-3xl text-sm text-on-surface-variant">
          Back up <span className="font-medium text-on-surface">DockBack itself</span> — every node &amp; its credentials,
          the full container-backup catalog, your settings &amp; destinations, and the admin account
          including 2FA. Archives are <span className="font-medium text-on-surface">encrypted with your master key</span>
          and stored on the backups volume; download one to keep it off-box (Nextcloud, Synology, SMB, USB) and restore any time.
        </p>

        <dl className="mb-4 grid max-w-md grid-cols-2 gap-y-1 text-sm">
          <dt className="text-on-surface-variant">Encryption key fingerprint</dt>
          <dd className="text-right font-mono text-xs">{abInfo?.key_fingerprint || "—"}</dd>
          <dt className="text-on-surface-variant">Current data size</dt>
          <dd className="text-right">{abInfo ? fmtBytes(abInfo.db_bytes) : "—"}</dd>
          <dt className="text-on-surface-variant">Last proven</dt>
          <dd className="text-right text-xs">
            {!abInfo || !abInfo.last_drill_at ? (
              <span className="text-on-surface-variant">never proven — run a test restore</span>
            ) : abInfo.last_drill_ok ? (
              <span className="text-success">{fmtAgo(abInfo.last_drill_at)} <Check size={12} className="inline" /></span>
            ) : (
              <span className="text-error" title={abInfo.last_drill_detail}>{fmtAgo(abInfo.last_drill_at)} — failed</span>
            )}
          </dd>
        </dl>

        {abMsg && <div className={`mb-3 max-w-3xl rounded px-3 py-2 text-sm ${abMsg.ok ? "bg-success/10 text-success" : "bg-error/10 text-error"}`}>{abMsg.text}</div>}

        <div className="mb-4 flex flex-wrap gap-2">
          <Button variant="primary" disabled={abBusy} onClick={createBackup}>
            {abBusy ? <Loader2 size={15} className="animate-spin" /> : <DatabaseBackup size={15} />} Create backup
          </Button>
          <Button variant="secondary" disabled={abBusy} onClick={() => restoreRef.current?.click()}>
            <Upload size={15} /> Upload &amp; restore…
          </Button>
          <Button variant="secondary" disabled={drilling} onClick={drillAppBackup} title="Decrypt the newest app-backup and run an integrity check to prove it restores.">
            {drilling ? <Loader2 size={15} className="animate-spin" /> : <ShieldCheck size={15} />} Test restore now
          </Button>
          <input ref={restoreRef} type="file" accept=".dback" className="hidden" onChange={onRestoreFile} />
        </div>

        {/* The chosen file is held in state so confirming here re-uploads the
            same one — a file input cannot be re-filled from script. */}
        {abStepUp?.kind === "restore-upload" && (
          <div className="mb-4 max-w-xl">
            <p className="mb-2 text-sm text-on-surface-variant">Restoring from <span className="font-medium text-on-surface">{abStepUp.upload.name}</span></p>
            <StepUpPrompt totp={abStepUp.totp} busy={abBusy} error={abStepUp.err}
              confirmLabel="Confirm and restore" onConfirm={(p, c) => restoreUpload(abStepUp.upload, p, c)} />
          </div>
        )}

        {/* Automatic application backup (F4) */}
        <div className="mb-5 rounded border border-outline-variant/50 bg-surface-lowest p-4">
          <div className="mb-1 flex items-center justify-between gap-3">
            <div className="flex items-center gap-2 text-sm font-semibold text-on-surface"><CalendarClock size={16} className="text-primary" /> Automatic application backup</div>
            <label className="flex items-center gap-2 text-sm">
              <input type="checkbox" checked={abSched.enabled} onChange={(e) => setAbSchedField({ enabled: e.target.checked })} /> Enabled
            </label>
          </div>
          <p className="mb-3 text-xs text-on-surface-variant">Runs on a schedule, keeps the newest copies, and (optionally) pushes each to your external destinations.</p>
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-4">
            <div>
              <Label>Frequency</Label>
              <Select value={abSched.kind} onChange={(e) => setAbSchedField({ kind: e.target.value })}>
                <option value="daily">Daily</option>
                <option value="weekly">Weekly</option>
                <option value="monthly">Monthly</option>
              </Select>
            </div>
            <div><Label>Time</Label><Input type="time" value={abSched.time} onChange={(e) => setAbSchedField({ time: e.target.value })} /></div>
            {abSched.kind === "weekly" && (
              <div>
                <Label>Day of week</Label>
                <Select value={abSched.weekday} onChange={(e) => setAbSchedField({ weekday: parseInt(e.target.value, 10) })}>
                  {WEEKDAYS.map((d, i) => <option key={i} value={i}>{d}</option>)}
                </Select>
              </div>
            )}
            {abSched.kind === "monthly" && (
              <div><Label>Day of month (1–28)</Label><Input type="number" min={1} max={28} value={abSched.monthday} onChange={(e) => setAbSchedField({ monthday: parseInt(e.target.value || "1", 10) })} /></div>
            )}
            <div><Label>Keep newest</Label><Input type="number" min={0} value={abSched.keep} onChange={(e) => setAbSchedField({ keep: Math.max(0, parseInt(e.target.value || "0", 10)) })} /></div>
          </div>
          <label className="mt-3 flex items-center gap-2 text-sm">
            <input type="checkbox" checked={abSched.push_external} onChange={(e) => setAbSchedField({ push_external: e.target.checked })} /> Also push to external destinations
          </label>
          {abSched.enabled && (
            <div className="mt-2 text-xs text-on-surface-variant">Next automatic backup: <span className="text-on-surface">{abNextRun ? new Date(abNextRun * 1000).toLocaleString() : "—"}</span></div>
          )}
        </div>

        {/* Available backups */}
        <Label>Available backups</Label>
        <div className="mt-1 space-y-2">
          {abList.length === 0 && (
            <div className="rounded border border-outline-variant/50 bg-surface-lowest px-3 py-4 text-center text-sm text-on-surface-variant">
              No application backups yet — click <span className="font-medium text-on-surface">Create backup</span> to make one.
            </div>
          )}
          {abList.map((b) => (
            <div key={b.file} className="flex flex-wrap items-center gap-3 rounded border border-outline-variant/50 bg-surface-lowest px-3 py-2.5">
              <div className="min-w-0 flex-1">
                <div className="text-sm font-medium text-on-surface">{fmtWhen(b.created_at)}</div>
                <div className="font-mono text-xs text-on-surface-variant">
                  {b.nodes} node{b.nodes === 1 ? "" : "s"} • {b.backups} backup{b.backups === 1 ? "" : "s"} • {b.destinations} destination{b.destinations === 1 ? "" : "s"} • {fmtBytes(b.size_bytes)}
                </div>
              </div>
              <div className="flex shrink-0 gap-1">
                <Button variant="ghost" size="sm" disabled={abBusy} onClick={() => downloadBackup(b)} title="Download to keep off-box — confirms your password first"><Download size={14} /> Download</Button>
                <Button variant="ghost" size="sm" disabled={abBusy} onClick={() => restoreLocal(b)} title="Restore this backup"><RotateCcw size={14} /> Restore</Button>
                <Button variant="ghost" size="sm" disabled={abBusy} onClick={() => deleteBackup(b)} title="Delete this backup"><Trash2 size={14} className="text-error" /></Button>
              </div>
              {/* basis-full so the prompt drops onto its own line under this
                  archive's row rather than squeezing the buttons. */}
              {abStepUp?.kind === "download" && abStepUp.file === b.file && (
                <div className="basis-full min-w-0">
                  <StepUpPrompt totp={abStepUp.totp} busy={abBusy} error={abStepUp.err}
                    confirmLabel="Confirm and download" onConfirm={(p, c) => downloadBackup(b, p, c)} />
                </div>
              )}
              {abStepUp?.kind === "restore-local" && abStepUp.file === b.file && (
                <div className="basis-full min-w-0">
                  <StepUpPrompt totp={abStepUp.totp} busy={abBusy} error={abStepUp.err}
                    confirmLabel="Confirm and restore" onConfirm={(p, c) => restoreLocal(b, p, c)} />
                </div>
              )}
            </div>
          ))}
        </div>

        {/* External destinations (separate from container backup destinations) */}
        <div className="mt-5 border-t border-outline-variant/40 pt-4">
          <div className="mb-1 flex items-center justify-between">
            <Label>External destinations</Label>
            <button onClick={() => setAddAppOpen(true)} className="flex items-center gap-1 text-sm font-medium text-primary hover:underline">
              <Plus size={14} /> Add destination
            </button>
          </div>
          <p className="mb-3 max-w-3xl text-xs text-on-surface-variant">
            Push application backups to your own server (Nextcloud, Synology, SMB, S3) — kept separate from the
            destinations used for container backups, so a host failure doesn't take your only copy.
          </p>

          {extMsg && <div className={`mb-3 max-w-3xl rounded px-3 py-2 text-sm ${extMsg.ok ? "bg-success/10 text-success" : "bg-error/10 text-error"}`}>{extMsg.text}</div>}

          <div className="space-y-2">
            {appDests.length === 0 && (
              <div className="rounded border border-outline-variant/50 bg-surface-lowest px-3 py-3 text-center text-sm text-on-surface-variant">
                No external destinations yet.
              </div>
            )}
            {appDests.map((d) => (
              <div key={d.id} className="rounded border border-outline-variant/50 bg-surface-lowest">
                <div className="flex flex-wrap items-center gap-3 px-3 py-2.5">
                  <span className={`h-2.5 w-2.5 shrink-0 rounded-full status-pulse ${d.reachable ? "bg-success" : "bg-error"}`} title={d.reachable ? "Reachable" : "Unreachable"} />
                  <div className="min-w-0 flex-1">
                    <div className="text-sm font-medium text-on-surface">{d.name}{!d.reachable && <span className="ml-2 text-xs text-error">Down</span>}</div>
                    <div className="font-mono text-xs uppercase text-on-surface-variant">{d.type}</div>
                  </div>
                  <label className="flex items-center gap-1.5 text-xs text-on-surface-variant"><input type="checkbox" checked={d.enabled} onChange={(e) => toggleAppDest(d, e.target.checked)} /> Enabled</label>
                  <Button variant="ghost" size="sm" disabled={abBusy} onClick={() => browseDest(d)}><FolderTree size={14} /> {browse?.id === d.id ? "Hide" : "Browse"}</Button>
                  <Button variant="ghost" size="sm" onClick={() => removeAppDest(d)} title="Remove"><Trash2 size={14} className="text-error" /></Button>
                </div>
                {browse?.id === d.id && (
                  <div className="border-t border-outline-variant/40 px-3 py-2">
                    {browse.items.length === 0 && <div className="py-1 text-xs text-on-surface-variant">No backups on this destination yet.</div>}
                    {browse.items.map((it) => (
                      <div key={it.key} className="py-1">
                        <div className="flex items-center justify-between text-sm">
                          <span className="font-mono text-xs text-on-surface-variant">{it.name}</span>
                          <Button variant="ghost" size="sm" disabled={abBusy} onClick={() => restoreExternal(d, it.name)}><RotateCcw size={13} /> Restore</Button>
                        </div>
                        {abStepUp?.kind === "restore-external" && abStepUp.destId === d.id && abStepUp.name === it.name && (
                          <div className="mt-2">
                            <StepUpPrompt totp={abStepUp.totp} busy={abBusy} error={abStepUp.err}
                              confirmLabel="Confirm and restore" onConfirm={(p, c) => restoreExternal(d, it.name, p, c)} />
                          </div>
                        )}
                      </div>
                    ))}
                  </div>
                )}
              </div>
            ))}
          </div>

          {appDests.some((d) => d.enabled) && (
            <Button variant="secondary" className="mt-3" disabled={extBusy} onClick={backupExternal}>
              {extBusy ? <Loader2 size={15} className="animate-spin" /> : <CloudUpload size={15} />} Back up to external now
            </Button>
          )}
        </div>

        <div className="mt-4 flex max-w-3xl items-start gap-2 rounded bg-warning/10 px-3 py-2 text-sm text-warning">
          <ShieldAlert size={16} className="mt-0.5 shrink-0" />
          <p>
            Restoring <span className="font-medium">replaces all current data and restarts the app</span> (you'll sign in
            again). It needs the <span className="font-medium">same encryption key</span> the backup was made with.
          </p>
        </div>
      </Card>
      )}

      {tab === "advanced" && <AddDestinationModal open={addAppOpen} purpose="app" onClose={() => setAddAppOpen(false)} onAdded={loadAppDests} />}

      {/* F101: reusable app-native export presets — write the recipe once, apply
          it from any container's page. */}
      {tab === "backups" && <ExportPresetsCard />}

      {/* Scheduled Backups — multiple named schedules (F6) */}
      {tab === "backups" && (
      <Card id="schedule" className="mt-5 scroll-mt-4 p-5">
        <div className="mb-4 flex items-center justify-between">
          <div className="flex items-center gap-2 text-lg font-semibold"><CalendarClock size={18} className="text-primary" /> Schedules</div>
          <Button variant="secondary" onClick={addScheduleDraft}><Plus size={16} /> Add schedule</Button>
        </div>

        <div className="mb-4 flex flex-wrap items-center gap-2 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm">
          <span className="text-on-surface-variant">Auto-remove targets missing for</span>
          <Input type="number" min={0} className="!w-20 text-right"
            value={String(policy.autoclean_missing_days ?? 0)}
            onChange={(e) => setPolicy({ ...policy, autoclean_missing_days: Math.max(0, parseInt(e.target.value || "0", 10)) })} />
          <span className="text-xs text-on-surface-variant">days (0 = never)</span>
          <span className="ml-auto text-xs text-on-surface-variant">Drops a scheduled container from its schedule after it has been gone this long, so a deleted container stops alerting forever.</span>
        </div>

        {scheds.length === 0 && (
          <div className="rounded border border-dashed border-outline-variant/60 px-3 py-6 text-center text-sm text-on-surface-variant">
            No schedules yet — add one to automate backups.
          </div>
        )}

        <div className="space-y-4">
          {scheds.map((sc, idx) => {
            const busyId = sc.id || `draft-${idx}`;
            return (
            <div key={busyId} className="rounded-lg border border-outline-variant bg-surface-lowest p-4">
              <div className="mb-3 flex flex-wrap items-center justify-between gap-3">
                <div className="min-w-[12rem] flex-1">
                  <Label>Schedule name</Label>
                  <Input value={sc.name} onChange={(e) => patchSched(idx, { name: e.target.value })} placeholder="Schedule name" />
                </div>
                <div className="flex items-center gap-3 pt-5">
                  <label className="flex items-center gap-2 text-sm">
                    <input type="checkbox" checked={sc.enabled} onChange={(e) => patchSched(idx, { enabled: e.target.checked })} /> Enabled
                  </label>
                  <Button variant="ghost" onClick={() => removeSched(idx)} title="Delete schedule"><Trash2 size={15} /> Delete</Button>
                </div>
              </div>

              <div className="grid grid-cols-1 gap-4 md:grid-cols-4">
                <div>
                  <Label>Frequency</Label>
                  <Select value={sc.kind} onChange={(e) => patchSched(idx, { kind: e.target.value })}>
                    <option value="daily">Daily</option>
                    <option value="weekly">Weekly</option>
                    <option value="monthly">Monthly</option>
                    <option value="custom">Custom (cron)</option>
                  </Select>
                </div>
                {sc.kind !== "custom" && (
                  <div><Label>Time</Label><Input type="time" value={sc.time} onChange={(e) => patchSched(idx, { time: e.target.value })} /></div>
                )}
                {sc.kind === "weekly" && (
                  <div>
                    <Label>Day of week</Label>
                    <Select value={sc.weekday} onChange={(e) => patchSched(idx, { weekday: parseInt(e.target.value, 10) })}>
                      {WEEKDAYS.map((d, i) => <option key={i} value={i}>{d}</option>)}
                    </Select>
                  </div>
                )}
                {sc.kind === "monthly" && (
                  <div><Label>Day of month (1–28)</Label><Input type="number" min={1} max={28} value={sc.monthday} onChange={(e) => patchSched(idx, { monthday: parseInt(e.target.value || "1", 10) })} /></div>
                )}
                {sc.kind === "custom" && (
                  <div className="md:col-span-2"><Label>Cron (min hour dom mon dow)</Label><Input value={sc.cron} onChange={(e) => patchSched(idx, { cron: e.target.value })} placeholder="0 3 * * 0" /></div>
                )}
              </div>

              <div className="mt-4">
                <Label>What to back up</Label>
                <p className="mb-2 text-xs text-on-surface-variant">Pick whole nodes (all {sc.include_stopped ? "" : "running "}containers), or expand to choose specific containers or a whole compose stack (optionally app-consistent). Backups go to the default destinations above, honoring the keep-count.</p>
                <label className="mb-3 flex items-center gap-2 text-sm">
                  <input type="checkbox" checked={!!sc.include_stopped} onChange={(e) => patchSched(idx, { include_stopped: e.target.checked })} />
                  Include stopped containers
                  <span className="text-xs text-on-surface-variant">— whole-node targets also back up containers that aren't running (a stopped app is captured as a file/volume snapshot). Specific containers you pick are always backed up regardless of state.</span>
                </label>
                <TargetPicker nodes={nodes} targets={sc.targets ?? []} includeStopped={!!sc.include_stopped} onChange={(targets) => patchSched(idx, { targets })} />
              </div>

              {/* F27: per-schedule destination override. */}
              <div className="mt-4">
                <Label>Destinations for this schedule</Label>
                <label className="mb-2 flex items-start gap-2 text-sm">
                  <input type="checkbox" className="mt-0.5" checked={!!sc.destinations_explicit}
                    onChange={(e) => patchSched(idx, { destinations_explicit: e.target.checked, destinations: e.target.checked ? (sc.destinations ?? policy.destinations) : [] })} />
                  <span>
                    Override where this schedule's runs go
                    <span className="mt-0.5 block text-xs text-on-surface-variant">Off = <b>Use policy default</b> (the default destinations above). On = choose per-schedule — e.g. keep a nightly local-only while a weekly pushes offsite. Uncheck all for local-only.</span>
                  </span>
                </label>
                {sc.destinations_explicit && (
                  <div className="space-y-1.5">
                    <div className="flex items-center gap-3 rounded border border-outline-variant bg-surface-lowest px-3 py-2 text-sm text-on-surface-variant">
                      <input type="checkbox" checked disabled />
                      <HardDrive size={15} /> Local <span className="ml-auto text-xs">always written</span>
                    </div>
                    {dests.map((d) => (
                      <label key={d.id} className="flex cursor-pointer items-center gap-3 rounded border border-outline-variant bg-surface-lowest px-3 py-2 text-sm">
                        <input type="checkbox" checked={(sc.destinations ?? []).includes(d.id)}
                          onChange={(e) => patchSched(idx, { destinations: e.target.checked ? [...(sc.destinations ?? []), d.id] : (sc.destinations ?? []).filter((x) => x !== d.id) })} />
                        <CloudUpload size={15} className="text-secondary" /> {d.name}
                        <span className="ml-auto text-xs uppercase text-on-surface-variant">{d.type}</span>
                      </label>
                    ))}
                    {dests.length === 0 && <div className="text-xs text-on-surface-variant">No external destinations configured — this schedule's runs are local-only.</div>}
                    {dests.length > 0 && (sc.destinations ?? []).length === 0 && (
                      <p className="text-xs text-warning">This schedule's runs stay <b>local-only</b> — no offsite copy is made (3-2-1 not satisfied for these backups).</p>
                    )}
                  </div>
                )}
              </div>

              <div className="mt-4 flex flex-wrap items-center justify-between gap-3">
                <div className="text-sm text-on-surface-variant">
                  {sc.enabled
                    ? (sc.next_run ? <>Next run: <span className="text-on-surface">{new Date(sc.next_run * 1000).toLocaleString()}</span></> : "Schedule set — save to compute next run")
                    : "Schedule disabled"}
                  {sc.last_run ? (
                    <div className="mt-0.5 text-xs">Last run: <span className="text-on-surface">{new Date(sc.last_run * 1000).toLocaleString()}</span></div>
                  ) : null}
                </div>
                <Button variant="secondary" onClick={() => runSchedNow(idx)} disabled={runningId !== "" || (sc.targets ?? []).length === 0} title="Save and back up these targets now">
                  <CloudUpload size={16} /> {runningId === busyId ? "Starting…" : "Backup Now"}
                </Button>
              </div>
            </div>
          );})}
        </div>
        {runMsg && <div className="mt-3 rounded bg-secondary/10 px-3 py-2 text-sm text-secondary">{runMsg}</div>}
      </Card>
      )}

      {/* External backup destinations */}
      {tab === "destinations" && (<>
      <div className="mt-8 flex items-end justify-between">
        <div>
          <h2 className="text-xl font-bold">External Backup Destinations</h2>
          <p className="text-xs uppercase tracking-wider text-on-surface-variant">Offsite &amp; network storage endpoints</p>
          {(() => {
            const g = dests.reduce((a, d) => a + Math.max(0, d.growth_bytes_per_month || 0), 0);
            return g > 0 ? <p className="mt-1 text-xs text-on-surface-variant">Estimated storage overhead ≈ <b className="text-on-surface">{fmtBytes(g)}/mo</b> across destinations</p> : null;
          })()}
        </div>
        <Button variant="primary" onClick={() => setAddOpen(true)}><Plus size={16} /> Add Storage Provider</Button>
      </div>

      <div className="tile-grid mt-4 [--tile-min:20rem]">
        {shownDests.map((d) => {
          const Icon = destIcon(d.type);
          const pct = d.total_bytes > 0 ? ((d.total_bytes - d.free_bytes) / d.total_bytes) * 100 : 0;
          return (
            <Card key={d.id} className="p-5">
              <div className="mb-4 flex items-start justify-between">
                <div className="grid h-10 w-10 place-items-center rounded bg-docker-blue/15 text-primary"><Icon size={20} /></div>
                <div className="flex items-center gap-2">
                  <span
                    className={`h-2.5 w-2.5 rounded-full status-pulse ${d.reachable ? "bg-success" : "bg-error"}`}
                    title={d.reachable ? "Reachable" : "Unreachable — this destination is down"}
                  />
                </div>
              </div>
              <div className="flex items-center gap-2">
                <span className="text-lg font-bold">{d.name}</span>
                {!d.reachable && <span className="text-xs font-medium text-error">Down</span>}
              </div>
              <div className="font-mono text-xs uppercase tracking-wider text-on-surface-variant">{d.type}</div>
              {d.total_bytes > 0 ? (
                <div className="mt-4">
                  <div className="mb-1 flex justify-between text-xs text-on-surface-variant">
                    <span>Capacity</span><span>{fmtBytes(d.total_bytes - d.free_bytes)} / {fmtBytes(d.total_bytes)}</span>
                  </div>
                  <div className="h-1.5 w-full overflow-hidden rounded-full bg-surface-highest"><div className="h-full bg-primary" style={{ width: `${Math.min(100, pct)}%` }} /></div>
                </div>
              ) : d.free_bytes > 0 ? (
                <div className="mt-4 text-xs text-on-surface-variant">{fmtBytes(d.free_bytes)} free</div>
              ) : d.used_bytes > 0 ? (
                <div className="mt-4 text-xs">
                  <div className="text-on-surface-variant">{fmtBytes(d.used_bytes)} used · <span className="text-warning">free space not reported</span></div>
                  <div className="mt-1 text-[11px] text-on-surface-variant">Unlimited quota — set a quota on this account so capacity shows and large backups are size-checked.</div>
                </div>
              ) : (
                <div className="mt-4 text-xs text-on-surface-variant">Capacity not reported</div>
              )}
              <DestForecastLine d={d} />
              <div className="mt-4 flex flex-wrap items-center justify-end gap-1">
                {backfills[d.id]?.running ? (
                  <span className="mr-auto flex items-center gap-1.5 px-1 text-xs text-secondary" title="Mirroring existing history to this destination, one upload at a time.">
                    <Loader2 size={13} className="animate-spin" /> Backfill · {backfills[d.id].done}/{backfills[d.id].total}{backfills[d.id].failed > 0 ? ` · ${backfills[d.id].failed} failed` : ""}
                  </span>
                ) : (
                  <Button variant="ghost" onClick={() => startBackfill(d)} title="Copy every existing backup that has no healthy copy here (one upload at a time; honors the upload window).">
                    <CloudUpload size={15} /> Backfill
                  </Button>
                )}
                <Button variant="ghost" onClick={() => adoptDest(d)} disabled={adoptingId === d.id} title="Look for backups on this destination that aren't in the catalog and re-import them.">
                  {adoptingId === d.id ? <Loader2 size={15} className="animate-spin" /> : <ScanSearch size={15} />} Scan &amp; adopt
                </Button>
                <Button variant="ghost" onClick={() => setEditDest(d)} title="Edit (change URL, password, path…)"><Pencil size={15} /> Edit</Button>
                <Button variant="ghost" onClick={() => removeDest(d)} title="Remove"><Trash2 size={15} /> Remove</Button>
              </div>
              <AdoptSkipList skips={adoptSkips[d.id]} onDismiss={() => setAdoptSkips((m) => ({ ...m, [d.id]: [] }))} />
            </Card>
          );
        })}
        <button onClick={() => setAddOpen(true)} className="group flex min-h-[180px] flex-col items-center justify-center rounded-lg border-2 border-dashed border-outline-variant/60 text-on-surface-variant transition-all hover:border-primary hover:text-primary">
          <div className="mb-3 grid h-12 w-12 place-items-center rounded-full border-2 border-dashed border-outline-variant group-hover:border-primary"><Plus size={22} /></div>
          <div className="font-semibold">Connect New Provider</div>
          <div className="px-6 text-center text-xs text-on-surface-variant">Synology, Nextcloud, or a Samba/SMB share</div>
        </button>
      </div>

      <AddDestinationModal open={addOpen} onClose={() => setAddOpen(false)} onAdded={loadDests} />
      <AddDestinationModal open={!!editDest} edit={editDest} onClose={() => setEditDest(null)} onAdded={() => { setEditDest(null); loadDests(); }} />
      </>)}
    </div>
  );
}

// TargetPicker lets the user select whole nodes or specific containers.
function TargetPicker({ nodes, targets, includeStopped, onChange }: { nodes: Node[]; targets: ScheduleTarget[]; includeStopped?: boolean; onChange: (t: ScheduleTarget[]) => void }) {
  const [open, setOpen] = useState<Record<string, boolean>>({});
  const [conts, setConts] = useState<Record<string, Container[]>>({});
  const [loaded, setLoaded] = useState<Record<string, boolean>>({});

  const isSpecific = (t: ScheduleTarget) => !!(t.container_id || t.container_name);
  const isStack = (t: ScheduleTarget) => !!t.stack;
  const hasWhole = (n: string) => targets.some((t) => t.node_id === n && !isSpecific(t) && !isStack(t));
  // Match a saved target to a live container by NAME (preferred, survives
  // recreation) or legacy ID.
  const matches = (t: ScheduleTarget, c: Container) =>
    (t.container_name ? t.container_name === c.name : t.container_id === c.id);
  const hasCont = (n: string, c: Container) => targets.some((t) => t.node_id === n && isSpecific(t) && matches(t, c));

  // Saved specific targets for a node whose container is NOT in the live list —
  // e.g. the user removed it. Surfaced so it can be cleaned up (PLAN §4.7).
  const missing = (n: string): ScheduleTarget[] => {
    if (!loaded[n]) return [];
    const live = conts[n] || [];
    const liveStacks = new Set(live.map((c) => c.stack).filter(Boolean));
    return targets.filter((t) => t.node_id === n && (
      (isSpecific(t) && !live.some((c) => matches(t, c))) ||
      (isStack(t) && !liveStacks.has(t.stack!))
    ));
  };

  const fetchConts = async (n: string) => {
    try { const d = await api.containers(n); setConts((c) => ({ ...c, [n]: d.containers })); }
    catch { /* leave unloaded */ }
    finally { setLoaded((l) => ({ ...l, [n]: true })); }
  };
  // Auto-load containers for nodes that already have specific targets, so stale
  // (removed-container) targets surface immediately without expanding.
  useEffect(() => {
    nodes.forEach((n) => {
      if (!loaded[n.id] && targets.some((t) => t.node_id === n.id && (isSpecific(t) || isStack(t)))) fetchConts(n.id);
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nodes, targets]);

  const toggleWhole = (n: string, on: boolean) => {
    let t = targets.filter((x) => !(x.node_id === n)); // drop all entries for this node
    if (on) t = [...t, { node_id: n }];
    onChange(t);
  };
  const toggleCont = (n: string, c: Container, on: boolean) => {
    let t = targets.filter((x) => !(x.node_id === n && isSpecific(x) && matches(x, c)));
    if (on) t = [...t, { node_id: n, container_id: c.id, container_name: c.name }];
    onChange(t);
  };
  const removeTarget = (t: ScheduleTarget) => onChange(targets.filter((x) => x !== t));

  // F47: whole-stack targets. Distinct compose projects come from the loaded
  // container list, so no extra fetch is needed.
  const stacksOf = (n: string) => [...new Set((conts[n] || []).map((c) => c.stack).filter(Boolean))].sort();
  const hasStack = (n: string, stack: string) => targets.some((t) => t.node_id === n && t.stack === stack);
  const stackConsistent = (n: string, stack: string) => !!targets.find((t) => t.node_id === n && t.stack === stack)?.consistent;
  const toggleStack = (n: string, stack: string, on: boolean) => {
    let t = targets.filter((x) => !(x.node_id === n && x.stack === stack));
    if (on) t = [...t, { node_id: n, stack }];
    onChange(t);
  };
  const toggleStackConsistent = (n: string, stack: string, on: boolean) =>
    onChange(targets.map((x) => (x.node_id === n && x.stack === stack ? { ...x, consistent: on } : x)));

  const expand = async (n: string) => {
    setOpen((o) => ({ ...o, [n]: !o[n] }));
    if (!loaded[n]) fetchConts(n);
  };

  // All stale targets across every node — for the one-click bulk cleanup (F13).
  const allMissing = nodes.flatMap((n) => missing(n.id));
  const removeAllMissing = () => {
    const dead = new Set(allMissing);
    onChange(targets.filter((t) => !dead.has(t)));
  };

  if (nodes.length === 0) return <div className="rounded border border-outline-variant bg-surface-lowest px-3 py-3 text-sm text-on-surface-variant">No nodes connected.</div>;

  return (
    <div className="overflow-hidden rounded border border-outline-variant">
      {allMissing.length > 0 && (
        <div className="flex items-center gap-2 border-b border-outline-variant/40 bg-warning/10 px-3 py-2 text-sm text-warning">
          <AlertTriangle size={14} className="shrink-0" />
          <span>{allMissing.length} scheduled target{allMissing.length === 1 ? "" : "s"} point at containers that no longer exist.</span>
          <button onClick={removeAllMissing} className="ml-auto rounded border border-warning/40 px-2 py-0.5 text-xs font-medium hover:bg-warning/10">Remove all missing</button>
        </div>
      )}
      <div className="divide-y divide-outline-variant/40">
      {nodes.map((n) => {
        const miss = missing(n.id);
        return (
        <div key={n.id} className="bg-surface-lowest">
          <div className="flex items-center gap-3 px-3 py-2.5 text-sm">
            <input type="checkbox" checked={hasWhole(n.id)} onChange={(e) => toggleWhole(n.id, e.target.checked)} />
            <Server size={15} className="text-primary" />
            <span className="font-medium">{n.name}</span>
            <span className="text-xs text-on-surface-variant">all {includeStopped ? "" : "running "}containers</span>
            {miss.length > 0 && (
              <span className="flex items-center gap-1 rounded bg-warning/10 px-1.5 py-0.5 text-[11px] font-medium text-warning" title="Scheduled containers that no longer exist">
                <AlertTriangle size={11} /> {miss.length} missing
              </span>
            )}
            <button onClick={() => expand(n.id)} className="ml-auto flex items-center gap-1 text-xs text-on-surface-variant hover:text-on-surface">
              {open[n.id] ? <ChevronDown size={14} /> : <ChevronRight size={14} />} specific
            </button>
          </div>
          {open[n.id] && (
            <div className="space-y-1 border-t border-outline-variant/40 bg-surface px-3 py-2 pl-9">
              {/* F47: whole compose stacks — optionally app-consistent. */}
              {stacksOf(n.id).length > 0 && (
                <div className="mb-1.5">
                  <div className="mb-1 text-[11px] font-semibold uppercase tracking-wide text-on-surface-variant">Stacks on this node</div>
                  {stacksOf(n.id).map((st) => {
                    const on = hasStack(n.id, st);
                    return (
                      <div key={`stk-${st}`} className="flex flex-col gap-0.5">
                        <label className="flex items-center gap-2 text-sm">
                          <input type="checkbox" checked={on} onChange={(e) => toggleStack(n.id, st, e.target.checked)} />
                          <Layers size={14} className="text-primary" />
                          <span className="font-medium">{st}</span>
                          <span className="text-[11px] text-on-surface-variant" title="Backs up every service in this project each run.">whole project</span>
                        </label>
                        {on && (
                          <label className="ml-6 flex items-center gap-2 text-xs text-on-surface-variant">
                            <input type="checkbox" checked={stackConsistent(n.id, st)} onChange={(e) => toggleStackConsistent(n.id, st, e.target.checked)} />
                            App-consistent (quiesce during capture)
                          </label>
                        )}
                      </div>
                    );
                  })}
                </div>
              )}
              {(conts[n.id] || []).map((c) => {
                const badge = c.state === "running" ? null : (c.state === "paused" ? "paused" : "stopped");
                // A whole-node target that excludes stopped containers won't cover a
                // stopped one — so keep its checkbox actionable (not force-checked) so
                // the user can still pin it as a specific target.
                const coveredByWhole = hasWhole(n.id) && (c.state === "running" || includeStopped);
                const coveredByStack = !!c.stack && hasStack(n.id, c.stack);
                const covered = coveredByWhole || coveredByStack;
                return (
                  <label key={c.id} className={`flex items-center gap-2 text-sm ${covered ? "opacity-40" : ""}`}>
                    <input type="checkbox" disabled={covered} checked={covered || hasCont(n.id, c)} onChange={(e) => toggleCont(n.id, c, e.target.checked)} />
                    {c.name} <span className="font-mono text-[11px] text-on-surface-variant">{c.image}</span>
                    {coveredByStack && <span className="rounded bg-primary/10 px-1.5 py-0.5 text-[10px] font-medium text-primary">in {c.stack}</span>}
                    {badge && <span className="rounded bg-on-surface-variant/10 px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wide text-on-surface-variant">{badge}</span>}
                  </label>
                );
              })}
              {(conts[n.id] || []).length === 0 && <div className="text-xs text-on-surface-variant">No containers.</div>}
              {/* Stale targets whose container was removed — offer to clean up. */}
              {miss.map((t, i) => (
                <div key={`m${i}`} className="flex items-center gap-2 text-sm text-warning">
                  <AlertTriangle size={13} className="shrink-0" />
                  <span className="line-through decoration-warning/50">{t.stack ? `stack: ${t.stack}` : (t.container_name || t.container_id)}</span>
                  <span className="text-[11px] not-italic">no longer exists</span>
                  <button onClick={() => removeTarget(t)} title="Remove from schedule" className="ml-auto rounded p-0.5 hover:bg-warning/10"><X size={13} /></button>
                </div>
              ))}
            </div>
          )}
        </div>
        );
      })}
      </div>
    </div>
  );
}
