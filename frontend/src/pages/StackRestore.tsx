// Stack restore, as a PAGE (F224). Was a 723-line dialog: target node, point in
// time, source copy, revert, snapshot, host reconstruction, three remaps, the
// offline key, the step-up prompt AND the per-service plan, inside a box that
// scrolled. The plan is the part you must read before agreeing to something
// destructive, and it was the part pushed furthest down.
//
// Here the plan is the left column and the options sit beside it, with the
// confirm in a bar that states what will happen in the same sentence as the
// button. Every control, gate and warning is the dialog's, moved — this is a
// layout change, not a behaviour one.
//
// Both entry points (a stack card on a node, the Backups drawer's "Restore
// stack") navigate here, so the two can still never offer different capability
// levels.
import { useEffect, useRef, useState } from "react";
import { ipFromNodeAddr } from "../lib/nodeAddress";
import { Link, useNavigate, useParams } from "react-router-dom";
import { ChevronRight, RotateCcw, Loader2, Database, Box, AlertTriangle, Info, Layers, ShieldCheck, Lock, HardDrive } from "lucide-react";
import { baseDirInvalid, baseDirProblem } from "../lib/absoluteBase";
import { api, BindSourcePlan, CrossRestoreDefaults, Node, RestoreCapacity, StackRestorePlan, StepUpError, fmtAgo, fmtBytes } from "../api";
import { Button, Card, Chip, Select } from "../components/ui";
import StepUpPrompt from "../components/StepUpPrompt";
import { readStackRestoreSeed, StackRestoreSeed } from "../lib/stackRestoreSeed";
import { capacityTone } from "../lib/capacityTone";


export default function StackRestore() {
  const { id: sourceNodeID = "", project = "" } = useParams();
  const navigate = useNavigate();
  const [nodes, setNodes] = useState<Node[]>([]);
  // The caller's already-made choices, handed over out-of-band (see
  // lib/stackRestoreSeed). Read ONCE on mount and consumed, so a reload of this
  // URL is a clean start rather than a replay of somebody's old selections.
  const [initial] = useState<StackRestoreSeed | null>(() => readStackRestoreSeed(sourceNodeID, project));

  const [targetNode, setTargetNode] = useState(initial?.targetNode || sourceNodeID);
  const [recreate, setRecreate] = useState(initial?.recreate ?? false);
  const [snapshot, setSnapshot] = useState(initial?.snapshot ?? true);
  const [group, setGroup] = useState(""); // "" = latest backup of each service
  const [groups, setGroups] = useState<{ id: string; at: number; services: string[]; complete: boolean }[]>([]);
  const [reconstructHost, setReconstructHost] = useState(initial?.reconstructHost ?? false);
  // #8: off by default — the capture-time finding is the default behaviour.
  const [promoteRestart, setPromoteRestart] = useState(false);
  const [injectHealthchecks, setInjectHealthchecks] = useState(false);
  const [hostBaseDir, setHostBaseDir] = useState(() => {
    if (initial?.hostBaseDir) return initial.hostBaseDir;
    try { return localStorage.getItem("dockback.hostBaseDir") || ""; } catch { return ""; }
  });
  const [remapIP, setRemapIP] = useState(initial?.remapIP ?? false);
  // F195: the domain rewrite for containers DockBack has no profile for — the
  // same literal from→to mechanism as the IP remap, applied to environment
  // values. Both fields are required when enabled; nothing sensible defaults.
  const [remapDomain, setRemapDomain] = useState(initial?.remapDomain ?? false);
  const [domainFrom, setDomainFrom] = useState(initial?.domainFrom || "");
  const [domainTo, setDomainTo] = useState(initial?.domainTo || "");
  const [remapFrom, setRemapFrom] = useState(initial?.remapFrom || "");
  const [remapTo, setRemapTo] = useState(initial?.remapTo || "");
  const [remapPath, setRemapPath] = useState(initial?.remapPath ?? false);
  const [pathFrom, setPathFrom] = useState(initial?.pathFrom || "");
  const [pathTo, setPathTo] = useState(initial?.pathTo || "");
  const [plan, setPlan] = useState<StackRestorePlan | null>(null);
  const [planErr, setPlanErr] = useState("");
  // F209: the offline private key for this stack's write-only members. Component
  // state only — never persisted, never in the URL, and sent in the request BODY.
  // Deliberately NOT carried across the navigation that opens this page: handing
  // key material to a storage API to save one paste is the wrong trade.
  const [privKey, setPrivKey] = useState("");
  // F213: services the operator has UNticked. Stored as the exclusion rather than
  // the selection so a plan that reloads (a different point in time, a changed
  // remap) keeps every new service on by default — the safe direction.
  const [deselected, setDeselected] = useState<Set<string>>(new Set());
  // F214: which COPY every member is read from. "" = auto (integrity-checked
  // local first, then offsite) — the behaviour before this existed.
  const [source, setSource] = useState(initial?.source || "");
  // F215: what this route needed last time. Values are offered, the checkboxes
  // are NOT armed from it — see the prefill effect below for why.
  const [remembered, setRemembered] = useState<CrossRestoreDefaults | null>(null);
  // F210: a member of this stack is marked protected, so overwriting it needs
  // fresh proof of the password — the same gate a single-container restore has.
  const [stepUp, setStepUp] = useState<{ totp: boolean; err: string } | null>(null);
  // F173: the two optional address changes, offered only when this stack's
  // application declares that it records one. Blank is the normal case.
  const [newSiteAddress, setNewSiteAddress] = useState(initial?.newSiteAddress || "");
  const [newUpstreamAddress, setNewUpstreamAddress] = useState(initial?.newUpstreamAddress || "");
  const [planLoading, setPlanLoading] = useState(false);
  const [liveServices, setLiveServices] = useState<string[] | null>(null); // stack members on the node, to show "will be skipped"
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const planSeq = useRef(0);

  const crossHost = targetNode !== sourceNodeID;
  const stackHome = `/servers/${sourceNodeID}/stacks/${encodeURIComponent(project)}`;

  useEffect(() => { api.nodes().then(setNodes).catch(() => setNodes([])); }, []);

  // The point-in-time groups + live members, once.
  useEffect(() => {
    api.stackGroups(sourceNodeID, project).then((gs) => {
      const complete = gs.filter((g) => g.complete);
      setGroups(complete);
      // Honor the clicked backup's point in time: preselect its snapshot group
      // when that group can cover the whole stack.
      if (initial?.group && complete.some((g) => g.id === initial.group)) setGroup(initial.group);
    }).catch(() => setGroups([]));
    api.containers(sourceNodeID).then(({ containers }) => setLiveServices(
      containers.filter((c) => c.stack === project).map((c) => c.service || c.name),
    )).catch(() => setLiveServices(null));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sourceNodeID, project]);

  // F215: fetch what this route needed last time, whenever the target changes.
  useEffect(() => {
    if (!targetNode) { setRemembered(null); return; }
    let gone = false;
    api.stackRestoreDefaults(sourceNodeID, project, targetNode)
      .then((d) => { if (!gone) setRemembered(d && Object.keys(d).length > 0 ? d : null); })
      .catch(() => { if (!gone) setRemembered(null); });
    return () => { gone = true; };
  }, [sourceNodeID, project, targetNode]);

  // F215: offer the remembered VALUES, never arm the switches.
  //
  // Auto-ticking a remap would mean opening this page, clicking restore, and
  // having environment values rewritten because of something done weeks ago — a
  // destructive default driven by history rather than intent. So only empty
  // fields are filled, the operator still decides what applies, and the line
  // under the target select tells them what this route used last time.
  useEffect(() => {
    if (!remembered) return;
    setDomainFrom((c) => c || remembered.remap_from_domain || "");
    setDomainTo((c) => c || remembered.remap_to_domain || "");
    setPathFrom((c) => c || remembered.remap_from_path || "");
    setPathTo((c) => c || remembered.remap_to_path || "");
    setRemapFrom((c) => c || remembered.remap_from_ip || "");
    setRemapTo((c) => c || remembered.remap_to_ip || "");
    setHostBaseDir((c) => c || remembered.host_base_dir || "");
    setNewSiteAddress((c) => c || remembered.new_site_address || "");
    setNewUpstreamAddress((c) => c || remembered.new_upstream_address || "");
  }, [remembered]);

  // Prefill remap fields on enable (same behavior as the per-backup drawer).
  useEffect(() => {
    if (!remapIP) return;
    setRemapFrom((c) => c || ipFromNodeAddr(nodes.find((n) => n.id === sourceNodeID)?.address));
    setRemapTo((c) => c || ipFromNodeAddr(nodes.find((n) => n.id === targetNode)?.address));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [remapIP, nodes]);
  useEffect(() => { if (remapIP) setRemapTo(ipFromNodeAddr(nodes.find((n) => n.id === targetNode)?.address)); /* eslint-disable-next-line react-hooks/exhaustive-deps */ }, [targetNode]);
  useEffect(() => {
    if (!remapPath) return;
    setPathTo((c) => c || hostBaseDir.trim());
    // From-base is derived server-side from the stack's recorded compose dir when
    // left blank — the plan preview shows the effect either way.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [remapPath]);

  // The plan (F82): refetch on every option that changes it, debounced 300 ms.
  // The confirm stays disabled until the CURRENT plan has loaded.
  useEffect(() => {
    const seq = ++planSeq.current;
    setPlanLoading(true); setPlanErr("");
    const t = setTimeout(() => {
      api.stackRestorePlan(sourceNodeID, project, {
        ...(group ? { group } : {}),
        ...(partial ? { services: keptServices.map((s) => s.service) } : {}),
        ...(reconstructHost ? { reconstruct_host: true, ...(hostBaseDir.trim() ? { host_base_dir: hostBaseDir.trim() } : {}) } : {}),
        ...(remapPath ? { remap_path: true, ...(pathFrom.trim() ? { remap_from_path: pathFrom.trim() } : {}), ...(pathTo.trim() ? { remap_to_path: pathTo.trim() } : {}) } : {}),
        // F81: the plan can only say which bind sources are missing if it knows
        // which host to look at.
        ...(targetNode && targetNode !== sourceNodeID ? { target_node: targetNode } : {}),
      }).then((p) => { if (planSeq.current === seq) { setPlan(p); setPlanLoading(false); } })
        .catch((e) => { if (planSeq.current === seq) { setPlan(null); setPlanErr((e as Error).message); setPlanLoading(false); } });
    }, 300);
    return () => clearTimeout(t);
    // keptServices is recomputed from `plan` on every render, so it cannot be a
    // dependency without looping; `deselected` is the state behind it and is the
    // thing that actually changes. Everything else the request sends is listed.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sourceNodeID, project, group, deselected, reconstructHost, hostBaseDir, remapPath, pathFrom, pathTo, targetNode]);

  const start = async (creds?: { password: string; code: string }) => {
    const targetName = nodes.find((n) => n.id === targetNode)?.name || targetNode;
    const n = keptServices.length;
    const total = plan?.services.length || 0;
    const scope = partial ? `${n} of ${total} service(s)` : `ALL ${n} service(s)`;
    const head = recreate
      ? `REVERT STACK UPDATE "${project}" — recreate ${scope} from the planned backups' saved images (rolling a broken upgrade back), in the order shown, on node ${targetName}${crossHost ? " (CROSS-HOST)" : ""}.`
      : `RESTORE STACK "${project}" — recreate and restore ${scope} in the order shown on node ${targetName}${crossHost ? " (CROSS-HOST — recreating the stack there)" : ""}.`;
    // ALL-services guarantee: a member with no backup CANNOT be recreated —
    // force explicit acknowledgment instead of a quietly incomplete stack.
    const gap = skippedLive.length > 0
      ? `\n\nWARNING: ${skippedLive.length} stack member(s) have NO backup and will NOT exist after this restore: ${skippedLive.join(", ")}. Run a stack backup first to cover every service.`
      : "";
    // F210: the destructive confirm is asked ONCE. On a step-up retry the
    // operator has already agreed to it — re-asking would train them to click
    // through the very dialog that carries the warning.
    if (!creds && !confirm(`${head}${snapshot ? " Current state is snapshotted first." : " No safety snapshot will be taken."}${gap} This is destructive. Continue?`)) return;
    setBusy(true); setErr("");
    try {
      await api.restoreStack(sourceNodeID, project, {
        recreate, snapshot, target_node: targetNode,
        ...(privKey.trim() ? { private_key: privKey.trim() } : {}),
        ...(creds ? { password: creds.password, code: creds.code } : {}),
        ...(newSiteAddress.trim() ? { new_site_address: newSiteAddress.trim() } : {}),
        ...(newUpstreamAddress.trim() ? { new_upstream_address: newUpstreamAddress.trim() } : {}),
        ...(group ? { group } : {}),
        ...(reconstructHost ? { reconstruct_host: true, ...(hostBaseDir.trim() ? { host_base_dir: hostBaseDir.trim() } : {}) } : {}),
        ...(promoteRestart ? { promote_restart_policy: true } : {}),
        ...(injectHealthchecks ? { inject_healthchecks: true } : {}),
        ...(remapIP ? { remap_ip: true, ...(remapFrom.trim() ? { remap_from_ip: remapFrom.trim() } : {}), ...(remapTo.trim() ? { remap_to_ip: remapTo.trim() } : {}) } : {}),
        ...(remapDomain && domainFrom.trim() && domainTo.trim() ? { remap_domain: true, remap_from_domain: domainFrom.trim(), remap_to_domain: domainTo.trim() } : {}),
        ...(remapPath ? { remap_path: true, ...(pathFrom.trim() ? { remap_from_path: pathFrom.trim() } : {}), ...(pathTo.trim() ? { remap_to_path: pathTo.trim() } : {}) } : {}),
      });
      setStepUp(null);
      // The stack's own page carries the console that follows this run.
      navigate(`${stackHome}?follow=restore`);
    } catch (e) {
      // F210: a protected member wants the password. That is a prompt, not a
      // failure — the operator is entitled to this restore, they just have to
      // prove who they are first.
      if (e instanceof StepUpError) {
        setStepUp({ totp: e.totp_required, err: creds ? e.message : "" });
        return;
      }
      setErr((e as Error).message);
    } finally { setBusy(false); }
  };

  // F213: what this run will actually cover. An atomic member can never be
  // dropped, so it is filtered out of the exclusion set rather than relied on to
  // stay unticked.
  const isKept = (svc: string, atomic?: boolean) => atomic || !deselected.has(svc);
  const keptServices = (plan?.services || []).filter((s) => isKept(s.service, s.atomic));
  const partial = !!plan && keptServices.length < plan.services.length;
  const toggleService = (svc: string, on: boolean) =>
    setDeselected((prev) => { const n = new Set(prev); if (on) n.delete(svc); else n.add(svc); return n; });

  // F214: the chosen copy does not cover every member — say so before the
  // confirm rather than letting the fallback be a surprise in the log.
  const sourcePartial = (plan?.source_copies || []).find(
    (c) => c.id === source && plan && c.services < plan.services.length,
  );

  // F215: a short, human list of what this route used last time.
  const rememberedSummary = ((): string[] => {
    const r = remembered;
    if (!r) return [];
    const out: string[] = [];
    if (r.remap_from_domain && r.remap_to_domain) out.push(`domain ${r.remap_from_domain} → ${r.remap_to_domain}`);
    if (r.remap_from_path && r.remap_to_path) out.push(`paths ${r.remap_from_path} → ${r.remap_to_path}`);
    if (r.remap_from_ip && r.remap_to_ip) out.push(`IP ${r.remap_from_ip} → ${r.remap_to_ip}`);
    if (r.new_site_address) out.push(`address ${r.new_site_address}`);
    if (r.new_upstream_address) out.push(`upstream ${r.new_upstream_address}`);
    if (r.host_base_dir) out.push(`base folder ${r.host_base_dir}`);
    return out;
  })();

  const planned = new Set((plan?.services || []).map((s) => s.service));
  // F216: the members a restore cannot bring back. The plan carries the server's
  // own answer from its cached inventory — so the list is right even when the
  // source node is down, which is exactly when it matters; the live call stays as
  // enrichment for anything created since the last refresh.
  //
  // Only ever computed against a plan that ARRIVED. The live half of this list is
  // "every service the plan did not mention", and a plan that failed or has not
  // returned mentions nothing — so without this guard a rejected dialog option
  // renders every service in the stack as having no backup, telling an operator
  // mid-recovery that their backups are gone while the snapshot picker beside it
  // lists them. A question this card cannot answer yet must be left unanswered.
  const planResolved = !!plan && !planErr && !planLoading;
  const skippedLive = !planResolved ? [] : Array.from(new Set([
    ...(plan.missing_members || []),
    ...(liveServices || []).filter((svc) => !planned.has(svc)),
  ])).sort();
  // F209: does this selection contain a member DockBack cannot open on its own?
  // F213: only the KEPT services can demand a key — deselecting the write-only
  // member means this run never opens it.
  const writeOnlyServices = keptServices.filter((s) => s.write_only).map((s) => s.service);
  const needsKey = writeOnlyServices.length > 0;
  // A base directory the dialog can already see is wrong blocks the button here,
  // rather than being discovered by the plan request and returned as a banner.
  const baseDirsInvalid = (remapPath && (baseDirInvalid(pathFrom) || baseDirInvalid(pathTo))) ||
    (reconstructHost && baseDirInvalid(hostBaseDir));
  const blocked = busy || planLoading || !plan || keptServices.length === 0 || !!plan.atomic_block || (needsKey && !privKey.trim()) || baseDirsInvalid;
  const targetName = nodes.find((n) => n.id === targetNode)?.name || targetNode;

  return (
    <div>
      <nav className="mb-6 flex flex-wrap items-center gap-2 text-xs uppercase tracking-wider text-on-surface-variant">
        <Link to="/servers" className="hover:text-primary">Servers</Link>
        <ChevronRight size={14} className="shrink-0" />
        <Link to={`/servers/${sourceNodeID}`} className="hover:text-primary">{nodes.find((n) => n.id === sourceNodeID)?.name || sourceNodeID}</Link>
        <ChevronRight size={14} className="shrink-0" />
        <Link to={stackHome} className="min-w-0 break-all hover:text-primary">{project}</Link>
        <ChevronRight size={14} className="shrink-0" />
        <span className="font-semibold text-on-surface">Restore</span>
      </nav>

      <Card className="mb-5 p-5">
        <div className="flex flex-wrap items-center gap-3">
          <h1 className="min-w-0 break-words text-2xl font-bold">Restore stack {project}</h1>
          {crossHost && <Chip kind="warn"><AlertTriangle size={11} /> cross-host rebuild</Chip>}
          <span className="ml-auto text-sm text-on-surface-variant">
            source catalog: <b className="text-on-surface">{nodes.find((n) => n.id === sourceNodeID)?.name || sourceNodeID}</b>
          </span>
        </div>
        <div className="mt-4 flex items-start gap-2.5 rounded border border-error/40 bg-error/10 px-3 py-2.5 text-sm text-error">
          <AlertTriangle size={16} className="mt-0.5 shrink-0" />
          <p className="min-w-0 break-words">
            <b>This is destructive.</b> Every selected service is recreated on <b>{targetName}</b> and its volumes and
            databases are overwritten from the backup. Nothing runs until you confirm at the bottom.
          </p>
        </div>
      </Card>

      <div className="grid grid-cols-1 gap-5 lg:grid-cols-3">
        {/* LEFT — the plan gets the room, because it is what must be read. */}
        <div className="space-y-5 lg:col-span-2">
          <Card className="overflow-hidden">
            <div className="flex flex-wrap items-center gap-x-2 gap-y-1 border-b border-outline-variant/60 px-5 py-3">
              <Layers size={15} className="shrink-0 text-primary" />
              <h2 className="text-base font-bold">Restore plan</h2>
              <span className="min-w-0 text-xs text-on-surface-variant">
                {plan ? `${keptServices.length} of ${plan.services.length} services · ` : ""}in dependency order, databases first
              </span>
              {planLoading && <span className="flex items-center gap-1 text-xs text-on-surface-variant"><Loader2 size={12} className="animate-spin" /> Loading plan…</span>}
              {plan && plan.services.length > 0 && keptServices.length > 0 && (
                <button onClick={() => setDeselected(new Set((plan.services || []).filter((s) => !s.atomic).map((s) => s.service)))}
                  className="ml-auto shrink-0 rounded border border-outline-variant px-2 py-0.5 text-xs text-on-surface-variant hover:text-on-surface">
                  Deselect all
                </button>
              )}
            </div>

            <div className="p-5">
              {planErr && <div className="flex items-start gap-2 text-xs text-error"><AlertTriangle size={13} className="mt-0.5 shrink-0" /> {planErr}</div>}
              {!planErr && plan && plan.services.length === 0 && (
                <div className="text-sm text-on-surface-variant">
                  No backups found for this stack — nothing can be restored.
                  {skippedLive.length > 0 && (
                    <span className="mt-1 block break-words text-warning">
                      {skippedLive.length} service{skippedLive.length === 1 ? "" : "s"} would be left behind: {skippedLive.join(", ")}. Run a stack backup first.
                    </span>
                  )}
                </div>
              )}
              {!planErr && plan && plan.services.length > 0 && (
                <ol className="divide-y divide-outline-variant/40">
                  {plan.services.map((s) => (
                    <li key={s.service} className={`flex gap-3 py-3 first:pt-0 last:pb-0 ${isKept(s.service, s.atomic) ? "" : "opacity-60"}`}>
                      {/* F213: pick the services this run covers. An atomic member
                          is disabled rather than refused after the fact —
                          restoring a subset of one produces a healthy-looking
                          deployment that does not work, and the operator should
                          learn that here and not from an error after confirming
                          something destructive. */}
                      <input
                        type="checkbox"
                        className="mt-1 shrink-0"
                        checked={isKept(s.service, s.atomic)}
                        disabled={!!s.atomic}
                        aria-label={`Restore ${s.service}`}
                        title={s.atomic ? "This application's services are only meaningful together — it is restored as a whole or not at all" : `Restore ${s.service}`}
                        onChange={(e) => toggleService(s.service, e.target.checked)}
                      />
                      <span className="mt-0.5 grid h-5 w-5 shrink-0 place-items-center rounded-full bg-surface-highest text-[10px] font-bold tnum">{s.order}</span>
                      <div className="min-w-0 flex-1">
                        <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                          {s.data_tier ? <Database size={13} className="shrink-0 text-secondary" /> : <Box size={13} className="shrink-0 text-on-surface-variant" />}
                          <span className="min-w-0 break-all font-semibold text-on-surface">{s.service}</span>
                          <span className="text-xs text-on-surface-variant">restores from {fmtAgo(s.created_at)}</span>
                          {s.verified === "verified" && <Chip kind="ok"><ShieldCheck size={11} /> Verified</Chip>}
                          {s.verified !== "verified" && <Chip kind="warn">{s.verified || "unverified"}</Chip>}
                          {s.write_only && <Chip kind="info"><Lock size={11} /> write-only</Chip>}
                          {s.atomic && <Chip kind="muted">atomic</Chip>}
                          {s.partial && <Chip kind="warn"><AlertTriangle size={11} /> Partial</Chip>}
                          {s.incremental && <Chip kind="info"><Layers size={11} /> delta</Chip>}
                          {(s.portability || []).length > 0 && <Chip kind="warn"><AlertTriangle size={11} /> host mismatch</Chip>}
                          {s.restore_block && <Chip kind="err"><AlertTriangle size={11} /> needs a change first</Chip>}
                          {(s.address_vars || []).length > 0 && <Chip kind="warn"><AlertTriangle size={11} /> records an address</Chip>}
                        </div>
                        {plan.target_dirs[s.service] && (
                          <p className="mt-1 break-all font-mono text-[11px] text-on-surface-variant">target folder: {plan.target_dirs[s.service]}</p>
                        )}
                        {/* F94: what this target cannot provide, against the one
                            service that has the problem. */}
                        {(s.portability || []).map((w, i) => (
                          <p key={i} className="mt-1 break-words text-[11px] text-warning">• {w}</p>
                        ))}
                        {/* F174: not a host mismatch but a configuration one, and
                            it stops the restore rather than degrading it. */}
                        {s.restore_block && <p className="mt-1 break-words text-[11px] text-error">• {s.restore_block}</p>}
                        {/* F177: this service records where it lives in its own
                            environment, and it is moving. Names only. */}
                        {(s.address_vars || []).length > 0 && (
                          <p className="mt-1 break-words text-[11px] text-warning">
                            • Records an address in {(s.address_vars || []).join(", ")} — carried over from the current host. Fill in the new address on the right if it changes.
                          </p>
                        )}
                        {/* F81: the host paths this target does not have yet. Shown
                            before the operator confirms, because a directory that
                            appears on a machine is only reassuring if they were
                            told it was going to. */}
                        {(s.bind_plan || []).length > 0 && <BindPlanList rows={s.bind_plan || []} />}
                      </div>
                    </li>
                  ))}
                </ol>
              )}
            </div>
          </Card>

          {/* F216: members this restore cannot bring back. Its own card, because
              "these will not exist afterwards" is not a footnote. */}
          {skippedLive.length > 0 && (
            <Card className="p-5">
              <div className="mb-1 text-xs font-semibold uppercase tracking-widest text-on-surface-variant">Not in this restore</div>
              <div className="flex flex-wrap items-center gap-2">
                {skippedLive.map((s) => <Chip key={s} kind="warn">{s}</Chip>)}
              </div>
              <p className="mt-2 break-words text-xs text-on-surface-variant">
                {skippedLive.length === 1 ? "This member of the stack has" : "These members of the stack have"} no backup &mdash;
                {skippedLive.length === 1 ? " it" : " they"} will not come back. Read from the catalog, so the list is right even when the source node is offline.
              </p>
            </Card>
          )}

          {/* F146: this application's services are only meaningful together and
              the chosen backups do not form one complete snapshot. The fix is to
              pick a different point in time, which is a choice on this page. */}
          {plan?.atomic_block && (
            <Card className="border-error/30 bg-error/10 p-5">
              <div className="flex items-center gap-2 font-semibold text-error">
                <AlertTriangle size={15} className="shrink-0" /> This restore is not allowed
              </div>
              <p className="mt-1 break-words text-sm text-error">{plan.atomic_block}</p>
            </Card>
          )}

          {/* #37: the numbers, before the commit. R5's operator moved a 58 GB
              stack to another disk because they could see them. */}
          {plan?.capacity && <CapacityPanel c={plan.capacity} />}

          {/* F149: what the restored stack needs from wherever it lands. */}
          {plan?.app_preconditions && (plan.app_preconditions.notes || []).length > 0 && (
            <Card className={`p-5 ${plan.app_preconditions.blocking ? "border-warning/40" : ""}`}>
              <div className={`flex flex-wrap items-center gap-2 font-semibold ${plan.app_preconditions.blocking ? "text-warning" : "text-primary"}`}>
                {plan.app_preconditions.blocking ? <AlertTriangle size={15} className="shrink-0" /> : <Info size={15} className="shrink-0" />}
                <span className="min-w-0 break-words">{plan.app_preconditions.app} — check these before restoring</span>
              </div>
              <ul className="mt-2 space-y-1 pl-5 text-sm text-on-surface-variant">
                {(plan.app_preconditions.notes || []).map((n, i) => <li key={i} className="break-words">• {n}</li>)}
              </ul>
              {/* F173: the fields those notes refer to. Blank — the default —
                  changes nothing. */}
              {plan.app_preconditions.address_prompt && (
                <div className="mt-3">
                  <div className="mb-1 text-xs font-medium text-on-surface-variant">{plan.app_preconditions.address_prompt}</div>
                  <input
                    className="w-full rounded border border-outline bg-surface px-2 py-1.5 font-mono text-xs text-on-surface placeholder:font-sans placeholder:text-on-surface-variant"
                    value={newSiteAddress} onChange={(e) => setNewSiteAddress(e.target.value)}
                    placeholder="https://cloud.example.com — leave blank to keep the current address"
                    spellCheck={false} autoCapitalize="off" autoCorrect="off"
                  />
                  <p className="mt-1 break-words text-[11px] text-on-surface-variant">
                    Only fill this in if the address is genuinely changing. Re-pointing DNS or your reverse proxy at the new
                    host keeps the same address and needs nothing here. Settings DockBack cannot undo are printed for you to
                    run, never applied automatically.
                  </p>
                </div>
              )}
              {plan.app_preconditions.upstream_prompt && (
                <div className="mt-3">
                  <div className="mb-1 text-xs font-medium text-on-surface-variant">{plan.app_preconditions.upstream_prompt}</div>
                  <input
                    className="w-full rounded border border-outline bg-surface px-2 py-1.5 font-mono text-xs text-on-surface placeholder:font-sans placeholder:text-on-surface-variant"
                    value={newUpstreamAddress} onChange={(e) => setNewUpstreamAddress(e.target.value)}
                    placeholder="http://10.168.1.50:32400 — leave blank if it has not moved"
                    spellCheck={false} autoCapitalize="off" autoCorrect="off"
                  />
                  <p className="mt-1 break-words text-[11px] text-on-surface-variant">
                    Only the recorded address changes. Nothing is re-authenticated — the stored credential is bound to that
                    service's identity, not to where it lives.
                  </p>
                </div>
              )}
            </Card>
          )}
        </div>

        {/* RIGHT — every option, one column, nothing hidden behind a scroll. */}
        <div className="space-y-5">
          <Card className="p-5">
            {/* F215: what this route needed last time. Shown rather than applied. */}
            {rememberedSummary.length > 0 && (
              <p className="mb-4 flex flex-wrap items-start gap-x-1.5 gap-y-1 rounded bg-surface-lowest px-3 py-2 text-xs text-on-surface-variant">
                <Info size={13} className="mt-0.5 shrink-0" />
                <span className="min-w-0 break-words">
                  Last restore of <b className="text-on-surface">{project}</b> to this machine used {rememberedSummary.join(", ")}. Those values are filled in below &mdash; tick what still applies.
                </span>
              </p>
            )}

            {/* Target node (F52). */}
            <div className="mb-4">
              <div className="mb-1 text-xs font-medium uppercase tracking-wider text-on-surface-variant">Restore to</div>
              <Select value={targetNode} onChange={(e) => setTargetNode(e.target.value)}>
                {nodes.filter((n) => n.reachable || n.id === sourceNodeID).map((n) => (
                  <option key={n.id} value={n.id}>{n.id === sourceNodeID ? `${n.name} — this node (original)` : n.name}</option>
                ))}
              </Select>
              {crossHost && (
                <p className="mt-1.5 break-words text-xs text-warning">
                  Restores every service onto the selected node — images are re-pulled by digest (or loaded from bundled image.tar) and volumes are recreated there. The original node is not touched.
                </p>
              )}
            </div>

            {/* Point-in-time (F43). */}
            {groups.length > 0 && (
              <div className="mb-4">
                <div className="mb-1 text-xs font-medium uppercase tracking-wider text-on-surface-variant">Point in time</div>
                <div className="flex flex-col gap-1.5">
                  <label className="flex cursor-pointer items-start gap-2 rounded border border-outline-variant/60 px-3 py-2 text-sm">
                    <input type="radio" name="srp-group" className="mt-0.5 shrink-0" checked={group === ""} onChange={() => setGroup("")} />
                    <span className="min-w-0"><span className="font-medium text-on-surface">Latest backup of each service</span><span className="text-on-surface-variant"> (may span different times)</span></span>
                  </label>
                  {groups.map((g) => (
                    <label key={g.id} className="flex cursor-pointer items-start gap-2 rounded border border-outline-variant/60 px-3 py-2 text-sm">
                      <input type="radio" name="srp-group" className="mt-0.5 shrink-0" checked={group === g.id} onChange={() => setGroup(g.id)} />
                      <span className="min-w-0">
                        <span className="font-medium text-on-surface">App-consistent snapshot</span>
                        <span className="text-on-surface-variant"> — captured together at {new Date(g.at * 1000).toLocaleString()}</span>
                        <span className="mt-0.5 block break-words text-xs text-on-surface-variant">{g.services.length} service{g.services.length === 1 ? "" : "s"}: {g.services.join(", ")}</span>
                      </span>
                    </label>
                  ))}
                </div>
              </div>
            )}

            {/* F214: WHICH COPY to read from. Only copies the planned members
                actually hold are offered, each saying how many it covers. */}
            {(plan?.source_copies || []).length > 0 && (
              <div className="mb-4">
                <div className="mb-1 text-xs font-medium uppercase tracking-wider text-on-surface-variant">Source copy</div>
                <Select value={source} onChange={(e) => setSource(e.target.value)}>
                  <option value="">Auto — fastest available (local first)</option>
                  {(plan?.source_copies || []).map((c) => (
                    <option key={c.id} value={c.id}>
                      {c.id === "local" ? "Local (fastest)" : `${c.name} (${c.type})`}
                      {plan && c.services < plan.services.length ? ` — ${c.services} of ${plan.services.length} services` : ""}
                    </option>
                  ))}
                </Select>
                {sourcePartial && (
                  <p className="mt-1.5 break-words text-xs text-warning">
                    Only {sourcePartial.services} of {plan?.services.length} services have a copy there — the rest fall back to their best available copy, and the log names which.
                  </p>
                )}
              </div>
            )}

            {/* Revert + snapshot. */}
            <label className="mb-2 flex cursor-pointer items-start gap-2 rounded border border-outline-variant/50 bg-surface-lowest px-3 py-2 text-sm text-on-surface-variant">
              <input type="checkbox" className="mt-0.5 shrink-0" checked={recreate} onChange={(e) => setRecreate(e.target.checked)} />
              <span className="min-w-0 break-words"><span className="font-medium text-on-surface">Revert update</span> — recreate each service from its backup's saved image (digest-pinned when recorded), rolling a broken upgrade back to the backed-up version.</span>
            </label>
            <label className="mb-2 flex cursor-pointer items-start gap-2 rounded border border-outline-variant/50 bg-surface-lowest px-3 py-2 text-sm text-on-surface-variant">
              <input type="checkbox" className="mt-0.5 shrink-0" checked={snapshot} onChange={(e) => setSnapshot(e.target.checked)} />
              <span className="min-w-0 break-words"><span className="font-medium text-on-surface">Safety snapshot first</span> — back up each service's current state before overwriting, so a bad restore can roll back.</span>
            </label>

            {/* Host reconstruction + remaps (parity with the per-backup drawer). */}
            <label className="mb-2 flex cursor-pointer items-start gap-2 rounded border border-outline-variant/50 bg-surface-lowest px-3 py-2 text-sm text-on-surface-variant">
              <input type="checkbox" className="mt-0.5 shrink-0" checked={reconstructHost} onChange={(e) => setReconstructHost(e.target.checked)} />
              <span className="min-w-0 break-words"><span className="font-medium text-on-surface">Reconstruct stack folder on host</span> — rebuild each service's on-host directory and write the reconstructed compose file back, so a fresh machine gets the organized layout, not just containers.</span>
            </label>
            {reconstructHost && partial && (
              <p className="mb-2 break-words rounded bg-warning/10 px-3 py-2 text-xs text-warning">
                The stack&rsquo;s compose file is <b>not</b> rewritten when only some services are restored — it describes the whole project, and a partial restore has no business replacing it.
              </p>
            )}
            {reconstructHost && (
              <div className="mb-2 rounded bg-secondary/10 px-3 py-2 text-xs text-secondary">
                <span className="mb-1 block">Base folder for non-compose services:</span>
                <input value={hostBaseDir} onChange={(e) => { setHostBaseDir(e.target.value); try { localStorage.setItem("dockback.hostBaseDir", e.target.value); } catch { /* ignore */ } }}
                  placeholder="/opt/docker" spellCheck={false}
                  className={`w-full rounded border bg-surface px-2 py-1 font-mono text-on-surface placeholder:text-on-surface-variant/50 ${baseDirProblem(hostBaseDir) ? "border-error" : "border-outline-variant/60"}`} />
                {baseDirProblem(hostBaseDir) && <div className="mt-1 break-words text-error">{baseDirProblem(hostBaseDir)}</div>}
              </div>
            )}
            {/* #8: a restart policy is not portable semantics — its meaning depends
                on what else starts the stack on this host. Offered, never assumed. */}
            <label className="mb-2 flex cursor-pointer items-start gap-2 rounded border border-outline-variant/50 bg-surface-lowest px-3 py-2 text-sm text-on-surface-variant">
              <input type="checkbox" className="mt-0.5 shrink-0" checked={promoteRestart} onChange={(e) => setPromoteRestart(e.target.checked)} />
              <span className="min-w-0 break-words"><span className="font-medium text-on-surface">Start automatically after a reboot</span> — set any service whose restart policy is <code className="font-mono">no</code> or <code className="font-mono">on-failure</code> to <code className="font-mono">unless-stopped</code>. Those policies do not start a container when the Docker daemon starts, so on a plain host the stack is gone after a reboot. Leave this off to reproduce exactly what the source had.</span>
            </label>
            {/* #16: a DB with no probe plus `depends_on: service_started` is a race
                on every boot — the dependants start before the database can answer.
                Offered, never assumed, and never over an existing probe. */}
            <label className="mb-2 flex cursor-pointer items-start gap-2 rounded border border-outline-variant/50 bg-surface-lowest px-3 py-2 text-sm text-on-surface-variant">
              <input type="checkbox" className="mt-0.5 shrink-0" checked={injectHealthchecks} onChange={(e) => setInjectHealthchecks(e.target.checked)} />
              <span className="min-w-0 break-words"><span className="font-medium text-on-surface">Add a readiness check to databases that have none</span> — a database with no healthcheck reports nothing better than <code className="font-mono">running</code>, so services waiting on it start before it can answer queries. Applies to MariaDB, PostgreSQL and Redis services that define no check of their own; an existing check is never replaced.</span>
            </label>
            <label className="mb-2 flex cursor-pointer items-start gap-2 rounded border border-outline-variant/50 bg-surface-lowest px-3 py-2 text-sm text-on-surface-variant">
              <input type="checkbox" className="mt-0.5 shrink-0" checked={remapIP} onChange={(e) => setRemapIP(e.target.checked)} />
              <span className="min-w-0 break-words"><span className="font-medium text-on-surface">Remap machine IP</span> — rewrite the source machine's IP to the target machine's in each recreated config{reconstructHost ? " and compose file" : ""}.{!crossHost && <span className="text-on-surface-variant/80"> Mainly for a cross-host restore.</span>}</span>
            </label>
            {remapIP && (
              <div className="mb-2 grid grid-cols-[auto_1fr] items-center gap-x-2 gap-y-1.5 rounded bg-secondary/10 px-3 py-2 text-xs text-secondary">
                <span className="whitespace-nowrap">From (source host):</span>
                <input value={remapFrom} onChange={(e) => setRemapFrom(e.target.value)} placeholder="10.168.1.10" spellCheck={false}
                  className="w-full rounded border border-outline-variant/60 bg-surface px-2 py-1 font-mono text-on-surface placeholder:text-on-surface-variant/50" />
                <span className="whitespace-nowrap">To (target host):</span>
                <input value={remapTo} onChange={(e) => setRemapTo(e.target.value)} placeholder="10.168.1.20" spellCheck={false}
                  className="w-full rounded border border-outline-variant/60 bg-surface px-2 py-1 font-mono text-on-surface placeholder:text-on-surface-variant/50" />
                {(!remapFrom.trim() || !remapTo.trim()) && (
                  <span className="col-span-2 break-words text-on-surface-variant">
                    A blank field is filled from the node&rsquo;s address when the restore starts — a node registered by hostname is resolved to its IP. If neither yields one, the restore refuses and asks for it here.
                  </span>
                )}
              </div>
            )}
            <label className="mb-2 flex cursor-pointer items-start gap-2 rounded border border-outline-variant/50 bg-surface-lowest px-3 py-2 text-sm text-on-surface-variant">
              <input type="checkbox" className="mt-0.5 shrink-0" checked={remapDomain} onChange={(e) => setRemapDomain(e.target.checked)} />
              <span className="min-w-0 break-words"><span className="font-medium text-on-surface">Remap domain</span> — rewrite the old domain to the new one wherever a service&rsquo;s environment carries it (base URLs, CORS and trusted-host lists). Works for every container, profile or not; only values naming the old domain change, so upstreams pointing elsewhere are never touched.</span>
            </label>
            {remapDomain && (
              <div className="mb-2 grid grid-cols-[auto_1fr] items-center gap-x-2 gap-y-1.5 rounded bg-secondary/10 px-3 py-2 text-xs text-secondary">
                <span className="whitespace-nowrap">Current domain:</span>
                <input value={domainFrom} onChange={(e) => setDomainFrom(e.target.value)} placeholder="app.old-home.net" spellCheck={false}
                  className="w-full rounded border border-outline-variant/60 bg-surface px-2 py-1 font-mono text-on-surface placeholder:text-on-surface-variant/50" />
                <span className="whitespace-nowrap">New domain:</span>
                <input value={domainTo} onChange={(e) => setDomainTo(e.target.value)} placeholder="app.new-home.net" spellCheck={false}
                  className="w-full rounded border border-outline-variant/60 bg-surface px-2 py-1 font-mono text-on-surface placeholder:text-on-surface-variant/50" />
                <span className="col-span-2 break-words text-on-surface-variant">Plain domain names only &mdash; no scheme, port or path. Subdomains are separate hosts and need their own pass.</span>
              </div>
            )}
            <label className="mb-2 flex cursor-pointer items-start gap-2 rounded border border-outline-variant/50 bg-surface-lowest px-3 py-2 text-sm text-on-surface-variant">
              <input type="checkbox" className="mt-0.5 shrink-0" checked={remapPath} onChange={(e) => setRemapPath(e.target.checked)} />
              <span className="min-w-0 break-words"><span className="font-medium text-on-surface">Remap stack paths</span> — rewrites bind-mount folders{reconstructHost ? ", the compose file, and the stack folder" : ""} from the old machine's layout to this machine's — exact matches only, every change is logged.</span>
            </label>
            {remapPath && (
              <div className="grid grid-cols-[auto_1fr] items-center gap-x-2 gap-y-1.5 rounded bg-secondary/10 px-3 py-2 text-xs text-secondary">
                <span className="whitespace-nowrap">From base folder:</span>
                <input value={pathFrom} onChange={(e) => setPathFrom(e.target.value)} placeholder="auto — the stack's recorded base" spellCheck={false}
                  className={`w-full rounded border bg-surface px-2 py-1 font-mono text-on-surface placeholder:text-on-surface-variant/50 ${baseDirProblem(pathFrom) ? "border-error" : "border-outline-variant/60"}`} />
                {baseDirProblem(pathFrom) && <span className="col-span-2 break-words text-error">{baseDirProblem(pathFrom)}</span>}
                <span className="whitespace-nowrap">To base folder:</span>
                <input value={pathTo} onChange={(e) => setPathTo(e.target.value)} placeholder="/opt/stacks" spellCheck={false}
                  className={`w-full rounded border bg-surface px-2 py-1 font-mono text-on-surface placeholder:text-on-surface-variant/50 ${baseDirProblem(pathTo) ? "border-error" : "border-outline-variant/60"}`} />
                {baseDirProblem(pathTo) && <span className="col-span-2 break-words text-error">{baseDirProblem(pathTo)}</span>}
              </div>
            )}
          </Card>

          {/* F209: this stack has a write-only member. Asked for BEFORE the
              confirm — the restore used to accept no key at all, overwrite the
              app services, then stop dead at the sealed one with nowhere to put
              it. */}
          {needsKey && (
            <Card className="border-primary/30 bg-primary/[0.06] p-5">
              <div className="mb-1 flex flex-wrap items-center gap-2 text-sm font-semibold text-primary">
                <Lock size={14} className="shrink-0" /> Offline private key
              </div>
              <p className="mb-2 break-words text-xs text-on-surface-variant">
                This stack contains <b className="text-on-surface">write-only encrypted</b> backups
                {writeOnlyServices.length === 1
                  ? <> ({writeOnlyServices[0]})</>
                  : <> ({writeOnlyServices.length} services: {writeOnlyServices.join(", ")})</>}
                . DockBack sealed them to a public key and cannot open them itself &mdash; paste the private key from their recovery sheet.
                It is checked against every sealed member <b className="text-on-surface">before anything is restored</b>, used for this restore only, and never saved or logged.
              </p>
              <textarea
                value={privKey} onChange={(e) => setPrivKey(e.target.value)}
                rows={3} spellCheck={false} autoComplete="off"
                placeholder="base64 private key" aria-label="Offline private key"
                className="w-full break-all rounded border border-outline-variant bg-surface-lowest px-3 py-2 font-mono text-xs text-on-surface focus:outline-none focus:ring-1 focus:ring-primary"
              />
              {!privKey.trim() && <p className="mt-1 text-[11px] text-outline">Required &mdash; the restore cannot start without it.</p>}
            </Card>
          )}

          {/* F210: a protected member's overwrite needs the password. */}
          {stepUp && (
            <StepUpPrompt totp={stepUp.totp} busy={busy} error={stepUp.err} confirmLabel="Confirm and restore"
              onConfirm={(pw, code) => start({ password: pw, code })} />
          )}
        </div>
      </div>

      {err && (
        <div className="mt-5 flex items-start gap-2 rounded bg-error/10 px-3 py-2 text-sm text-error">
          <AlertTriangle size={15} className="mt-0.5 shrink-0" /> <span className="min-w-0 break-words">{err}</span>
        </div>
      )}

      {/* The confirm, stating what it will do in the same sentence as the button. */}
      <div className="sticky bottom-0 z-10 mt-5 flex flex-wrap items-center gap-3 rounded-lg border border-outline-variant bg-surface-container/95 px-4 py-3 backdrop-blur">
        <p className="min-w-0 flex-1 break-words text-xs text-on-surface-variant">
          {plan
            ? <><b className="text-on-surface">{keptServices.length} of {plan.services.length} service{plan.services.length === 1 ? "" : "s"}</b> onto <b className="text-on-surface">{targetName}</b>
                {snapshot ? " · safety snapshot first" : " · NO safety snapshot"}
                {source ? ` · reading from ${(plan.source_copies || []).find((c) => c.id === source)?.name || source}` : " · reading from the fastest copy"}</>
            : "Loading the plan…"}
        </p>
        <Link to={stackHome} className="shrink-0 rounded border border-outline-variant px-3 py-1.5 text-sm text-on-surface-variant hover:text-on-surface">Cancel</Link>
        {/* F146: a refused selection cannot be started. The page says why and what
            to pick instead, rather than letting the confirm through to an error
            the operator only sees after agreeing to something destructive. */}
        <Button variant="danger" onClick={() => start()} disabled={blocked}>
          {busy ? <Loader2 size={15} className="animate-spin" /> : <RotateCcw size={15} />} {recreate ? "Revert stack" : "Restore stack"}
        </Button>
      </div>
    </div>
  );
}

// The host paths the target does not have yet, and what the restore intends for
// each (F81).
//
// Its value is entirely in being read BEFORE the operator confirms. The restore
// creates directories on a machine, and a directory that simply appears is
// indistinguishable from one that appeared by mistake — so the same five words
// the run log will use are shown here first, with the ones DockBack will not
// create called out, because those are the only rows that need a decision.
const BIND_ACTIONS: Record<BindSourcePlan["action"], { label: string; tone: string }> = {
  "fill": { label: "create + fill", tone: "text-on-surface-variant" },
  "create-empty": { label: "create empty", tone: "text-on-surface-variant" },
  "write-file": { label: "write file", tone: "text-on-surface-variant" },
  "placeholder": { label: "empty placeholder", tone: "text-warning" },
  "manual": { label: "you must create", tone: "text-error" },
};

// CapacityPanel reports where the restore lands and what the disk has left.
//
// Shown whether or not the restore is refused: a placement that clears the
// margin can still be the wrong disk, and the numbers are what let an operator
// judge that. Refusal turns it red and the confirm button is already blocked.
function CapacityPanel({ c }: { c: RestoreCapacity }) {
  const tone = capacityTone(c);
  const where = c.mount_point || c.filesystem || "the destination filesystem";
  return (
    <Card className={`p-5 ${c.refuse ? "border-error/30 bg-error/10" : tone === "warning" ? "border-warning/40" : ""}`}>
      <div className={`flex flex-wrap items-center gap-2 font-semibold text-${tone}`}>
        {c.refuse ? <AlertTriangle size={15} className="shrink-0" /> : <HardDrive size={15} className="shrink-0" />}
        <span className="min-w-0 break-words">
          {c.refuse ? "Not enough room on the destination disk" : "Destination capacity"}
        </span>
      </div>
      {/* Labels and values stack on narrow screens and in languages whose words
          for these run long; the grid never forces a fixed label column. */}
      <dl className="mt-2 grid grid-cols-[minmax(0,auto)_minmax(0,1fr)] gap-x-3 gap-y-1 text-sm">
        <dt className="text-on-surface-variant">Will write</dt>
        <dd className="min-w-0 break-words text-on-surface">
          {fmtBytes(c.payload_bytes)}{c.estimated && <span className="text-on-surface-variant"> (estimated)</span>}
        </dd>
        <dt className="text-on-surface-variant">Into</dt>
        <dd className="min-w-0 break-all font-mono text-[13px] text-on-surface">{c.path}</dd>
        <dt className="text-on-surface-variant">On</dt>
        <dd className="min-w-0 break-all text-on-surface">
          {where}{c.total_bytes ? ` — ${fmtBytes(c.free_bytes)} free of ${fmtBytes(c.total_bytes)}` : ` — ${fmtBytes(c.free_bytes)} free`}
        </dd>
        <dt className="text-on-surface-variant">Would leave</dt>
        <dd className={`min-w-0 break-words ${c.refuse ? "text-error" : "text-on-surface"}`}>
          {fmtBytes(Math.max(0, c.after_bytes))}
          <span className="text-on-surface-variant"> (at least {fmtBytes(c.margin_bytes)} must stay clear)</span>
        </dd>
      </dl>
      {c.refuse && (
        <p className="mt-2 break-words text-sm text-error">
          Remap the host paths to a disk with more room, or free space here first. The restore is blocked until then.
        </p>
      )}
    </Card>
  );
}

function BindPlanList({ rows }: { rows: BindSourcePlan[] }) {
  const manual = rows.filter((r) => r.action === "manual").length;
  const creates = rows.length - manual;
  return (
    <div className="mt-1.5 rounded border border-outline-variant/60 bg-surface-lowest px-2.5 py-2">
      <p className="text-[11px] text-on-surface-variant">
        {creates > 0
          ? <>This host is missing {rows.length} bind mount source{rows.length === 1 ? "" : "s"} — DockBack creates {creates} of them before the container.</>
          : <>This host is missing {rows.length} bind mount source{rows.length === 1 ? "" : "s"}.</>}
        {manual > 0 && <span className="text-error"> {manual} need{manual === 1 ? "s" : ""} creating by hand first.</span>}
      </p>
      <ul className="mt-1.5 space-y-1">
        {rows.map((r) => {
          const a = BIND_ACTIONS[r.action] || BIND_ACTIONS.manual;
          return (
            <li key={r.source + r.destination} className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
              {/* The action label is allowed to grow; the path is what must wrap
                  rather than push the row wide, so it takes the flexible column. */}
              <span className={`shrink-0 text-[10px] font-semibold uppercase tracking-wide ${a.tone}`}>{a.label}</span>
              <span className="min-w-0 flex-1 basis-full break-all font-mono text-[11px] text-on-surface sm:basis-0">{r.source}</span>
              <span className="shrink-0 font-mono text-[10px] text-outline">→ {r.destination}</span>
              {r.action === "manual" && <span className="basis-full break-words text-[11px] text-error">{r.note}</span>}
            </li>
          );
        })}
      </ul>
    </div>
  );
}
