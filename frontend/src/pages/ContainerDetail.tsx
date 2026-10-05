// Container backup-management page — matches stitch_docker_container_backup_hub(1).
// Live container stats, available backups, a wired "Run Manual Backup" panel
// (compression, target, options), and a live console. Nothing is hardcoded.
import { Fragment, useEffect, useRef, useState } from "react";
import StateChip from "../components/StateChip";
import { fmtDuration } from "../lib/format";
import { useParams, useNavigate, Link } from "react-router-dom";
import {
  ChevronRight, Terminal, Cpu, MemoryStick, CloudUpload, SlidersHorizontal, 
  CheckCircle2, XCircle, Loader2, AlertTriangle, ShieldCheck, HardDrive, X, ScrollText, History, Lock, Undo2,
} from "lucide-react";
import { followRun } from "../lib/logStream";
import { api, Backup, ContainerInfo, ContainerSizes, DrillStatus, RestoreCompatError, RestoreReadiness, StepUpError, fmtBytes, fmtAgo } from "../api";
import { isRestoreProgressLine } from "../lib/restoreLogFilter";
import { Button, Card, Chip, Modal } from "../components/ui";
import BackupCloudIcon from "../components/BackupCloudIcon";
import BackupConsole from "../components/BackupConsole";
import { RunLogPanel } from "../components/RunLog";
import { Sparkline } from "../components/Sparkline";
import { useToast } from "../components/Toast";
import { useStickyScroll } from "../hooks/useStickyScroll";
import RestoreTimeline from "../components/RestoreTimeline";
import StepUpPrompt from "../components/StepUpPrompt";
import { usePoll } from "../hooks/usePoll";

function backupLocations(b: Backup): { kind: string; name: string; type: string; status?: string; detail?: string }[] {
  let l: any[] = [];
  try { l = JSON.parse(b.locations || "[]"); } catch { l = []; }
  return l.length ? l : [{ kind: "local", name: "local", type: "local" }];
}

// F217: sealed to an OFFLINE key (F86) — restoring it needs the private key
// from its recovery sheet, which the quick-restore card deliberately does not
// ask for. Same test the restore drawer makes, from whichever of the two shapes
// this row arrived in: the paged list carries a computed summary, the
// container-detail payload carries the manifest itself.
function writeOnlyBackup(b: Backup): boolean {
  if (b.summary) return !!b.summary.write_only;
  try { return !!(JSON.parse(b.manifest_json || "{}") as { wrapped_key_pub?: string }).wrapped_key_pub; } catch { return false; }
}

// F217: why the one-click restore cannot take this backup — empty means it can.
// Each of these is a real DECISION, and the full restore drawer is where
// decisions are made: the card names the one that is waiting and links straight
// to it, rather than offering a confirm that would fail, or worse, quietly
// restoring something other than what the button says.
function quickRestoreBlock(b: Backup | null): string {
  if (!b) return "";
  if (b.verified === "failed") return "This backup did NOT pass verification. Restoring it in one click would be restoring something known to be wrong — open it in the full restore view to read the report first.";
  if (b.verified !== "verified") return "This backup has not been verified, so a one-click restore would be an unproven one. Open it in the full restore view to check it and restore from there.";
  if (writeOnlyBackup(b)) return "This backup is write-only encrypted: it opens only with the offline private key from its recovery sheet. Open it in the full restore view to paste the key.";
  if (b.key_mismatch) return "This backup was encrypted with a master key that is no longer the current one. Open it in the full restore view, which handles that case.";
  return "";
}


// rpoChoices are the selectable target RPO windows (seconds). 5 min is the floor
// enforced server-side so low-RPO can't overload the database (PLAN §9.7/§9.9).

export default function ContainerDetail() {
  const { id = "", cid = "" } = useParams();
  const navigate = useNavigate();
  const toast = useToast();
  const [c, setC] = useState<ContainerInfo | null>(null);
  const [nodeName, setNodeName] = useState("");
  const [backups, setBackups] = useState<Backup[]>([]);
  const [drills, setDrills] = useState<Record<string, DrillStatus>>({}); // restore-drill results, keyed by backup id (for the recovery timeline)
  const [logOpen, setLogOpen] = useState<Set<string>>(new Set()); // backup ids whose persisted run log is expanded (F8)
  const [totalBytes, setTotalBytes] = useState(0);
  const [sizes, setSizes] = useState<ContainerSizes | null>(null); // backup-size trend (F11)
  const [err, setErr] = useState("");
  const [msg, setMsg] = useState("");

  // Bridge between "backup accepted" (POST returned) and "backup visible in the
  // polled list": holds the accepted run's id so the button stays disabled with
  // no enabled gap — the gap is what made clicks feel unrecorded and invited
  // repeat clicks (each of which used to queue a whole extra run).
  const [pendingId, setPendingId] = useState<string | null>(null);
  // F69 ransomware tripwire: reason of an active mass-change retention hold ("" = none).
  const [tripwireHold, setTripwireHold] = useState("");
  const [tripwireClearing, setTripwireClearing] = useState(false);
  // F73 config drift: cheap boolean from the detail payload; the field breakdown
  // is lazy-loaded from the /drift endpoint only while the banner is showing.
  const [drift, setDrift] = useState<{ changed: boolean } | null>(null);
  const [driftFields, setDriftFields] = useState<string[] | null>(null);

  // F73: fetch the drift field breakdown once per shown banner.
  useEffect(() => {
    if (!drift?.changed || driftFields !== null) return;
    let cancelled = false;
    api.containerDrift(id, cid)
      .then((r) => { if (!cancelled) setDriftFields(r.fields || []); })
      .catch(() => { if (!cancelled) setDriftFields([]); });
    return () => { cancelled = true; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [drift?.changed, driftFields, id, cid]);


  // F217 one-click restore of the newest verified backup (see runQuickRestore).
  const [qrOpen, setQrOpen] = useState(false);
  const [qrTarget, setQrTarget] = useState<Backup | null>(null); // the backup the open card would restore
  const [qrReady, setQrReady] = useState<RestoreReadiness | null>(null);
  const [qrReadyErr, setQrReadyErr] = useState("");
  const [qrChecking, setQrChecking] = useState(false);
  const [qrSnapshot, setQrSnapshot] = useState(true); // safety snapshot first (PLAN §3.7), on by default
  const [qrState, setQrState] = useState<"idle" | "running" | "done" | "failed" | "canceled">("idle");
  const [qrErr, setQrErr] = useState("");
  const [qrLines, setQrLines] = useState<{ level: string; msg: string }[]>([]);
  const [qrStepUp, setQrStepUp] = useState<{ totp: boolean; err: string } | null>(null);
  const [qrCanceling, setQrCanceling] = useState(false);
  const qrEsRef = useRef<(() => void) | null>(null);
  const qrLog = useStickyScroll(qrLines.length);
  // Leaving the page must not leave the stream open behind it.
  useEffect(() => () => { qrEsRef.current?.(); }, []);

  const load = async () => {
    try {
      const d = await api.containerDetail(id, cid);
      setC(d.container); setNodeName(d.node_name || ""); setBackups(d.backups || []); setTotalBytes(d.total_bytes); 
      setTripwireHold(d.tripwire_hold || "");
      setDrift(d.drift || null);
      if (!d.drift?.changed) setDriftFields(null); // banner cleared (fresh backup) → reset the breakdown
      // The pause/autosnap/bind/timeout seeds moved with the form they seed.
      // Only seed the hook editor once (don't clobber edits on the 6s poll).
      // The remembered backup options seed the form, which moved.
    } catch (e) { setErr((e as Error).message); }
  };
  useEffect(() => {
    load();
    // Destination selection moved with the form.
    // F62 standby, the notification nudge and the fleet node list all fed the
    // backup card and moved with it.
  }, [id, cid]);
  usePoll(load, 6000, [id, cid]);

  // F62: standby rehearsal actions.

  // Restore-drill results feed the recovery timeline's "drill-proven" state.
  // Light, fleet-wide call; refreshed slowly so a drill's outcome appears.
  useEffect(() => {
    const f = () => api.drills().then((arr) => {
      const m: Record<string, DrillStatus> = {};
      for (const d of arr) m[d.backup_id] = d;
      setDrills(m);
    }).catch(() => {});
    f();
  }, []);
  usePoll(() => api.drills().then((arr) => {
    const m: Record<string, DrillStatus> = {};
    for (const d of arr) m[d.backup_id] = d;
    setDrills(m);
  }).catch(() => {}), 15000);

  // True while a backup of THIS container is running (from the polled list).
  const backupInProgress = !!backups?.some((b) => b.status === "running");

  // F217: the newest SUCCESSFUL backup, by time rather than by list position —
  // the quick path must never restore an older row because the list arrived in
  // an order this page did not choose.
  const latest = backups.reduce<Backup | null>(
    (best, b) => (b.status === "success" && (!best || b.created_at > best.created_at) ? b : best), null);
  // What the OPEN card is about. Frozen when the dialog opens rather than read
  // from the list, because the list keeps polling: a scheduled backup finishing
  // mid-decision would otherwise change what "Restore now" restores without the
  // operator seeing it — and after any restore the newest backup of this
  // container is that restore's own pre-restore snapshot.
  const target = qrTarget;
  const quickBlock = target ? quickRestoreBlock(target) : "";
  // Everything that stands between the operator and the confirm button, once the
  // backup itself is eligible. Advisory findings are NOT in here — the drawer
  // shows those; this is only what makes the restore wrong to start.
  const quickStop = !target ? "No successful backup to restore yet."
    : quickBlock ? quickBlock
      : qrReadyErr ? `Couldn't check restore readiness: ${qrReadyErr}`
        : qrReady?.restore_block ? qrReady.restore_block
          : backupInProgress ? "A backup of this container is running. Let it finish first — a restore would overwrite the data it is still reading."
            : "";
  const quickDeepLink = target ? `/backups?node=${id}&open=${target.id}` : `/backups?node=${id}`;

  // Critical-DB (low-RPO) status moved with the backup card.

  // Reset the (per-container) mount data + pending-run bridge when switching containers.
  useEffect(() => { setPendingId(null); }, [id, cid]);

  // Backup-size trend (F11): refresh when the container changes or a new backup
  // lands (backups.length grows), not on every 6s poll — keeps it cheap.
  useEffect(() => {
    api.containerSizes(id, cid).then(setSizes).catch(() => setSizes(null));
  }, [id, cid, backups.length]);

  const cancelBackup = async (bid: string) => {
    try { await api.cancelBackup(bid); load(); } catch (e) { setErr((e as Error).message); }
  };

  // F225: starting a backup moved to the backup page with the form that
  // configures it. What is left here links there.

  // Resolve the pending run once it shows up in the polled list. Large
  // containers surface as "running" (the in-progress state takes over); small
  // ones can finish between polls — surface THAT explicitly, so a backup that
  // completed in under a second doesn't look like a click that never happened.
  useEffect(() => {
    if (!pendingId) return;
    const row = backups.find((b) => b.id === pendingId);
    if (!row) return;
    setPendingId(null);
    if (row.status === "success") toast.success(`Backup of ${c?.name || row.target_name} completed.`);
    else if (row.status === "failed") toast.error(`Backup of ${c?.name || row.target_name} failed${row.error ? `: ${row.error}` : ""}.`);
    // "running" needs no toast — the button flips to "Backup in progress…".
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [backups, pendingId]);

  // Failsafe: never leave the button stuck if the accepted run's row can't be
  // observed (e.g. it stays queued behind other work on this node for a long
  // time). Re-enabling is safe — the server now coalesces duplicate requests.
  useEffect(() => {
    if (!pendingId) return;
    const t = setTimeout(() => setPendingId(null), 90000);
    return () => clearTimeout(t);
  }, [pendingId]);

  const memPct = c && c.mem_limit > 0 ? (c.mem_used / c.mem_limit) * 100 : 0;
  // F217 — one-click "Restore latest", from the page the container is on.
  //
  // The shortest same-host restore used to be: pick a point on the recovery
  // timeline, land on /backups, find the row again, open the drawer, cross its
  // options, confirm. Six interactions across two pages to do the one thing this
  // page is about. This card is that path for the ordinary case and NOTHING
  // else: newest VERIFIED backup, same container, same host, safety snapshot
  // first. Every restore that needs a decision — another version, another host,
  // an address remap, an offline key, a copy — still opens the drawer, because
  // that is where those decisions are made. The quick path never guesses one.
  const runQuickRestore = async (confirmIncompatible: boolean, stepUp?: { password: string; code: string }) => {
    if (!target) return;
    const b = target;
    setQrState("running"); setQrLines([]); setQrErr("");

    // Follow THIS backup's restore. Restore lines are keyed by the backup id, not
    // by container — the page's own console filters on container_id and so never
    // shows them, which is why this dialog carries its own view of the run.
    const start = Date.now();
    qrEsRef.current?.();
    qrEsRef.current = followRun(b.id, {
      mode: "one",
      since: start - 2000, // skip replayed history
      filter: isRestoreProgressLine,
      onLine: (l) => setQrLines((p) => [...p.slice(-60), { level: l.level, msg: l.msg }]),
      onDone: (outcome, message) => {
        if (outcome === "ok") { setQrState("done"); toast.success(`${b.target_name} restored.`); }
        else if (outcome === "canceled") { setQrState("canceled"); setQrErr(message); }
        else { setQrState("failed"); setQrErr(message); }
        load();
      },
    });
    try {
      await api.restore(b.id, {
        node_id: id, target_id: cid, volumes: true, database: true, confirm: true, snapshot: qrSnapshot,
        ...(confirmIncompatible ? { confirm_incompatible: true } : {}),
        ...(stepUp ? { password: stepUp.password, code: stepUp.code } : {}),
      });
      setQrStepUp(null); // F206: satisfied — clear the prompt
    } catch (e) {
      qrEsRef.current?.();
      // The engine's compatibility gate (a dump taken by a newer server than the
      // one about to import it). Same wording and same one retry the drawer uses
      // — this is exactly the case where a quick path must not decide for you.
      if (e instanceof RestoreCompatError) {
        setQrState("idle"); setQrLines([]);
        if (confirm(`Restore compatibility warning\n\n${e.compat_warning}\n\nRestoring anyway can corrupt or fail. Continue?`)) {
          return runQuickRestore(true);
        }
        return;
      }
      // F206: overwriting a PROTECTED container asks for the password again. A
      // prompt, not a failure — and never a logout, which is what a bare 401
      // would have caused before api.restore learned to tell the two apart.
      if (e instanceof StepUpError) {
        setQrState("idle"); setQrLines([]);
        setQrStepUp({ totp: e.totp_required, err: stepUp ? e.message : "" });
        return;
      }
      setQrState("failed"); setQrErr((e as Error).message);
    }
  };

  // Stop a running restore. The engine stops at the next safe point and rolls
  // nothing back on its own — the wording says so, because "cancel" reads like
  // "undo" and here it is not.
  const cancelQuickRestore = async () => {
    if (!target) return;
    if (!confirm(
      "Cancel this restore?\n\n" +
      "DockBack stops at the next safe point and does NOT roll anything back automatically. " +
      "Data already written stays written — the log will name the pre-restore snapshot (if one was taken) so you can undo it deliberately."
    )) return;
    setQrCanceling(true);
    try { await api.cancelRestore(target.id); }
    catch (e) { setQrErr(`Couldn't cancel: ${(e as Error).message}`); }
    finally { setQrCanceling(false); }
  };

  // Fetch the readiness verdict for the backup this card would restore. The
  // quick path REQUIRES an answer: restore_block (F174) is computed here and
  // nowhere else, so "couldn't check" has to mean "not from this card" rather
  // than "proceed and hope" — the drawer stays one click away either way.
  const checkQuickReadiness = (b: Backup | null) => {
    if (!b) return;
    setQrChecking(true); setQrReadyErr("");
    api.restoreReadiness(b.id)
      .then((r) => setQrReady(r))
      .catch((e) => { setQrReady(null); setQrReadyErr((e as Error).message); })
      .finally(() => setQrChecking(false));
  };

  const openQuickRestore = () => {
    if (!latest) return;
    // Re-attach to a run already in flight rather than resetting it — the dialog
    // says closing it does not stop the restore, so reopening must find it.
    if (qrState === "running") { setQrOpen(true); return; }
    setQrTarget(latest);
    setQrOpen(true); setQrState("idle"); setQrErr(""); setQrLines([]); setQrStepUp(null);
    setQrSnapshot(true); setQrReady(null); setQrReadyErr("");
    if (!quickRestoreBlock(latest)) checkQuickReadiness(latest); // a blocked backup is the drawer's job; don't spend the call
  };

  const closeQuickRestore = () => {
    if (qrState !== "running") { qrEsRef.current?.(); qrEsRef.current = null; }
    setQrOpen(false);
  };

  return (
    <div>
      {/* Breadcrumb */}
      <nav className="mb-6 flex items-center gap-2 text-xs uppercase tracking-wider text-on-surface-variant">
        <Link to="/servers" className="hover:text-primary">Servers</Link>
        <ChevronRight size={14} />
        <Link to={`/servers/${id}`} className="hover:text-primary">{nodeName || id}</Link>
        <ChevronRight size={14} />
        <span className="font-semibold text-on-surface">{c?.name || cid.slice(0, 12)}</span>
      </nav>

      {err && <div className="mb-4 rounded bg-error/10 px-4 py-3 text-error">{err}</div>}
      {msg && <div className="mb-4 rounded bg-secondary/10 px-4 py-3 text-secondary">{msg}</div>}

      {/* F69 ransomware tripwire: retention is frozen after a mass-change event until reviewed. */}
      {tripwireHold && (
        <div className="mb-4 rounded border border-error/40 bg-error/10 p-4">
          <div className="flex items-center gap-2 font-semibold text-error"><AlertTriangle size={16} className="shrink-0" /> Retention on hold — review this container's backups</div>
          <p className="mt-1 text-sm text-on-surface-variant">
            A recent backup's incremental delta looked like a mass-change event ({tripwireHold}). Pruning is frozen for this container so clean pre-event
            backups cannot be aged out. Review the backups below — restore or drill a pre-event one if in doubt — then clear the hold to resume normal retention.
          </p>
          <div className="mt-2">
            <Button
              variant="danger"
              disabled={tripwireClearing}
              onClick={async () => {
                if (!confirm("Clear the mass-change hold?\n\nRetention pruning resumes for this container and its suspect flags are reset. Only do this after reviewing the flagged backups.")) return;
                setTripwireClearing(true);
                try { await api.clearTripwire(id, cid); setTripwireHold(""); setMsg("Tripwire hold cleared — retention resumes for this container."); load(); }
                catch (e) { setErr((e as Error).message); }
                finally { setTripwireClearing(false); }
              }}
            >
              {tripwireClearing ? <Loader2 size={15} className="animate-spin" /> : <ShieldCheck size={15} />} Clear hold
            </Button>
          </div>
        </div>
      )}

      {/* F73 config drift: the live container no longer matches the newest backup's captured config. */}
      {drift?.changed && (
        <div className="mb-4 rounded border border-warning/40 bg-warning/10 p-4">
          <div className="flex items-center gap-2 font-semibold text-warning">
            <AlertTriangle size={16} className="shrink-0" />
            Configuration changed since the last backup{driftFields && driftFields.length > 0 ? `: ${driftFields.join(", ")}` : ""}
          </div>
          <p className="mt-1 text-sm text-on-surface-variant">
            The running container's configuration differs from what the newest backup captured — restoring that backup would bring back the <b>older</b> configuration.
            If the change is intentional, take a fresh backup so it's protected too.
          </p>
          <div className="mt-2">
            <Link to={`/servers/${id}/containers/${cid}/backup`}
              className="inline-flex items-center gap-1.5 rounded border border-outline-variant bg-surface-high/40 px-3 py-1.5 text-sm font-medium text-on-surface hover:bg-surface-high">
              <HardDrive size={15} /> Back up now to capture it
            </Link>
          </div>
        </div>
      )}

      <div className="grid grid-cols-1 gap-6 lg:grid-cols-3">
        {/* LEFT: header + available backups */}
        <div className="space-y-6 lg:col-span-2">
          {/* Container header */}
          <Card className="relative overflow-hidden p-5">
            {/* F172: decoration, and nothing else — no state, no meaning, no
                interaction. Sized and placed to sit ENTIRELY inside the card:
                at 120px with negative offsets the card's overflow-hidden cut it
                mid-stroke, which read as a rendering fault rather than a
                watermark. The colour is theme-aware (see .card-watermark) and
                it is hidden from assistive technology. */}
            <Terminal size={62} aria-hidden="true" className="card-watermark pointer-events-none absolute right-3 top-3" />
            <div className="mb-3 flex flex-wrap items-center gap-3">
              <StateChip state={c?.state} />
              <span className="font-mono text-xs text-on-surface-variant">ID: {(c?.id || cid).slice(0, 12)}</span>
              {/* F225: the backup form and everything remembered about this
                  container's protection moved to their own page. What is left
                  here is the container: what it is, its backups, its console. */}
              <div className="ml-auto flex flex-wrap gap-2">
                <Link to={`/servers/${id}/containers/${cid}/backup?tab=schedule`}
                  className="inline-flex items-center gap-1.5 rounded border border-outline-variant bg-surface-high/40 px-3 py-1.5 text-sm font-medium text-on-surface hover:bg-surface-high">
                  <SlidersHorizontal size={15} /> Backup settings
                </Link>
                <Link to={`/servers/${id}/containers/${cid}/backup`}
                  className="inline-flex items-center gap-1.5 rounded bg-docker-blue px-3 py-1.5 text-sm font-medium text-white hover:bg-[#1f86d6]">
                  <BackupCloudIcon size={15} active={backupInProgress} /> {backupInProgress ? "Backup in progress…" : "Back up now"}
                </Link>
              </div>
            </div>
            <h1 className="text-2xl font-bold">{c?.name || "…"}</h1>
            {c?.description && <p className="mt-2 max-w-2xl text-on-surface-variant">{c.description}</p>}
            <div className="mt-6 grid grid-cols-2 gap-6 border-t border-outline-variant/50 pt-4 md:grid-cols-3">
              <Field label="Image" value={c?.image} mono />
              <Field label="Uptime" value={c?.uptime || (c?.state === "running" ? "—" : "stopped")} />
              <Field label="IP Address" value={c?.ip || "—"} mono />
            </div>
          </Card>

          {/* Available backups */}
          <Card className="overflow-hidden">
            <div className="flex flex-wrap items-center justify-between gap-2 px-6 pt-5">
              <h2 className="text-xl font-bold">Available Backups</h2>
              {/* F217: the restore action belongs beside the backups it acts on.
                  Hidden outright when there is nothing successful to restore —
                  a disabled button here would only pose a question the page has
                  already answered two lines below. */}
              {latest && (
                <Button variant="secondary" onClick={openQuickRestore} title={`Restore ${c?.name || "this container"} from its newest backup, in place, on this host`}>
                  {qrState === "running" && !qrOpen
                    ? <><Loader2 size={15} className="animate-spin" /> Restoring…</>
                    : <><Undo2 size={15} /> Restore…</>}
                </Button>
              )}
            </div>
            {/* Recovery timeline (C9) — scrub versions; click a point to restore/revert. */}
            {backups.filter((b) => b.status === "success").length > 1 && (
              <div className="mt-4 border-b border-outline-variant/40 px-6 pb-4">
                <div className="mb-2 text-xs font-semibold uppercase tracking-widest text-on-surface-variant">Recovery timeline</div>
                <RestoreTimeline backups={backups} drills={drills} onPick={(b) => navigate(`/backups?node=${id}&open=${b.id}`)} />
              </div>
            )}
            <table className="mt-4 w-full text-left text-sm">
              <thead className="border-y border-outline-variant/60 bg-surface-lowest/40 text-xs uppercase tracking-widest text-on-surface-variant">
                <tr>
                  <th className="px-4 py-2.5 font-semibold">Status</th>
                  <th className="px-4 py-2.5 font-semibold">Backup Name / Date</th>
                  <th className="px-4 py-2.5 font-semibold">Location(s)</th>
                  <th className="px-4 py-2.5 font-semibold">Size</th>
                  <th className="px-4 py-2.5 text-right font-semibold">Retention</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-outline-variant/30">
                {backups.map((b, i) => {
                  const showLog = logOpen.has(b.id);
                  const toggleLog = () => setLogOpen((s) => { const n = new Set(s); n.has(b.id) ? n.delete(b.id) : n.add(b.id); return n; });
                  return (
                  <Fragment key={b.id}>
                  <tr className="hover:bg-surface-highest/40">
                    <td className="px-4 py-3"><BackupStatus b={b} /></td>
                    <td className="px-4 py-3">
                      <div className="font-medium text-on-surface">{baseName(b.storage_key) || b.id.slice(0, 12)}</div>
                      <div className="text-xs text-on-surface-variant">{new Date(b.created_at * 1000).toLocaleString()}</div>
                    </td>
                    <td className="px-4 py-3">
                      <div className="flex flex-wrap gap-1">
                        {backupLocations(b).map((l, j) => (
                          l.status === "failed" ? (
                            // Intended offsite copy that did NOT upload — show it as failed, never as a real copy.
                            <span key={j} title={l.detail || "offsite upload failed"} className="inline-flex items-center gap-1 rounded bg-error/10 px-1.5 py-0.5 text-[10px] text-error">
                              <CloudUpload size={10} /> {l.name} failed
                            </span>
                          ) : (
                            <span key={j} className={`inline-flex items-center gap-1 rounded px-1.5 py-0.5 text-[10px] ${l.kind === "local" ? "bg-surface-high text-on-surface-variant" : "bg-secondary/10 text-secondary"}`}>
                              {l.kind === "local" ? <HardDrive size={10} /> : <CloudUpload size={10} />}{l.kind === "local" ? "Local" : l.name}
                            </span>
                          )
                        ))}
                      </div>
                    </td>
                    <td className="px-4 py-3 tnum">
                      {b.status === "running"
                        ? <div className="h-1 w-24 overflow-hidden rounded-full bg-surface-highest"><div className="h-full w-1/2 animate-pulse bg-gradient-to-r from-secondary to-primary" /></div>
                        : (
                          <>
                            <div>{fmtBytes(b.size_bytes)}</div>
                            {b.status === "success" && b.duration_ms ? <div className="text-[11px] text-on-surface-variant" title="How long this backup took">{fmtDuration(b.duration_ms / 1000)}</div> : null}
                          </>
                        )}
                    </td>
                    <td className="px-4 py-3 text-right">
                      <div className="flex items-center justify-end gap-2">
                        {b.status !== "running" && (
                          <button onClick={toggleLog} title="Show this run's stored log" className={`inline-flex items-center gap-1 rounded border px-2 py-0.5 text-xs ${showLog ? "border-primary text-primary" : "border-outline-variant text-on-surface-variant hover:text-on-surface"}`}>
                            <ScrollText size={12} /> Log
                          </button>
                        )}
                        {/* A "Keep (N gen)" / "Prunable" chip stood here. It judged
                            from this row's POSITION and the global generations
                            count alone, so it ignored the container's own retention
                            override, the grandfather-father-son rules, a pin, a WORM
                            lock, an incremental chain a newer backup depends on, and
                            the separate auto-snapshot budget — telling operators a
                            backup was safe when the policy would prune it, and
                            prunable when nothing would touch it. Settings →
                            "Preview pruning (dry run)" answers it against the real
                            policy. */}
                        {b.status === "running"
                          ? <button onClick={() => cancelBackup(b.id)} className="rounded border border-error/40 px-2 py-0.5 text-xs text-error hover:bg-error/10">Cancel</button>
                          : b.status !== "success" && <span className="text-on-surface-variant">—</span>}
                      </div>
                    </td>
                  </tr>
                  {showLog && (
                    <tr>
                      <td colSpan={5} className="px-4 pb-3">
                        <RunLogPanel backupId={b.id} collapsible={false} />
                      </td>
                    </tr>
                  )}
                  </Fragment>
                  );
                })}
                {backups.length === 0 && <tr><td colSpan={5} className="px-6 py-10 text-center text-on-surface-variant">No backups yet for this container.</td></tr>}
              </tbody>
            </table>
            <div className="px-4 py-3 text-xs text-on-surface-variant">Showing {backups.length} backup{backups.length === 1 ? "" : "s"}</div>
          </Card>

          {/* Console output — lives under the backups to fill the left column */}
          <BackupConsole containerId={cid} />
        </div>

        {/* RIGHT: performance + run manual backup */}
        <div className="space-y-6">
          <Card className="p-5">
            <div className="mb-4 text-xs font-semibold uppercase tracking-widest text-on-surface-variant">Performance Metrics</div>
            <Metric icon={<Cpu size={13} />} label="CPU Usage" pct={c?.cpu_percent || 0} text={`${(c?.cpu_percent || 0).toFixed(1)}%`} />
            <div className="h-3" />
            <Metric icon={<MemoryStick size={13} />} label="Memory" pct={memPct} text={c ? `${fmtBytes(c.mem_used)} / ${c.mem_limit ? fmtBytes(c.mem_limit) : "∞"}` : "—"} />
            <div className="mt-5 grid grid-cols-3 gap-2 border-t border-outline-variant/50 pt-4 text-center">
              <BigStat value={`${backups.length}`} label="Backups" />
              <BigStat value={fmtBytes(totalBytes)} label="Total Storage" />
              <BigStat value={healthText(c)} label="Health" />
            </div>
            {sizes && sizes.points.length >= 2 && (
              <div className="mt-4 flex items-center justify-between gap-3 border-t border-outline-variant/50 pt-4">
                <div>
                  <div className="text-xs text-on-surface-variant">Backup size trend</div>
                  <div className="mt-0.5 text-sm font-semibold text-on-surface">
                    {sizes.growth_bytes_per_month > 0
                      ? <span className="text-warning">+{fmtBytes(sizes.growth_bytes_per_month)}/mo</span>
                      : sizes.growth_bytes_per_month < 0
                        ? <span className="text-secondary">−{fmtBytes(-sizes.growth_bytes_per_month)}/mo</span>
                        : <span className="text-on-surface-variant">stable</span>}
                  </div>
                </div>
                <Sparkline points={sizes.points} className={sizes.growth_bytes_per_month > 0 ? "text-warning" : "text-secondary"} />
              </div>
            )}
          </Card>

        </div>
      </div>

      {/* F217: the one-click restore card. Deliberately small — it confirms one
          restore and offers exactly one choice (the safety snapshot). Everything
          else is a link to the drawer, which is unchanged. */}
      {target && (
        <Modal
          open={qrOpen}
          onClose={closeQuickRestore}
          title="Restore latest backup"
          footer={
            qrState === "running" ? (
              <>
                <Button variant="danger" disabled={qrCanceling} onClick={cancelQuickRestore}>
                  {qrCanceling ? <Loader2 size={15} className="animate-spin" /> : <X size={15} />} {qrCanceling ? "Canceling…" : "Cancel restore"}
                </Button>
                <Button variant="ghost" onClick={closeQuickRestore}>Close</Button>
              </>
            ) : qrState === "done" || qrState === "failed" || qrState === "canceled" ? (
              <Button variant="secondary" onClick={closeQuickRestore}>Close</Button>
            ) : (
              <>
                <Link to={quickDeepLink} className="inline-flex items-center gap-1.5 rounded border border-outline-variant bg-surface-high/40 px-3 py-1.5 text-sm font-medium text-on-surface hover:bg-surface-high">
                  <History size={15} /> More options…
                </Link>
                <Button variant="ghost" onClick={closeQuickRestore}>Cancel</Button>
                <Button variant="danger" disabled={!!quickStop || qrChecking || !!qrStepUp} onClick={() => runQuickRestore(false)}
                  title={quickStop || "Restore this container's volumes and database from the newest backup, in place"}>
                  <Undo2 size={15} /> Restore now
                </Button>
              </>
            )
          }
        >
          <div className="space-y-3 text-sm">
            {/* What would be restored. */}
            <div className="rounded border border-outline-variant/60 bg-surface-lowest p-3">
              <div className="flex flex-wrap items-center gap-2">
                <span className="min-w-0 break-all font-medium text-on-surface">{baseName(target.storage_key) || target.id.slice(0, 12)}</span>
                {target.verified === "verified"
                  ? <Chip kind="ok"><ShieldCheck size={11} /> Verified</Chip>
                  : target.verified === "failed"
                    ? <Chip kind="err"><XCircle size={11} /> Verification failed</Chip>
                    : <Chip kind="warn"><AlertTriangle size={11} /> Unverified</Chip>}
                {writeOnlyBackup(target) && <Chip kind="info"><Lock size={11} /> Write-only</Chip>}
                {target.label && <Chip kind="muted">{target.label}</Chip>}
              </div>
              {/* The newest backup of a container that was JUST restored is that
                  restore's own safety snapshot. Restoring it is a legitimate
                  thing to want — it undoes the restore — but it is not what
                  "restore the latest backup" sounds like, so the card says which
                  one this is instead of letting the word "latest" imply it. */}
              {(target.label || "").startsWith("auto: pre-restore") && (
                <p className="mt-1.5 break-words text-xs text-warning">
                  This is the safety snapshot taken before the last restore &mdash; restoring it <b>undoes</b> that restore rather than repeating it. For an earlier version, use <b>More options</b>.
                </p>
              )}
              <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-on-surface-variant">
                <span className="tnum">{new Date(target.created_at * 1000).toLocaleString()}</span>
                <span aria-hidden="true">·</span>
                <span className="tnum">{fmtAgo(target.created_at)}</span>
                <span aria-hidden="true">·</span>
                <span className="tnum">{fmtBytes(target.size_bytes)}</span>
              </div>
            </div>

            {/* Where it goes. In place, on this host, onto this container — the
                three facts that make the other 14 options in the drawer
                unnecessary, stated rather than assumed. */}
            <p className="break-words text-on-surface-variant">
              Restores <b className="text-on-surface">{c?.name || cid.slice(0, 12)}</b>&rsquo;s volumes and database <b className="text-on-surface">in place</b> on <b className="text-on-surface">{nodeName || id}</b>, then starts it.
              This overwrites its live data. For another version, another host, an address change or a throwaway copy, use <b className="text-on-surface">More options</b>.
            </p>

            {/* Readiness. The card asks for it on open and will not confirm
                without an answer — restore_block is computed nowhere else. */}
            {!quickBlock && (qrChecking || qrReadyErr || qrReady) && (
              <div className="flex items-start gap-2 rounded bg-surface-high/40 px-3 py-2 text-xs">
                {qrChecking ? (
                  <><Loader2 size={14} className="mt-0.5 shrink-0 animate-spin text-on-surface-variant" /><span className="text-on-surface-variant">Checking restore readiness…</span></>
                ) : qrReadyErr ? (
                  <>
                    <AlertTriangle size={14} className="mt-0.5 shrink-0 text-warning" />
                    <span className="min-w-0 break-words text-warning">
                      Couldn&rsquo;t check restore readiness: {qrReadyErr}.{" "}
                      <button onClick={() => checkQuickReadiness(target)} className="underline hover:text-on-surface">Try again</button>
                    </span>
                  </>
                ) : qrReady?.image_ok ? (
                  <><ShieldCheck size={14} className="mt-0.5 shrink-0 text-success" /><span className="min-w-0 break-words text-on-surface-variant"><span className="font-medium text-success">Image ready</span> — {qrReady.detail}</span></>
                ) : qrReady ? (
                  <><AlertTriangle size={14} className="mt-0.5 shrink-0 text-warning" /><span className="min-w-0 break-words text-warning">{qrReady.detail}</span></>
                ) : null}
              </div>
            )}

            {/* AC3: a container whose own configuration would stop this restore
                (F174) — named, in error styling, with the confirm disabled. */}
            {qrReady?.restore_block && (
              <div className="flex items-start gap-2 rounded border border-error/40 bg-error/10 px-3 py-2 text-xs text-error">
                <XCircle size={14} className="mt-0.5 shrink-0" />
                <span className="min-w-0 break-words">{qrReady.restore_block}</span>
              </div>
            )}

            {/* Why this backup is the drawer's job, not this card's. */}
            {quickBlock && (
              <div className="flex items-start gap-2 rounded border border-warning/40 bg-warning/10 px-3 py-2 text-xs text-warning">
                <AlertTriangle size={14} className="mt-0.5 shrink-0" />
                <span className="min-w-0 break-words">{quickBlock}</span>
              </div>
            )}

            {/* The one choice this card offers. */}
            <label className="flex cursor-pointer items-start gap-2 text-xs">
              <input type="checkbox" className="mt-0.5 shrink-0" checked={qrSnapshot} onChange={(e) => setQrSnapshot(e.target.checked)} disabled={qrState !== "idle"} />
              <span className="min-w-0 break-words text-on-surface-variant">
                <b className="text-on-surface">Take a safety snapshot first</b> — backs up the current state before overwriting it, so this restore can be undone.
                {!qrSnapshot && <span className="text-warning"> Without it, this overwrite is not reversible.</span>}
              </span>
            </label>

            {/* F206: protected container — said before the confirm, so the
                password request is a stated condition and not a surprise. */}
            {qrReady?.step_up_required && !qrStepUp && qrState === "idle" && (
              <p className="flex items-start gap-2 rounded bg-primary/[0.08] px-3 py-2 text-xs text-on-surface-variant">
                <Lock size={14} className="mt-0.5 shrink-0 text-primary" />
                <span className="min-w-0 break-words">This container is protected: overwriting its live data asks for your password first.</span>
              </p>
            )}
            {qrStepUp && (
              <StepUpPrompt totp={qrStepUp.totp} busy={qrState === "running"} error={qrStepUp.err}
                confirmLabel="Confirm and restore"
                onConfirm={(pw, code) => runQuickRestore(false, { password: pw, code })} />
            )}

            {/* Live progress, and the terminal result. */}
            {qrState === "running" && (
              <div className="flex flex-wrap items-center gap-2 rounded bg-secondary/10 px-3 py-2 text-xs text-secondary">
                <Loader2 size={15} className="shrink-0 animate-spin" />
                <span className="min-w-0 break-words">Restoring {c?.name || target.target_name}… closing this dialog does not stop it.</span>
              </div>
            )}
            {qrState === "done" && (
              <div className="flex items-start gap-2 rounded bg-success/10 px-3 py-2 text-xs text-success">
                <CheckCircle2 size={15} className="mt-0.5 shrink-0" />
                <span className="min-w-0 break-words">Restore completed. {qrSnapshot ? "The safety snapshot taken first is in this container&rsquo;s backup list, labelled as a pre-restore snapshot." : "No safety snapshot was taken."}</span>
              </div>
            )}
            {qrState === "canceled" && (
              <div className="flex items-start gap-2 rounded bg-warning/10 px-3 py-2 text-xs text-warning">
                <AlertTriangle size={15} className="mt-0.5 shrink-0" />
                <span className="min-w-0 break-words">Restore canceled{qrErr ? `: ${qrErr}` : "."} Nothing was rolled back automatically — the log says what state it was left in.</span>
              </div>
            )}
            {qrState === "failed" && (
              <div className="flex items-start gap-2 rounded bg-error/10 px-3 py-2 text-xs text-error">
                <XCircle size={15} className="mt-0.5 shrink-0" />
                <span className="min-w-0 break-words">{qrErr || "Restore failed."}</span>
              </div>
            )}
            {qrLines.length > 0 && (
              <div ref={qrLog.ref} onScroll={qrLog.onScroll} className="max-h-40 overflow-y-auto rounded border border-outline-variant bg-surface-lowest p-3 font-mono text-xs">
                {qrLines.map((l, i) => (
                  <div key={i} className={`break-words ${l.level === "ERR" ? "text-error" : "text-on-surface-variant"}`}>{l.msg}</div>
                ))}
              </div>
            )}
          </div>
        </Modal>
      )}
    </div>
  );
}

function Field({ label, value, mono }: { label: string; value?: string; mono?: boolean }) {
  return (
    <div>
      <div className="text-xs uppercase tracking-wider text-on-surface-variant">{label}</div>
      <div className={`mt-1 ${mono ? "font-mono text-sm" : "font-medium"}`}>{value || "—"}</div>
    </div>
  );
}
function Metric({ icon, label, pct, text }: { icon: React.ReactNode; label: string; pct: number; text: string }) {
  return (
    <div className="space-y-1.5">
      <div className="flex items-center justify-between text-on-surface-variant">
        <span className="flex items-center gap-2 text-xs font-medium uppercase tracking-wider">{icon}{label}</span>
        <span className="font-mono text-xs text-secondary">{text}</span>
      </div>
      <div className="h-1.5 w-full overflow-hidden rounded-full bg-surface-highest">
        <div className="h-full bg-gradient-to-r from-secondary to-primary" style={{ width: `${Math.min(100, Math.max(1, pct))}%` }} />
      </div>
    </div>
  );
}
function BigStat({ value, label }: { value: string; label: string }) {
  return <div><div className="tnum text-xl font-bold">{value}</div><div className="text-xs text-on-surface-variant">{label}</div></div>;
}
function BackupStatus({ b }: { b: Backup }) {
  if (b.status === "running") return <span className="flex items-center gap-2 text-secondary"><BackupCloudIcon size={15} active /> In Progress</span>;
  if (b.status === "canceled") return <span className="flex items-center gap-2 text-on-surface-variant"><XCircle size={15} /> Canceled</span>;
  if (b.status === "failed") return <span className="flex items-center gap-2 text-error"><XCircle size={15} /> Failed</span>;
  return <span className="flex items-center gap-2 text-success">{b.verified === "verified" ? <ShieldCheck size={15} /> : <CheckCircle2 size={15} />} {b.verified === "verified" ? "Verified" : "Success"}</span>;
}
function healthText(c: ContainerInfo | null): string {
  if (!c) return "—";
  if (c.health) return c.health.charAt(0).toUpperCase() + c.health.slice(1);
  return c.state === "running" ? "Running" : "Stopped";
}
function baseName(key: string): string { return key ? key.split("/").pop() || key : ""; }

// Per-container "back up at most every" presets (maps to min_interval_hours).

// ContainerPolicyPanel — per-container backup frequency + retention override
// (PLAN §4.2 granular control). Collapsible like "Advanced — backup hooks";
// replaces the old "Schedule in Settings" button. Overrides the global policy for
// THIS container so large apps (Jellyfin, Plex, Bookstack…) can keep fewer/shorter
// copies and back up less often, instead of eating space at the fleet default.
