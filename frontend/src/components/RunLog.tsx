// RunLogPanel shows a finished run's persisted log (F8), fetched on demand, with
// a "Download log" link. Used in the Backups detail drawer (collapsible) and on
// the container page per backup row (always-open in an expanded table row).
import { useCallback, useEffect, useState } from "react";
import { ScrollText, ChevronDown, Download, Loader2 } from "lucide-react";
import { api, RunLogLine } from "../api";

// The colour of a log line's severity, shared by every view of a run's output.
export function levelClass(level: string) {
  if (level === "ERR") return "text-error";
  if (level === "WARN") return "text-warning";
  return "text-secondary";
}

export function RunLogPanel({ backupId, collapsible = true }: { backupId: string; collapsible?: boolean }) {
  const [open, setOpen] = useState(!collapsible);
  const [lines, setLines] = useState<RunLogLine[] | null>(null);
  const [loading, setLoading] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    try { const r = await api.runLog(backupId); setLines(r.lines); }
    catch { setLines([]); }
    finally { setLoading(false); }
  }, [backupId]);

  // Non-collapsible (table) use fetches immediately; collapsible fetches on open.
  useEffect(() => { if (!collapsible) load(); }, [collapsible, load]);

  const toggle = () => {
    const next = !open;
    setOpen(next);
    if (next && lines === null) load();
  };

  return (
    <div className="rounded-lg border border-outline-variant">
      <div className="flex items-center gap-2 px-3 py-2">
        {collapsible ? (
          <button onClick={toggle} className="flex flex-1 items-center gap-2 text-sm font-medium text-on-surface hover:text-primary">
            <ScrollText size={15} className="text-primary" /> Run log
            <ChevronDown size={15} className={`transition-transform ${open ? "rotate-180" : ""}`} />
          </button>
        ) : (
          <span className="flex flex-1 items-center gap-2 text-sm font-medium text-on-surface">
            <ScrollText size={15} className="text-primary" /> Run log
          </span>
        )}
        <a
          href={api.runLogDownloadURL(backupId)}
          download
          className="inline-flex items-center gap-1 rounded px-2 py-1 text-xs text-on-surface-variant hover:bg-surface-highest hover:text-on-surface"
          title="Download this run's log as a text file"
        >
          <Download size={13} /> Download log
        </a>
      </div>

      {open && (
        <div className="border-t border-outline-variant/50">
          {loading ? (
            <div className="flex items-center gap-2 px-3 py-4 text-sm text-on-surface-variant">
              <Loader2 size={14} className="animate-spin" /> Loading log…
            </div>
          ) : lines && lines.length > 0 ? (
            <div className="max-h-72 overflow-y-auto bg-surface-lowest p-3 font-mono text-xs leading-relaxed">
              {lines.map((l, i) => (
                <div key={i} className="flex gap-2 py-0.5">
                  <span className="shrink-0 text-on-surface-variant">{new Date(l.ts * 1000).toLocaleTimeString()}</span>
                  <span className={`shrink-0 font-semibold ${levelClass(l.level)}`}>{l.level}</span>
                  <span className="break-all text-on-surface">{l.msg}</span>
                </div>
              ))}
            </div>
          ) : (
            <div className="px-3 py-4 text-sm text-on-surface-variant">No stored log for this run.</div>
          )}
        </div>
      )}
    </div>
  );
}
