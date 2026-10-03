// F223 — "Recovering an existing fleet?": the empty state as a runbook.
//
// Every part of a total-loss recovery already existed, on a different screen —
// restore the app-backup, re-adopt the destinations so the catalog points at
// real archives, rebuild each dead node from that catalog. What did not exist
// was the ORDER, and a fresh install after a fire showed an ordinary empty
// dashboard that assumed you knew it.
//
// This component is guidance and a remembered position. It starts nothing and
// deletes nothing: every step is a link to the screen that already does the
// work. A recovery wizard that reimplements recovery would be a second
// implementation of the most dangerous code in the product.
import { Link } from "react-router-dom";
import { LifeBuoy, DatabaseBackup, HardDrive, ServerCog, Check, X, ArrowRight } from "lucide-react";
import { RecoveryState } from "../api";

// The three steps, in the order a recovery actually happens. Each names the
// screen it hands off to, because "where do I do that" is the question the old
// empty dashboard left unanswered.
const STEPS = [
  {
    key: "app-restored" as const,
    icon: DatabaseBackup,
    title: "Restore DockBack's own backup",
    body: "Brings back every node and its credentials, the whole backup catalog, your settings, destinations and admin account. Upload the archive you kept off-box, or point DockBack at the destination it was pushed to. The app restarts into the restored state.",
    to: "/settings?tab=advanced#app-backup",
    cta: "Open Application Backup",
  },
  {
    key: "dests-verified" as const,
    icon: HardDrive,
    title: "Check the destinations still resolve",
    body: "The restored catalog describes archives that live on your destinations. Confirm each one still connects and holds what the catalog expects — a destination that moved or rotated its credentials is the difference between a catalog and a recovery.",
    to: "/settings?tab=destinations",
    cta: "Open Destinations",
  },
  {
    key: "done" as const,
    icon: ServerCog,
    title: "Rebuild each machine you lost",
    body: "Recovery lists every node in the restored catalog with the order to bring its services back — databases first. A node whose hardware is gone can be rebuilt onto a different host from the same backups.",
    to: "/recovery",
    cta: "Open Recovery",
  },
];

export default function RecoveryWizard({ state, onStep }: {
  state: RecoveryState;
  onStep: (step: RecoveryState["step"]) => void;
}) {
  const step = state.step;
  if (step === "dismissed" || step === "done") return null;
  // The full card is for an install that holds nothing. After the app-restore
  // the catalog is populated — so it is NOT fresh any more — and what is wanted
  // is the resume banner picking up where the restart interrupted.
  const resuming = step === "app-restored" || step === "dests-verified";
  if (!state.fresh_install && !resuming) return null;

  // Which step is live: the first one not yet marked complete.
  const doneUpTo = step === "dests-verified" ? 2 : step === "app-restored" ? 1 : 0;

  return (
    <div className="mb-5 overflow-hidden rounded-[14px] border border-primary/30 bg-primary/[0.06]">
      <div className="flex flex-wrap items-start gap-3 border-b border-primary/20 px-4 py-3">
        <LifeBuoy size={20} className="mt-0.5 shrink-0 text-primary" />
        <div className="min-w-0 flex-1">
          <h2 className="text-[15px] font-bold tracking-tight text-on-surface">
            {resuming ? "Recovery in progress" : "Recovering an existing fleet?"}
          </h2>
          <p className="mt-0.5 max-w-3xl break-words text-sm text-on-surface-variant">
            {resuming
              ? "DockBack has restarted into the restored state. Two things are left before your backups are usable again."
              : "This instance is empty. If you are rebuilding after a loss, do these three things in this order — each one opens the screen that does the work."}
          </p>
        </div>
        <button
          onClick={() => onStep("dismissed")}
          title="Hide this — a normal new install does not need it"
          className="shrink-0 rounded-md p-1.5 text-on-surface-variant transition-colors hover:bg-surface-high hover:text-on-surface"
          aria-label="Dismiss the recovery guide"
        >
          <X size={16} />
        </button>
      </div>

      <ol className="divide-y divide-primary/15">
        {STEPS.map((s, i) => {
          const complete = i < doneUpTo;
          const current = i === doneUpTo;
          const Icon = s.icon;
          return (
            <li key={s.key} className={`flex flex-wrap items-start gap-3 px-4 py-3 ${complete ? "opacity-60" : ""}`}>
              {/* The number carries the order, which is the whole point of the
                  card — these steps are not interchangeable. */}
              <span
                className={`mt-0.5 flex h-6 w-6 shrink-0 items-center justify-center rounded-full text-[11px] font-bold tnum ${
                  complete ? "bg-success/15 text-success" : current ? "bg-primary text-white" : "bg-surface-high text-on-surface-variant"
                }`}
                aria-hidden="true"
              >
                {complete ? <Check size={13} /> : i + 1}
              </span>
              <div className="min-w-0 flex-1">
                <div className="flex flex-wrap items-center gap-2">
                  <Icon size={15} className="shrink-0 text-on-surface-variant" />
                  <span className="min-w-0 break-words text-sm font-semibold text-on-surface">{s.title}</span>
                  {complete && <span className="shrink-0 text-[11px] font-semibold uppercase tracking-wide text-success">done</span>}
                </div>
                <p className="mt-1 max-w-3xl break-words text-xs text-on-surface-variant">{s.body}</p>
                {i === 0 && !state.has_app_destinations && !complete && (
                  <p className="mt-1 max-w-3xl break-words text-xs text-on-surface-variant">
                    No destination is configured here yet &mdash; after a total loss there usually is not one. Add it on that screen with the same credentials, then fetch the newest archive from it.
                  </p>
                )}
                <div className="mt-2 flex flex-wrap items-center gap-2">
                  <Link
                    to={s.to}
                    className="inline-flex items-center gap-1.5 rounded border border-outline-variant bg-surface-high/60 px-2.5 py-1 text-xs font-medium text-on-surface hover:bg-surface-high"
                  >
                    {s.cta} <ArrowRight size={13} className="shrink-0" />
                  </Link>
                  {/* Every step can be marked by hand, step 1 included: it is
                      normally set by the restore itself, but somebody who
                      restored before this existed — or who is only rebuilding
                      nodes — must not be stuck on a step they already did. */}
                  {current && (
                    <button
                      onClick={() => onStep(s.key)}
                      className="inline-flex items-center gap-1.5 rounded px-2 py-1 text-xs font-medium text-on-surface-variant hover:text-on-surface"
                    >
                      <Check size={13} className="shrink-0" /> {i === STEPS.length - 1 ? "Finish" : "Mark done"}
                    </button>
                  )}
                </div>
              </div>
            </li>
          );
        })}
      </ol>

      <p className="border-t border-primary/20 px-4 py-2.5 text-xs text-on-surface-variant">
        Nothing here runs on its own &mdash; every step opens an existing screen and waits for you.
      </p>
    </div>
  );
}
