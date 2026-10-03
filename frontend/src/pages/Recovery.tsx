// Disaster Recovery runbook (Fable-UI-UX C3) — the artifact you need at 2 a.m.
// Auto-composed from real backup metadata (manifests, drills, locations, key
// escrow, app-backup): the correct restore ORDER (databases first, extensions to
// reinstall, then app volumes), where every copy lives, and the last proven
// restore per service. Viewable in-app and exportable to PDF (browser print) or
// Markdown (client-side) — no external service.
import { useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import {
  LifeBuoy, Printer, Download, KeyRound, ShieldCheck, Database, Box,
  HardDrive, AlertTriangle, CheckCircle2, XCircle, Loader2, Server as ServerIcon, Lock, RotateCcw, Share2, Copy, Check, Trash2, ServerCog,
} from "lucide-react";
import { followRun } from "../lib/logStream";
import { api, Node, NodeRestorePlanEntry, Runbook, RunbookNode, RunbookService, StepUpError, fmtAgo, fmtBytes } from "../api";
import { Card, Button, Modal, Select } from "../components/ui";
import StepUpPrompt from "../components/StepUpPrompt";

const fmtDate = (ts: number) => (ts ? new Date(ts * 1000).toLocaleString() : "never");
const fmtDateShort = (ts: number) => (ts ? new Date(ts * 1000).toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" }) : "never");

export default function Recovery() {
  const [rb, setRb] = useState<Runbook | null>(null);
  const [err, setErr] = useState(false);

  useEffect(() => { api.runbook().then(setRb).catch(() => setErr(true)); }, []);

  if (err && !rb) return <div className="py-16 text-center text-error">Couldn't generate the runbook. Try again.</div>;
  if (!rb) return <div className="flex items-center justify-center gap-2 py-16 text-on-surface-variant"><Loader2 size={16} className="animate-spin" /> Composing runbook…</div>;

  const downloadMd = () => {
    const blob = new Blob([toMarkdown(rb)], { type: "text/markdown" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = `dockback-dr-runbook-${new Date().toISOString().slice(0, 10)}.md`;
    a.click();
    URL.revokeObjectURL(a.href);
  };

  const keyRisk = rb.key.ephemeral && !rb.key.acknowledged;

  return <RunbookView rb={rb} keyRisk={keyRisk} downloadMd={downloadMd} onChanged={() => api.runbook().then(setRb).catch(() => {})} />;
}

// RunbookView renders the runbook and drives the one-click whole-node restore
// (F6): a "Restore entire node" action that executes the computed order and
// streams progress into the same live log the stack restore uses.
function RunbookView({ rb, keyRisk, downloadMd, onChanged }: { rb: Runbook; keyRisk: boolean; downloadMd: () => void; onChanged: () => void }) {
  const [activeNode, setActiveNode] = useState<string>("");
  const [rState, setRState] = useState<"idle" | "running" | "done" | "failed">("idle");
  const [rLines, setRLines] = useState<{ level: string; msg: string }[]>([]);
  const [rErr, setRErr] = useState("");
  // F210: a container on this node is marked protected, so recreating every
  // service on it needs fresh proof of the password — the same gate restoring
  // one of those containers on its own has had since F206.
  const [stepUp, setStepUp] = useState<{ node: RunbookNode; totp: boolean; err: string } | null>(null);
  // F211: the pre-flight plan. The two blind confirm() dialogs this replaces
  // asked the operator to agree to recreating every service on a machine without
  // showing them one of those services — and a member the run could not handle
  // was discovered at its turn, after the ones before it were already recreated.
  const [planFor, setPlanFor] = useState<RunbookNode | null>(null);
  const [plan, setPlan] = useState<NodeRestorePlanEntry[] | null>(null);
  const [planErr, setPlanErr] = useState("");
  const [skipBlocked, setSkipBlocked] = useState(false);
  // F212: rebuild onto a DIFFERENT machine. Same grammar the stack dialog uses,
  // because it is the same engine underneath — this button just drives the whole
  // runbook instead of one project.
  const [targetNode, setTargetNode] = useState("");
  const [reconstructHost, setReconstructHost] = useState(true);
  const [hostBaseDir, setHostBaseDir] = useState(() => { try { return localStorage.getItem("dockback.hostBaseDir") || ""; } catch { return ""; } });
  const [remapIP, setRemapIP] = useState(false);
  const [remapFrom, setRemapFrom] = useState("");
  const [remapTo, setRemapTo] = useState("");
  const [remapPath, setRemapPath] = useState(false);
  const [pathTo, setPathTo] = useState("");
  const [privKey, setPrivKey] = useState("");
  const [nodes, setNodes] = useState<Node[]>([]);
  useEffect(() => { api.nodes().then(setNodes).catch(() => setNodes([])); }, []);
  const esRef = useRef<(() => void) | null>(null);
  useEffect(() => () => esRef.current?.(), []);

  // F212: a pasted offline key changes what is blocked, so the plan's blocked
  // rows must stop being blocked in front of the operator rather than only when
  // the request comes back.
  //
  // Declared BEFORE the two filters below, and it has to stay there. They call
  // it synchronously while the component renders, so a declaration further down
  // puts it in its temporal dead zone and the page dies with "Cannot access …
  // before initialization". TypeScript does not catch that — a reference inside
  // a callback is legal to it, because it cannot know the callback runs
  // immediately — and an EMPTY plan never invokes the callback at all, so the
  // page loads fine right up until a plan comes back with services in it.
  const effectiveBlocked = (e: NodeRestorePlanEntry) =>
    e.write_only && privKey.trim() ? "" : (e.blocked || "");

  // F227: which services this rebuild covers. Stored as the EXCLUSION rather than
  // the selection, so a plan that reloads keeps every newly-appearing service in
  // by default — the same direction the stack restore page chose, and the safe
  // one: a service nobody has decided about should be rebuilt, not silently
  // dropped.
  const [dropped, setDropped] = useState<Set<string>>(new Set());
  useEffect(() => { setDropped(new Set()); }, [planFor?.node_id]);

  const isPicked = (e: NodeRestorePlanEntry) => !effectiveBlocked(e) && !dropped.has(e.label);
  // Siblings of a compose project, blocked ones excluded — they are not pickable
  // either way.
  const stackMates = (e: NodeRestorePlanEntry) =>
    !e.stack ? [e] : (plan || []).filter((x) => x.stack === e.stack && !effectiveBlocked(x));

  // Ticking any member of a project ticks the whole project — restoring an app
  // without its database gives you something that starts and does not work, so
  // the DEFAULT is always the whole thing. Unticking drops only what was
  // unticked, which is what makes a blocked or unwanted member removable instead
  // of taking its project out of the run.
  const togglePick = (e: NodeRestorePlanEntry, on: boolean) =>
    setDropped((prev) => {
      const n = new Set(prev);
      if (on) for (const m of stackMates(e)) n.delete(m.label);
      else n.delete(e.label), n.add(e.label);
      return n;
    });

  const picked = (plan || []).filter(isPicked);
  // F146: an atomic member cannot be dropped on its own — the engine refuses such
  // a set, and learning that after confirming something destructive is the wrong
  // place to learn it.
  const atomicBroken = (plan || []).some((e) => e.atomic && e.stack && !effectiveBlocked(e) && dropped.has(e.label)
    && (plan || []).some((x) => x.stack === e.stack && x.atomic && isPicked(x)));
  // A project rebuilt without one of its members is worth saying out loud.
  const partialStacks = Array.from(new Set(
    (plan || []).filter((e) => e.stack && isPicked(e)).map((e) => e.stack as string),
  )).filter((st) => (plan || []).some((x) => x.stack === st && !effectiveBlocked(x) && dropped.has(x.label))).sort();

  // Restore every service on a node in the runbook's proven order (databases
  // first, then dependent apps). Destructive and node-wide, so it double-confirms.
  // F211: open the plan. Nothing is confirmed here — the operator sees what will
  // run, in what order, and what cannot run at all, before agreeing to anything.
  const planBlocked = (plan || []).filter((e) => !!effectiveBlocked(e));
  // F212: is this a rebuild onto another machine, or a restore in place?
  const crossHost = !!planFor && !!targetNode && targetNode !== planFor.node_id;
  const planWriteOnly = (plan || []).filter((e) => e.write_only);

  const openNodePlan = (node: RunbookNode, rebuild = false) => {
    setPlanFor(node); setPlan(null); setPlanErr(""); setSkipBlocked(false); setStepUp(null);
    setTargetNode(rebuild ? "" : node.node_id);
    setRemapIP(rebuild); setRemapPath(false); setRemapFrom(""); setRemapTo(""); setPathTo("");
    setReconstructHost(rebuild); setPrivKey("");
    api.restoreNodeAllPlan(node.node_id)
      .then((r) => setPlan(r.services || []))
      .catch((e) => setPlanErr((e as Error).message));
  };

  const restoreEntireNode = async (node: RunbookNode, creds?: { password: string; code: string }) => {

    esRef.current?.();
    setActiveNode(node.node_id); setRState("running"); setRLines([]); setRErr("");
    const logID = "node:" + node.node_id;
    // F212: a compose project is restored as ONE step, through the stack path,
    // which logs under its own "stack:<project>" id. Following only the node log
    // would leave the panel silent for the whole of each stack — which on a
    // multi-stack machine is most of the run. The plan already names every stack,
    // so those logs are followed too; only the node's own lines decide the
    // terminal state, so a stack's internal wording cannot end the run early.
    const stackIDs = new Set((plan || []).map((e) => e.stack).filter(Boolean).map((sName) => "stack:" + sName));
    const start = Date.now();
    // #N10: the structured verdict for the NODE's own run, with the shared
    // node line rules as the fallback. The stacks' lines are SHOWN (alsoShow)
    // but can never decide the outcome — followRun only judges the run's own.
    esRef.current = followRun(logID, {
      mode: "node",
      since: start - 2000, // skip replayed history
      alsoShow: [...stackIDs],
      onLine: (l) => setRLines((p) => [...p.slice(-120), {
        level: l.level, msg: l.backup_id === logID ? l.msg : "    " + l.msg,
      }]),
      onDone: (outcome, message) => {
        if (outcome === "ok") setRState("done");
        else { setRState("failed"); setRErr(message); }
        onChanged();
      },
    });
    try {
      await api.restoreNodeAll(node.node_id, {
        ...creds,
        ...(skipBlocked ? { skip_blocked: true } : {}),
        // F227: only when something was actually dropped — omitted means "every
        // service", which is exactly what this call has always meant.
        ...(dropped.size > 0 ? { services: picked.map((e) => e.label) } : {}),
        ...(crossHost ? { target_node: targetNode } : {}),
        ...(privKey.trim() ? { private_key: privKey.trim() } : {}),
        ...(reconstructHost ? { reconstruct_host: true, ...(hostBaseDir.trim() ? { host_base_dir: hostBaseDir.trim() } : {}) } : {}),
        ...(remapIP ? { remap_ip: true, ...(remapFrom.trim() ? { remap_from_ip: remapFrom.trim() } : {}), ...(remapTo.trim() ? { remap_to_ip: remapTo.trim() } : {}) } : {}),
        ...(remapPath ? { remap_path: true, ...(pathTo.trim() ? { remap_to_path: pathTo.trim() } : {}) } : {}),
      });
      setStepUp(null); setPlanFor(null);
    } catch (e) {
      esRef.current?.();
      // F210: a protected container wants the password. A prompt, not a failure.
      if (e instanceof StepUpError) {
        setRState("idle"); setRLines([]);
        setStepUp({ node, totp: e.totp_required, err: creds ? e.message : "" });
        return;
      }
      setRState("failed"); setRErr((e as Error).message);
    }
  };

  // F59: signed, expiring share links.
  const [shareOpen, setShareOpen] = useState(false);
  const [shareHours, setShareHours] = useState(24);
  const [mintedUrl, setMintedUrl] = useState("");
  const [shares, setShares] = useState<{ id: string; created: number; exp: number; revoked: boolean; expired: boolean }[]>([]);
  const [shareBusy, setShareBusy] = useState(false);
  const [copied, setCopied] = useState(false);
  const [shareErr, setShareErr] = useState("");
  const loadShares = () => api.runbookShares().then(setShares).catch(() => setShares([]));
  const openShare = () => { setMintedUrl(""); setCopied(false); setShareErr(""); setShareHours(24); setShareOpen(true); loadShares(); };
  const mintShare = async () => {
    setShareBusy(true); setShareErr("");
    try { const r = await api.shareRunbook(shareHours); setMintedUrl(window.location.origin + r.url); setCopied(false); loadShares(); }
    catch (e) { setShareErr((e as Error).message); }
    finally { setShareBusy(false); }
  };
  const revokeShare = async (id: string) => {
    try { await api.revokeRunbookShare(id); loadShares(); } catch (e) { setShareErr((e as Error).message); }
  };

  return (
    <div className="dr-runbook">
      <div className="mb-5 flex flex-wrap items-start justify-between gap-3">
        <div className="flex items-start gap-3">
          <span className="grid h-11 w-11 shrink-0 place-items-center rounded-xl bg-docker-blue/15 text-primary"><LifeBuoy size={22} /></span>
          <div>
            <h1 className="text-[23px] font-bold tracking-tight">Disaster Recovery Runbook</h1>
            <p className="mt-1 text-sm text-on-surface-variant">Generated {fmtDate(rb.generated_at)} · DockBack {rb.app_version} · {rb.summary.services} protected service{rb.summary.services === 1 ? "" : "s"}</p>
          </div>
        </div>
        <div className="no-print flex items-center gap-2">
          <Button variant="secondary" onClick={openShare}><Share2 size={15} /> Share link</Button>
          <Button variant="secondary" onClick={downloadMd}><Download size={15} /> Markdown</Button>
          <Button variant="primary" onClick={() => window.print()}><Printer size={15} /> Print / Save PDF</Button>
        </div>
      </div>

      {/* F59: signed, expiring, revocable share link. */}
      {/* F211: the whole-node restore's pre-flight plan. Replaces two blind
          confirm() dialogs that asked the operator to agree to recreating every
          service on a machine without showing them one of those services. */}
      <Modal
        open={!!planFor}
        onClose={() => { setPlanFor(null); setStepUp(null); }}
        title={planFor ? (crossHost ? `Rebuild “${planFor.node_name}” onto another node` : `Restore entire node “${planFor.node_name}”`) : "Restore entire node"}
        footer={<>
          <Button variant="secondary" onClick={() => { setPlanFor(null); setStepUp(null); }}>Cancel</Button>
          <Button
            variant="danger"
            disabled={!plan || plan.length === 0 || (planBlocked.length > 0 && !skipBlocked) || picked.length === 0 || atomicBroken || rState === "running"}
            onClick={() => planFor && restoreEntireNode(planFor)}
          >
            {/* F227: the count is what will ACTUALLY run — the selection, not the
                plan. A button that says twelve while rebuilding four is the kind
                of thing you only notice afterwards. */}
            <RotateCcw size={15} /> {crossHost ? "Rebuild" : "Restore"} {picked.length} service{picked.length === 1 ? "" : "s"}
          </Button>
        </>}
      >
        <div className="flex flex-col gap-3 text-sm">
          <p className="text-on-surface-variant">
            Recreates the services you tick below from their latest backup, <b className="text-on-surface">in this order</b> — databases first, then what depends on them. A compose project is restored as one step, so its own dependency order and its compose file come with it.{crossHost ? " Nothing on the original node is touched." : " Each service’s current state is snapshotted first, so a bad restore can roll back."} <b className="text-on-surface">This is destructive.</b>
          </p>

          {/* F212: where this lands. Defaults to the node itself — "Rebuild onto
              another node…" opens the same dialog pre-armed for a move. */}
          <div>
            <div className="mb-1 text-xs font-medium text-on-surface-variant">Restore to</div>
            <Select value={targetNode} onChange={(e) => setTargetNode(e.target.value)}>
              {planFor && <option value={planFor.node_id}>{planFor.node_name} — this node (original)</option>}
              {nodes.filter((n) => n.reachable && n.id !== planFor?.node_id).map((n) => (
                <option key={n.id} value={n.id}>{n.name}</option>
              ))}
            </Select>
            {crossHost && (
              <p className="mt-1.5 text-xs text-warning">
                Every service is recreated on <b>{nodes.find((n) => n.id === targetNode)?.name || targetNode}</b> — images re-pulled by digest (or loaded from a bundled tarball) and volumes recreated there. The original node is not touched.
              </p>
            )}
          </div>

          {crossHost && (
            <div className="flex flex-col gap-2">
              <label className="flex cursor-pointer items-start gap-2 rounded border border-outline-variant/50 bg-surface-lowest px-3 py-2 text-xs text-on-surface-variant">
                <input type="checkbox" className="mt-0.5" checked={reconstructHost} onChange={(e) => setReconstructHost(e.target.checked)} />
                <span className="min-w-0"><span className="font-medium text-on-surface">Reconstruct stack folders on host</span> — rebuild each project&rsquo;s on-host directory and write its compose file back, so the new machine gets the organized layout, not just containers.</span>
              </label>
              {reconstructHost && (
                <div className="flex flex-wrap items-center gap-2 rounded bg-secondary/10 px-3 py-2 text-xs text-secondary">
                  <span className="whitespace-nowrap">Base folder for non-compose services:</span>
                  <input value={hostBaseDir} onChange={(e) => { setHostBaseDir(e.target.value); try { localStorage.setItem("dockback.hostBaseDir", e.target.value); } catch { /* ignore */ } }}
                    placeholder="/opt/docker" spellCheck={false}
                    className="min-w-[160px] flex-1 rounded border border-outline-variant/60 bg-surface px-2 py-1 font-mono text-on-surface placeholder:text-on-surface-variant/50" />
                </div>
              )}
              <label className="flex cursor-pointer items-start gap-2 rounded border border-outline-variant/50 bg-surface-lowest px-3 py-2 text-xs text-on-surface-variant">
                <input type="checkbox" className="mt-0.5" checked={remapIP} onChange={(e) => setRemapIP(e.target.checked)} />
                <span className="min-w-0"><span className="font-medium text-on-surface">Remap machine IP</span> — rewrite the old machine&rsquo;s IP to the new one in each recreated config. Blank fields are filled from the two nodes&rsquo; addresses; if neither yields one, the rebuild refuses and asks here.</span>
              </label>
              {remapIP && (
                <div className="grid grid-cols-[auto_1fr] items-center gap-x-2 gap-y-1.5 rounded bg-secondary/10 px-3 py-2 text-xs text-secondary">
                  <span className="whitespace-nowrap">From (old host):</span>
                  <input value={remapFrom} onChange={(e) => setRemapFrom(e.target.value)} placeholder="auto — the old node&rsquo;s address" spellCheck={false}
                    className="w-full rounded border border-outline-variant/60 bg-surface px-2 py-1 font-mono text-on-surface placeholder:text-on-surface-variant/50" />
                  <span className="whitespace-nowrap">To (new host):</span>
                  <input value={remapTo} onChange={(e) => setRemapTo(e.target.value)} placeholder="auto — the new node&rsquo;s address" spellCheck={false}
                    className="w-full rounded border border-outline-variant/60 bg-surface px-2 py-1 font-mono text-on-surface placeholder:text-on-surface-variant/50" />
                </div>
              )}
              <label className="flex cursor-pointer items-start gap-2 rounded border border-outline-variant/50 bg-surface-lowest px-3 py-2 text-xs text-on-surface-variant">
                <input type="checkbox" className="mt-0.5" checked={remapPath} onChange={(e) => setRemapPath(e.target.checked)} />
                <span className="min-w-0"><span className="font-medium text-on-surface">Remap stack paths</span> — move bind-mount folders to the new machine&rsquo;s layout. Each project&rsquo;s old base is read from its own recorded layout; one that records none keeps its paths.</span>
              </label>
              {remapPath && (
                <div className="flex flex-wrap items-center gap-2 rounded bg-secondary/10 px-3 py-2 text-xs text-secondary">
                  <span className="whitespace-nowrap">To base folder:</span>
                  <input value={pathTo} onChange={(e) => setPathTo(e.target.value)} placeholder="/opt/stacks" spellCheck={false}
                    className="min-w-[160px] flex-1 rounded border border-outline-variant/60 bg-surface px-2 py-1 font-mono text-on-surface placeholder:text-on-surface-variant/50" />
                </div>
              )}
            </div>
          )}

          {/* F212: an offline key unblocks the write-only members, which a
              whole-node run otherwise has nowhere to ask for. */}
          {planWriteOnly.length > 0 && (
            <div className="rounded border border-primary/30 bg-primary/[0.06] p-3">
              <div className="mb-1 flex flex-wrap items-center gap-2 text-xs font-semibold text-primary">
                <Lock size={13} className="shrink-0" /> Offline private key
              </div>
              <p className="mb-2 text-xs text-on-surface-variant">
                {planWriteOnly.length === 1 ? "One service on this node is" : `${planWriteOnly.length} services on this node are`} <b className="text-on-surface">write-only encrypted</b> ({planWriteOnly.map((e) => e.label).join(", ")}). Paste the private key from their recovery sheet to include them; leave it blank to skip them.
              </p>
              <textarea value={privKey} onChange={(e) => setPrivKey(e.target.value)} rows={2} spellCheck={false} autoComplete="off"
                placeholder="base64 private key" aria-label="Offline private key"
                className="w-full break-all rounded border border-outline-variant bg-surface-lowest px-3 py-2 font-mono text-xs text-on-surface focus:outline-none focus:ring-1 focus:ring-primary" />
            </div>
          )}

          {planErr && <div className="flex items-start gap-2 rounded bg-error/10 px-3 py-2 text-xs text-error"><AlertTriangle size={14} className="mt-0.5 shrink-0" /> <span className="min-w-0 break-words">{planErr}</span></div>}
          {!plan && !planErr && <div className="flex items-center gap-2 py-4 text-on-surface-variant"><Loader2 size={15} className="animate-spin" /> Working out the restore order…</div>}

          {plan && plan.length === 0 && (
            <div className="py-4 text-on-surface-variant">No restorable backups on this node — there is nothing to restore.</div>
          )}

          {plan && plan.length > 0 && (
            <div className="mb-2 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs">
              <span className="font-medium uppercase tracking-wider text-on-surface-variant">What to rebuild</span>
              <span className="text-on-surface-variant">{picked.length} of {plan.filter((e) => !effectiveBlocked(e)).length} services</span>
              <span className="ml-auto flex shrink-0 gap-2">
                <button onClick={() => setDropped(new Set())} className="text-primary hover:underline">Select all</button>
                <span className="text-outline">&middot;</span>
                <button onClick={() => setDropped(new Set(plan.filter((e) => !effectiveBlocked(e)).map((e) => e.label)))} className="text-primary hover:underline">Select none</button>
              </span>
            </div>
          )}

          {plan && plan.length > 0 && (
            <ol className="flex flex-col gap-1.5 rounded border border-outline-variant bg-surface-lowest p-3">
              {plan.map((e, i) => (
                <li key={e.backup_id} className={`flex flex-wrap items-center gap-x-2 gap-y-1 text-xs ${isPicked(e) ? "" : "opacity-60"}`}>
                  {/* F227: pick what this rebuild covers. Ticking any member of a
                      compose project ticks the whole project — an app restored
                      without its database starts and does not work — but a single
                      member can still be dropped, which is how a write-only or
                      unwanted service is left out without taking its project with
                      it. An atomic member (F146) is the exception: the engine
                      refuses a partial set, so the box is disabled rather than
                      offering a choice that is refused after the confirm. */}
                  <input
                    type="checkbox"
                    className="shrink-0"
                    checked={isPicked(e)}
                    disabled={!!effectiveBlocked(e) || (!!e.atomic && isPicked(e) && stackMates(e).filter((m) => m.atomic && isPicked(m)).length > 1)}
                    aria-label={`Rebuild ${e.label}`}
                    title={effectiveBlocked(e)
                      ? "This service cannot be rebuilt by a whole-node run"
                      : e.atomic
                        ? "This application's services are only meaningful together — it is rebuilt as a whole or not at all"
                        : e.stack
                          ? `Rebuild ${e.label} — ticking it brings the rest of ${e.stack} with it`
                          : `Rebuild ${e.label}`}
                    onChange={(ev) => togglePick(e, ev.target.checked)}
                  />
                  <span className="grid h-5 w-5 shrink-0 place-items-center rounded-full bg-surface-highest text-[10px] font-bold tnum">{e.order}</span>
                  {e.role === "database"
                    ? <Database size={13} className="shrink-0 text-secondary" />
                    : <Box size={13} className="shrink-0 text-on-surface-variant" />}
                  <span className={`min-w-0 break-words font-semibold ${effectiveBlocked(e) ? "text-on-surface-variant line-through" : "text-on-surface"}`}>{e.label}</span>
                  <span className="text-on-surface-variant">from {fmtAgo(e.created_at)}</span>
                  {e.verified === "verified"
                    ? <span className="flex shrink-0 items-center gap-1 text-success"><ShieldCheck size={11} /> verified</span>
                    : <span className="shrink-0 text-warning">{e.verified || "unverified"}</span>}
                  {e.write_only && <span className="flex shrink-0 items-center gap-1 text-primary"><Lock size={11} /> write-only</span>}
                  {effectiveBlocked(e) && (
                    <span className="basis-full break-words pl-7 text-[11px] text-error">
                      &bull; will be skipped &mdash; {effectiveBlocked(e)}
                    </span>
                  )}
                  {!effectiveBlocked(e) && e.stack && !isPicked(e) && (
                    <span className="basis-full break-words pl-7 text-[11px] text-warning">
                      &bull; left out &mdash; <b>{e.stack}</b> is rebuilt without it. Tick it again to bring the whole project back.
                    </span>
                  )}
                </li>
              ))}
            </ol>
          )}

          {/* F227: a project rebuilt without one of its own members. Not refused —
              the operator may be leaving out exactly the service they cannot
              restore here — but never silent either. */}
          {partialStacks.length > 0 && (
            <div className="mt-2 flex items-start gap-2 rounded border border-warning/40 bg-warning/10 px-3 py-2 text-xs text-warning">
              <AlertTriangle size={13} className="mt-0.5 shrink-0" />
              <span className="min-w-0 break-words">
                {partialStacks.length === 1 ? <>Project <b>{partialStacks[0]}</b> is</> : <>{partialStacks.length} projects ({partialStacks.join(", ")}) are</>} rebuilt
                without {partialStacks.length === 1 ? "one of its own services" : "some of their own services"}. That is allowed &mdash; it is how you leave out a service this
                run cannot restore &mdash; but the project comes back incomplete.
              </span>
            </div>
          )}
          {atomicBroken && (
            <div className="mt-2 flex items-start gap-2 rounded border border-error/40 bg-error/10 px-3 py-2 text-xs text-error">
              <AlertTriangle size={13} className="mt-0.5 shrink-0" />
              <span className="min-w-0 break-words">
                One of these applications is only meaningful with all of its services, so it is rebuilt as a whole or not at all. Tick its
                remaining service back on, or untick the whole project.
              </span>
            </div>
          )}

          {planBlocked.length > 0 && (
            <label className="flex cursor-pointer items-start gap-2 rounded border border-error/30 bg-error/10 px-3 py-2 text-xs text-error">
              <input type="checkbox" className="mt-0.5" checked={skipBlocked} onChange={(e) => setSkipBlocked(e.target.checked)} />
              <span className="min-w-0">
                <b>Skip these and restore the rest.</b> {planBlocked.length === 1 ? "One service" : `${planBlocked.length} services`} cannot be restored by a whole-node run: <span className="break-words">{planBlocked.map((e) => e.label).join(", ")}</span>. Restore {planBlocked.length === 1 ? "it" : "them"} individually afterwards &mdash; a write-only backup needs its offline key pasted, which this run has nowhere to ask for.
              </span>
            </label>
          )}

          {stepUp && planFor && (
            <StepUpPrompt totp={stepUp.totp} busy={rState === "running"} error={stepUp.err}
              confirmLabel="Confirm and restore node"
              onConfirm={(pw, code) => restoreEntireNode(planFor, { password: pw, code })} />
          )}
        </div>
      </Modal>

      <Modal open={shareOpen} onClose={() => setShareOpen(false)} title="Share this runbook"
        footer={<Button variant="secondary" onClick={() => setShareOpen(false)}>Close</Button>}>
        <div className="flex flex-col gap-3 text-sm">
          <p className="text-on-surface-variant">Anyone with this link can read a redacted, read-only copy of this runbook until it expires. No login needed. The encryption key, node addresses, and storage details are omitted.</p>

          {mintedUrl ? (
            <div className="flex flex-col gap-2">
              <div className="text-xs font-medium text-on-surface-variant">Your link — copy it now:</div>
              <div className="flex items-center gap-2 rounded border border-outline-variant bg-surface-lowest px-3 py-2">
                <code className="min-w-0 flex-1 break-all font-mono text-xs text-on-surface">{mintedUrl}</code>
                <Button variant="secondary" className="shrink-0 h-8 px-2 py-0 text-xs" onClick={() => { navigator.clipboard?.writeText(mintedUrl).then(() => setCopied(true)).catch(() => {}); }}>
                  {copied ? <Check size={14} /> : <Copy size={14} />} {copied ? "Copied" : "Copy link"}
                </Button>
              </div>
              <Button variant="ghost" className="self-start" onClick={() => setMintedUrl("")}>Create another</Button>
            </div>
          ) : (
            <div className="flex flex-wrap items-end gap-2">
              <div>
                <div className="mb-1 text-xs font-medium text-on-surface-variant">Valid for</div>
                <Select value={String(shareHours)} onChange={(e) => setShareHours(parseInt(e.target.value, 10))}>
                  <option value="1">1 hour</option>
                  <option value="24">24 hours</option>
                  <option value="168">7 days</option>
                </Select>
              </div>
              <Button variant="primary" onClick={mintShare} disabled={shareBusy}>
                {shareBusy ? <Loader2 size={15} className="animate-spin" /> : <Share2 size={15} />} Create link
              </Button>
            </div>
          )}

          {shareErr && <div className="rounded bg-error/10 px-3 py-2 text-xs text-error">{shareErr}</div>}

          {shares.length > 0 && (
            <div className="mt-1">
              <div className="mb-1.5 text-xs font-medium text-on-surface-variant">Active share links</div>
              <ul className="flex flex-col divide-y divide-outline-variant/50 rounded border border-outline-variant/60">
                {shares.map((sh) => (
                  <li key={sh.id} className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2 text-xs">
                    <span className="font-mono text-on-surface-variant">…{sh.id.slice(-6)}</span>
                    <span className="text-on-surface-variant">created {fmtAgo(sh.created)}</span>
                    <span className={sh.revoked ? "text-error" : sh.expired ? "text-on-surface-variant" : "text-success"}>
                      {sh.revoked ? "revoked" : sh.expired ? "expired" : `expires ${new Date(sh.exp * 1000).toLocaleString()}`}
                    </span>
                    {!sh.revoked && !sh.expired && (
                      <button onClick={() => revokeShare(sh.id)} className="ml-auto flex items-center gap-1 rounded border border-outline-variant px-1.5 py-0.5 text-on-surface-variant hover:text-error"><Trash2 size={12} /> Revoke</button>
                    )}
                  </li>
                ))}
              </ul>
            </div>
          )}
        </div>
      </Modal>

      {/* 1 — Before you begin: key + control plane */}
      <Card className="mb-4 p-5">
        <SectionTitle n={1}>Before you begin</SectionTitle>

        <div className="grid gap-3 [grid-template-columns:repeat(auto-fit,minmax(280px,1fr))]">
          <div className={`rounded-lg border p-3.5 ${keyRisk ? "border-error/40 bg-error/[0.06]" : "border-outline-variant bg-surface-lowest"}`}>
            <div className="mb-1.5 flex items-center gap-2 text-sm font-semibold"><KeyRound size={16} className={keyRisk ? "text-error" : "text-primary"} /> Master encryption key</div>
            <p className="text-xs text-on-surface-variant">Every backup is encrypted with this key. Without it, no archive can be restored — recover it first.</p>
            <div className="mt-2 space-y-1 text-xs">
              <Row label="Fingerprint"><code className="font-mono text-on-surface">{rb.key.fingerprint || "—"}</code></Row>
              <Row label="Escrow confirmed">{rb.key.acknowledged ? <span className="text-success">Yes</span> : <span className="text-warning">No — back it up</span>}</Row>
            </div>
            {keyRisk && (
              <div className="mt-2 flex items-start gap-2 rounded border border-error/30 bg-error/10 px-2.5 py-2 text-xs text-error">
                <AlertTriangle size={14} className="mt-0.5 shrink-0" />
                <span>This key is <b>in-memory only</b> and not confirmed backed up. If DockBack restarts you will <b>lose access to every backup</b>. Reveal and store it now (Settings → Security).</span>
              </div>
            )}
          </div>

          <div className="rounded-lg border border-outline-variant bg-surface-lowest p-3.5">
            <div className="mb-1.5 flex items-center gap-2 text-sm font-semibold"><ServerIcon size={16} className="text-primary" /> Control plane (DockBack itself)</div>
            <p className="text-xs text-on-surface-variant">If the machine running DockBack is lost, restore this app-backup first — it rebuilds nodes, catalog, and settings.</p>
            <div className="mt-2 space-y-1 text-xs">
              {rb.app_backup.exists
                ? <><Row label="Newest app-backup"><span className="text-on-surface">{fmtDate(rb.app_backup.newest_at)}</span></Row>
                    <Row label="Copies on file"><span className="text-on-surface">{rb.app_backup.count}</span></Row></>
                : <div className="flex items-start gap-2 rounded border border-warning/30 bg-warning/10 px-2.5 py-2 text-warning"><AlertTriangle size={14} className="mt-0.5 shrink-0" /><span>No app-backup yet. Create one (Settings → App Backup) so DockBack itself can be rebuilt.</span></div>}
            </div>
          </div>
        </div>
      </Card>

      {/* 2 — Destinations */}
      {rb.destinations.length > 0 && (
        <Card className="mb-4 p-5">
          <SectionTitle n={2}>Where your copies live</SectionTitle>
          <div className="flex flex-wrap gap-2">
            {rb.destinations.map((d) => (
              <span key={d.name} className="flex items-center gap-2 rounded-lg border border-outline-variant bg-surface-lowest px-3 py-1.5 text-sm">
                <HardDrive size={14} className="text-on-surface-variant" /> {d.name}
                <span className="font-mono text-[11px] uppercase tracking-wider text-on-surface-variant">{d.type}</span>
                {!d.enabled && <span className="text-[11px] text-warning">disabled</span>}
              </span>
            ))}
          </div>
        </Card>
      )}

      {/* 3 — Restore order per node */}
      <Card className="p-5">
        <SectionTitle n={3}>Restore order</SectionTitle>
        <p className="mb-4 -mt-1 text-xs text-on-surface-variant">Restore in this order on each node: <b>databases first</b> (with their extensions), then the applications that depend on them.</p>

        {rb.nodes.every((n) => n.services.length === 0) ? (
          <div className="flex items-center gap-2 py-6 text-sm text-on-surface-variant"><AlertTriangle size={15} className="opacity-70" /> No successful backups to build a restore plan from yet.</div>
        ) : rb.nodes.map((node) => node.services.length > 0 && (
          <div key={node.node_id} className="mb-5 last:mb-0">
            <div className="mb-2.5 flex flex-wrap items-center gap-2 border-b border-outline-variant/60 pb-1.5">
              <ServerIcon size={15} className="text-on-surface-variant" />
              <span className="font-semibold">{node.node_name}</span>
              {!node.reachable && <span className="flex items-center gap-1 text-xs text-error"><XCircle size={11} /> unreachable now</span>}
              <div className="no-print ml-auto">
                <Button variant="secondary" onClick={() => openNodePlan(node, true)} disabled={rState === "running"} title="Recreate every service from this node onto a DIFFERENT machine, in the proven order — for when this host is gone">
                  <ServerCog size={14} /> Rebuild onto another node…
                </Button>
                <Button variant="danger" onClick={() => openNodePlan(node)} disabled={rState === "running"} title="Recreate and restore every service on this node in the proven order (databases first)">
                  {rState === "running" && activeNode === node.node_id ? <Loader2 size={14} className="animate-spin" /> : <RotateCcw size={14} />} Restore entire node
                </Button>
              </div>
            </div>
            <ol className="space-y-2.5">
              {node.services.map((svc) => <ServiceStep key={svc.container} svc={svc} />)}
            </ol>
            {activeNode === node.node_id && rState !== "idle" && (
              <div className="no-print mt-3">
                {rState === "running" && <div className="flex items-center gap-2 rounded bg-secondary/10 px-3 py-2 text-sm text-secondary"><Loader2 size={14} className="animate-spin" /> Restoring {node.node_name} in the proven order — databases first…</div>}
                {rState === "done" && <div className="flex items-center gap-2 rounded bg-success/10 px-3 py-2 text-sm text-success"><CheckCircle2 size={14} /> {node.node_name} restored — every service is back up in the proven order.</div>}
                {rState === "failed" && <div className="flex items-start gap-2 rounded bg-error/10 px-3 py-2 text-sm text-error"><AlertTriangle size={14} className="mt-0.5 shrink-0" /> Node restore failed{rErr ? `: ${rErr.replace(/^.*?failed:\s*/i, "")}` : ""}. Services already restored are up; the rest were not.</div>}
                {rLines.length > 0 && (
                  <pre className="mt-2 max-h-64 overflow-auto rounded bg-surface-lowest p-3 text-[11px] leading-relaxed text-on-surface-variant">
                    {rLines.map((l, i) => <div key={i} className={l.level === "ERR" ? "text-error" : l.level === "WARN" ? "text-warning" : ""}>{l.msg}</div>)}
                  </pre>
                )}
              </div>
            )}
          </div>
        ))}
      </Card>

      <p className="mt-4 text-center text-xs text-on-surface-variant">
        This runbook is generated from your live backup metadata. Also see <Link to="/docs/restore/disaster" className="text-primary hover:underline">Docs → Disaster recovery</Link>.
      </p>
    </div>
  );
}

function SectionTitle({ n, children }: { n: number; children: React.ReactNode }) {
  return (
    <h2 className="mb-3 flex items-center gap-2.5 text-lg font-bold">
      <span className="grid h-7 w-7 place-items-center rounded-full bg-docker-blue/15 text-sm text-primary">{n}</span>
      {children}
    </h2>
  );
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return <div className="flex items-center justify-between gap-3"><span className="text-on-surface-variant">{label}</span><span>{children}</span></div>;
}

function ServiceStep({ svc }: { svc: RunbookService }) {
  const isDB = svc.role === "database";
  const drill = svc.last_drill_ok;
  return (
    <li className="rounded-lg border border-outline-variant bg-surface-lowest p-3.5">
      <div className="flex flex-wrap items-center gap-2.5">
        <span className="grid h-6 w-6 shrink-0 place-items-center rounded-full bg-surface-highest text-xs font-bold tnum">{svc.order}</span>
        {isDB ? <Database size={16} className="shrink-0 text-secondary" /> : <Box size={16} className="shrink-0 text-on-surface-variant" />}
        <span className="text-sm font-semibold">{svc.container}</span>
        <span className={`rounded px-1.5 py-0.5 text-[10px] font-bold uppercase tracking-wide ${isDB ? "bg-secondary/15 text-secondary" : "bg-surface-highest text-on-surface-variant"}`}>{isDB ? (svc.engine || "database") : "app"}</span>
        {svc.stack && <span className="text-xs text-on-surface-variant">stack: {svc.stack}</span>}
        <span className="ml-auto flex items-center gap-2">
          {svc.partial && <span className="flex items-center gap-1 text-[11px] font-semibold text-warning"><AlertTriangle size={12} /> partial</span>}
          {!svc.image_bundled && !svc.image_digest && svc.image && (
            <span className="flex items-center gap-1 text-[11px] font-semibold text-warning"
              title="This backup's image is only referenced by a tag and isn't bundled — a restore depends on that tag still existing in its registry. Re-back up with the image bundled (image.tar) for an exact, offline-proof restore.">
              <AlertTriangle size={12} /> image not bundled
            </span>
          )}
          {drill === undefined
            ? <span className="text-[11px] text-on-surface-variant">not drilled</span>
            : drill
              ? <span className="flex items-center gap-1 text-[11px] text-success"><CheckCircle2 size={12} /> drill passed</span>
              : <span className="flex items-center gap-1 text-[11px] text-error"><XCircle size={12} /> drill failed</span>}
          {svc.standby_node && (
            svc.standby_ok === undefined
              ? <span className="flex items-center gap-1 text-[11px] text-on-surface-variant" title={`Standby onto ${svc.standby_node} is configured but hasn't rehearsed yet.`}><ServerCog size={12} /> standby: not proven</span>
              : svc.standby_ok
                ? <span className="flex items-center gap-1 text-[11px] text-success" title={`Proven to boot on ${svc.standby_node}${svc.standby_at ? " · " + fmtDateShort(svc.standby_at) : ""}`}><ServerCog size={12} /> standby proven</span>
                : <span className="flex items-center gap-1 text-[11px] text-error" title={`Standby rehearsal onto ${svc.standby_node} is failing.`}><ServerCog size={12} /> standby failing</span>
          )}
        </span>
      </div>

      <div className="mt-2 grid gap-x-6 gap-y-1 text-xs [grid-template-columns:repeat(auto-fit,minmax(220px,1fr))]">
        {svc.image && <Row label="Image"><code className="truncate font-mono text-[11px] text-on-surface">{svc.image}{svc.image_digest ? `@${svc.image_digest.slice(0, 19)}…` : ""}</code></Row>}
        <Row label="Contents"><span className="text-on-surface-variant">{svc.databases > 0 ? `${svc.databases} DB dump${svc.databases === 1 ? "" : "s"}` : ""}{svc.databases > 0 && svc.volumes > 0 ? " · " : ""}{svc.volumes > 0 ? `${svc.volumes} volume${svc.volumes === 1 ? "" : "s"}` : svc.databases === 0 ? "config only" : ""}</span></Row>
        <Row label="Last backup"><span className="text-on-surface-variant">{fmtDateShort(svc.last_backup_at)}</span></Row>
        <Row label="Last proven restore"><span className="text-on-surface-variant">{svc.last_drill_at ? fmtDateShort(svc.last_drill_at) : "never"}</span></Row>
        {svc.standby_node && (
          <Row label="Standby"><span className="text-on-surface-variant">{
            svc.standby_ok === undefined ? `${svc.standby_node} · not configured proven yet`
              : svc.standby_ok ? `proven on ${svc.standby_node}${svc.standby_at ? " · " + fmtDateShort(svc.standby_at) : ""}`
                : `failing on ${svc.standby_node}`
          }</span></Row>
        )}
      </div>

      {/* Copy locations */}
      <div className="mt-2 flex flex-wrap items-center gap-1.5">
        {svc.locations.map((l, i) => (
          <span key={i} className={`flex items-center gap-1 rounded border px-2 py-0.5 text-[11px] ${l.status === "failed" ? "border-error/40 bg-error/10 text-error" : "border-outline-variant bg-surface-container text-on-surface-variant"}`}>
            <HardDrive size={11} /> {l.name}
            {l.immutable && <Lock size={10} className="text-success" />}
            {l.status === "failed" && <span title={l.detail}>(failed)</span>}
          </span>
        ))}
      </div>

      {/* Extensions to reinstall (the Immich gotcha) */}
      {svc.extensions && svc.extensions.length > 0 && (
        <div className="mt-2 flex items-start gap-2 rounded border border-secondary/30 bg-secondary/10 px-2.5 py-1.5 text-xs">
          <Database size={13} className="mt-0.5 shrink-0 text-secondary" />
          <span className="text-on-surface-variant">Reinstall extensions before importing: <b className="text-on-surface">{svc.extensions.join(", ")}</b></span>
        </div>
      )}

      {/* Skipped mounts — covered ones (F83 shared binds) are informational, not warnings. */}
      {svc.skipped_mounts && svc.skipped_mounts.some((m) => !m.covered_by) && (
        <div className="mt-2 rounded border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs">
          <div className="flex items-center gap-1.5 font-semibold text-warning"><AlertTriangle size={13} /> NOT captured — recover separately</div>
          <ul className="mt-1 list-disc pl-5 text-on-surface-variant">
            {svc.skipped_mounts.filter((m) => !m.covered_by).map((m, i) => <li key={i}>{m.destination} <span className="text-on-surface-variant/70">({m.reason}{m.bytes ? `, ${fmtBytes(m.bytes)}` : ""})</span></li>)}
          </ul>
        </div>
      )}
      {svc.skipped_mounts && svc.skipped_mounts.some((m) => !!m.covered_by) && (
        <div className="mt-2 rounded border border-outline-variant bg-surface-lowest px-2.5 py-1.5 text-xs text-on-surface-variant">
          <div className="font-semibold text-on-surface">Shared folder — captured via another container</div>
          <ul className="mt-1 list-disc pl-5">
            {svc.skipped_mounts.filter((m) => !!m.covered_by).map((m, i) => <li key={i}>{m.destination} — captured via <span className="font-medium text-on-surface">{m.covered_by}</span>{m.bytes ? ` (${fmtBytes(m.bytes)})` : ""}</li>)}
          </ul>
        </div>
      )}

      {/* Notes */}
      {svc.notes && svc.notes.length > 0 && (
        <ul className="mt-2 space-y-1 text-xs text-on-surface-variant">
          {svc.notes.filter((_, i) => !(svc.extensions && svc.extensions.length && i === 0)).map((note, i) => (
            <li key={i} className="flex items-start gap-1.5"><span className="mt-1 h-1 w-1 shrink-0 rounded-full bg-on-surface-variant" /> {note}</li>
          ))}
        </ul>
      )}
    </li>
  );
}

// toMarkdown renders the runbook as a portable .md (no server round-trip).
function toMarkdown(rb: Runbook): string {
  const L: string[] = [];
  L.push(`# DockBack Disaster Recovery Runbook`, "");
  L.push(`_Generated ${fmtDate(rb.generated_at)} · DockBack ${rb.app_version}_`, "");

  L.push(`## 1. Before you begin`, "");
  L.push(`### Master encryption key`);
  L.push(`- Fingerprint: \`${rb.key.fingerprint || "—"}\``);
  L.push(`- Escrow confirmed: ${rb.key.acknowledged ? "yes" : "NO — back it up"}`);
  if (rb.key.ephemeral && !rb.key.acknowledged) L.push(`- **WARNING:** key is in-memory only and not confirmed backed up — a restart loses access to every backup. Reveal and store it now.`);
  L.push("");
  L.push(`### Control plane (DockBack itself)`);
  L.push(rb.app_backup.exists ? `- Newest app-backup: ${fmtDate(rb.app_backup.newest_at)} (${rb.app_backup.count} on file)` : `- **No app-backup yet** — create one so DockBack can be rebuilt.`);
  L.push("");

  if (rb.destinations.length) {
    L.push(`## 2. Where your copies live`, "");
    for (const d of rb.destinations) L.push(`- ${d.name} (${d.type})${d.enabled ? "" : " — disabled"}`);
    L.push("");
  }

  L.push(`## 3. Restore order`, "", `Restore in this order on each node: **databases first** (with their extensions), then dependent applications.`, "");
  for (const node of rb.nodes) {
    if (!node.services.length) continue;
    L.push(`### ${node.node_name}`, "");
    for (const s of node.services) {
      L.push(`${s.order}. **${s.container}** — ${s.role === "database" ? (s.engine || "database") : "app"}${s.stack ? ` (stack: ${s.stack})` : ""}`);
      if (s.image) L.push(`   - Image: \`${s.image}${s.image_digest ? "@" + s.image_digest : ""}\``);
      L.push(`   - Copies: ${s.locations.map((l) => l.name + (l.status === "failed" ? " (FAILED)" : "")).join(", ") || "local"}`);
      L.push(`   - Last backup: ${fmtDateShort(s.last_backup_at)}; last proven restore: ${s.last_drill_at ? fmtDateShort(s.last_drill_at) : "never"}`);
      if (s.standby_node) L.push(`   - Standby node: ${s.standby_node} — ${s.standby_ok === undefined ? "not yet rehearsed" : s.standby_ok ? `proven ${s.standby_at ? fmtDateShort(s.standby_at) : ""}` : "FAILING"}`);
      if (s.extensions?.length) L.push(`   - Reinstall extensions before import: ${s.extensions.join(", ")}`);
      {
        // F83: covered shared binds are a pointer to their owner, not a warning.
        const uncov = (s.skipped_mounts ?? []).filter((m) => !m.covered_by);
        const cov = (s.skipped_mounts ?? []).filter((m) => !!m.covered_by);
        if (uncov.length) L.push(`   - NOT captured (recover separately): ${uncov.map((m) => `${m.destination} (${m.reason})`).join("; ")}`);
        if (cov.length) L.push(`   - Shared folders captured via another container: ${cov.map((m) => `${m.destination} (via ${m.covered_by})`).join("; ")}`);
      }
      for (const note of s.notes ?? []) L.push(`   - ${note}`);
      L.push("");
    }
  }
  return L.join("\n");
}
