// Per-node "unprotected containers" banner (Fable-UI-UX B2, moved to the node
// detail page). Lists this node's running containers that have never been backed
// up, and, separately, those whose newest backup is stale (older than twice their
// schedule's interval, or 8 days with none), each with a one-click Protect (B5).
// F13 adds a quieter secondary section: STOPPED containers that still hold a named
// data volume and have no backup — genuinely at risk, but surfaced without nagging
// (they never count toward the primary "unprotected running" number).
// Collapsed by default; expands to the actionable list.
import { useState } from "react";
import { ShieldAlert, ShieldCheck, ChevronDown, Box as BoxIcon, Loader2, EyeOff } from "lucide-react";
import { CoverageContainer, fmtAgo } from "../api";

export default function UnprotectedBanner({ nodeName, unprotected, stale = [], stoppedAtRisk = [], onProtect, onIgnore, onProtectAll, protectingAll, onOpen }: {
  nodeName: string;
  unprotected: CoverageContainer[];
  stale?: CoverageContainer[];
  stoppedAtRisk?: CoverageContainer[];
  onProtect: (cid: string) => Promise<void>;
  // Leave a container out of these warnings, and of whole-server backups, for
  // good — one whose data comes back on its own (downloaded models, caches).
  onIgnore?: (c: CoverageContainer) => Promise<void>;
  // F220: protect everything listed in one action, grouped so a compose project
  // is protected ONCE as a stack rather than service by service.
  onProtectAll?: () => Promise<void>;
  protectingAll?: boolean;
  onOpen?: (cid: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState<Set<string>>(new Set());

  const n = unprotected.length;
  const k = stale.length;
  const m = stoppedAtRisk.length;
  if (n === 0 && k === 0 && m === 0) return null;
  const neutral = n === 0 && k === 0; // only stopped-at-risk → quiet, non-amber styling
  const waiting = unprotected.filter((c) => c.scheduled).length;
  // F220: how many ACTIONS "Protect all" is, not how many containers — a stack
  // counts once, because that is how it will be protected. Promising six and
  // performing one would read as a failure when it is the better outcome.
  const stacks = new Set(unprotected.filter((c) => c.stack).map((c) => c.stack));
  const units = unprotected.filter((c) => !c.stack).length + stacks.size;

  const withBusy = async (cid: string, action: () => Promise<void>) => {
    setBusy((b) => new Set(b).add(cid));
    try { await action(); }
    finally { setBusy((b) => { const s = new Set(b); s.delete(cid); return s; }); }
  };

  const Row = (c: CoverageContainer) => {
    const isBusy = busy.has(c.container_id);
    return (
      <div key={c.container_id} className="flex items-center gap-2.5 rounded-lg bg-surface-container px-3 py-2">
        <BoxIcon size={15} className="shrink-0 text-on-surface-variant" />
        <button onClick={() => onOpen?.(c.container_id)} className="min-w-0 flex-1 text-left" title="Open container">
          <span className="block truncate text-sm text-on-surface hover:text-primary">{c.name}</span>
          <span className="block truncate text-xs text-on-surface-variant">
            {c.image}{c.stack ? ` · ${c.stack}` : ""}
            {c.last_backup_at ? ` · last backed up ${fmtAgo(c.last_backup_at)}` : ""}
            {c.scheduled ? " · scheduled" : ""}
          </span>
        </button>
        {onIgnore && (
          <button
            onClick={() => withBusy(c.container_id, () => onIgnore(c))}
            disabled={isBusy}
            className="flex shrink-0 items-center gap-1.5 rounded-md px-2 py-1 text-xs font-medium text-on-surface-variant hover:bg-surface-high hover:text-on-surface disabled:opacity-60"
            title={`Stop warning about ${c.name} and leave it out of whole-server backups`}
          >
            <EyeOff size={13} /> Ignore
          </button>
        )}
        <button
          onClick={() => withBusy(c.container_id, () => onProtect(c.container_id))}
          disabled={isBusy}
          className="flex shrink-0 items-center gap-1.5 rounded-md border border-warning/40 bg-warning/10 px-2.5 py-1 text-xs font-semibold text-warning hover:bg-warning/20 disabled:opacity-60"
          title={`Protect ${c.name} with smart defaults`}
        >
          {isBusy ? <Loader2 size={13} className="animate-spin" /> : <ShieldCheck size={13} />} Protect
        </button>
      </div>
    );
  };

  return (
    <div className={`mb-4 overflow-hidden rounded-[12px] border ${neutral ? "border-outline-variant bg-surface-container/40" : "border-warning/30 bg-warning/[0.07]"}`}>
      <button onClick={() => setOpen((o) => !o)} className="flex w-full items-center gap-3 px-4 py-3 text-left" aria-expanded={open}>
        <ShieldAlert size={18} className={`shrink-0 ${neutral ? "text-on-surface-variant" : "text-warning"}`} />
        <div className="min-w-0 flex-1">
          {n > 0 && (
            <>
              <div className="text-sm font-semibold text-on-surface">{n} container{n === 1 ? "" : "s"} never backed up</div>
              <div className="text-xs text-on-surface-variant">
                Running on {nodeName} with no successful backup yet{waiting > 0 ? ` — ${waiting} of them scheduled, waiting for a first run` : ""}.
              </div>
            </>
          )}
          {k > 0 && (
            <div className={n > 0 ? "mt-1 text-xs text-on-surface-variant" : ""}>
              {n > 0
                ? <>Plus {k} with a stale backup, older than its schedule allows.</>
                : <>
                    <div className="text-sm font-semibold text-on-surface">{k} container{k === 1 ? "" : "s"} with a stale backup</div>
                    <div className="text-xs text-on-surface-variant">Running on {nodeName}, last backed up longer ago than twice their schedule&apos;s interval (8 days with no schedule).</div>
                  </>}
            </div>
          )}
          {m > 0 && (
            neutral ? (
              <>
                <div className="text-sm font-semibold text-on-surface">{m} stopped container{m === 1 ? "" : "s"} with data and no backup</div>
                <div className="text-xs text-on-surface-variant">Stopped on {nodeName} but holding data volumes — protect them so an occasional or crashed app isn't lost.</div>
              </>
            ) : (
              <div className="mt-1 text-xs text-on-surface-variant">Plus {m} stopped container{m === 1 ? "" : "s"} with data and no backup.</div>
            )
          )}
        </div>
        <ChevronDown size={16} className={`shrink-0 text-on-surface-variant transition-transform ${open ? "rotate-180" : ""}`} />
      </button>
      {/* F220: the bulk fix, beside the list it fixes. Only for the RUNNING
          unprotected set — a stopped container is usually off on purpose, so
          sweeping it into a bulk action would protect things nobody asked about. */}
      {n > 0 && onProtectAll && (
        <div className="flex flex-wrap items-center gap-2 border-t border-warning/20 px-4 py-2.5">
          <button
            onClick={() => { void onProtectAll(); }}
            disabled={protectingAll}
            className="inline-flex shrink-0 items-center gap-1.5 rounded-md border border-warning/40 bg-warning/10 px-2.5 py-1 text-xs font-semibold text-warning hover:bg-warning/20 disabled:opacity-60"
            title="Add every unprotected container here to the automatic schedule and back each one up now"
          >
            {protectingAll ? <Loader2 size={13} className="animate-spin" /> : <ShieldCheck size={13} />}
            Protect all ({units})
          </button>
          <span className="min-w-0 break-words text-xs text-on-surface-variant">
            {stacks.size > 0
              ? <>Each compose project is protected once, as one app-consistent stack &mdash; not service by service.</>
              : <>Smart defaults for each, plus a first backup.</>}
          </span>
        </div>
      )}
      {open && (
        <div className={`border-t px-2 py-2 ${neutral ? "border-outline-variant/60" : "border-warning/20"}`}>
          {n > 0 && <div className="space-y-1">{unprotected.map(Row)}</div>}
          {k > 0 && (
            <>
              {n > 0 && <div className="px-2 pb-1 pt-2 text-[11px] font-semibold uppercase tracking-wide text-on-surface-variant">Stale backup</div>}
              <div className="space-y-1">{stale.map(Row)}</div>
            </>
          )}
          {m > 0 && (
            <>
              {n > 0 && <div className="px-2 pb-1 pt-2 text-[11px] font-semibold uppercase tracking-wide text-on-surface-variant">Stopped, with data</div>}
              <div className="space-y-1">{stoppedAtRisk.map(Row)}</div>
            </>
          )}
        </div>
      )}
    </div>
  );
}
