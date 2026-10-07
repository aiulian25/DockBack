// Reusable "choose backup destinations" modal for bulk backups (Backup Stack /
// Full Server Backup). The per-container detail page has its own inline picker;
// this gives the same choice to the node-level bulk actions, which previously
// ran straight to the policy/all-enabled destinations with no say. Local is
// always kept — only external destinations are toggled. Nothing is hardcoded.
import { useEffect, useState } from "react";
import { CloudUpload, HardDrive, Database } from "lucide-react";
import { Destination, fmtBytes } from "../api";
import { Modal, Button } from "./ui";

export default function BackupTargetsModal({
  open, title, subtitle, destinations, defaultSelected, confirmLabel = "Start Backup",
  showPause = false, dbNames = [], onConfirm, onClose, children,
}: {
  open: boolean;
  title: string;
  subtitle?: string;
  // children (F221): an extra per-run choice this particular caller needs, shown
  // under the destinations. Optional — every existing caller passes none and is
  // rendered byte-for-byte as before.
  children?: React.ReactNode;
  destinations: Destination[];
  defaultSelected: string[];
  confirmLabel?: string;
  // showPause adds a per-run "Consistency during volume backup" selector applied
  // to the whole run; dbNames are the services detected as databases (dumped live
  // and never paused) — surfaced so the operator knows they're excluded (PLAN §4.2).
  showPause?: boolean;
  dbNames?: string[];
  onConfirm: (selected: string[], pauseMode: string, consistent: boolean, compression: string) => void;
  onClose: () => void;
}) {
  // Only enabled destinations can receive a mirror (the engine skips disabled
  // ones), so they're the only ones worth offering.
  const usable = destinations.filter((d) => d.enabled);
  const [sel, setSel] = useState<Set<string>>(new Set());
  // "" = leave each service to its remembered per-container setting; a value
  // forces that mode for the whole run. Reset to inherit each time it opens.
  const [pause, setPause] = useState("");
  // F79: optional per-run compression override. "" = each service's saved
  // setting (falling back to balanced), exactly like a scheduled run.
  const [compression, setCompression] = useState("");
  // consistent (F33): capture the whole stack as one app-consistent snapshot —
  // the app tier is quiesced while every service's DB dump + volumes are taken
  // together, for a single coherent point-in-time (adds brief downtime). Off by
  // default; only offered for a stack (showPause).
  const [consistent, setConsistent] = useState(false);

  // Seed the selection from the policy default each time the modal opens.
  useEffect(() => {
    if (open) { setSel(new Set(defaultSelected.filter((id) => usable.some((d) => d.id === id)))); setPause(""); setConsistent(false); setCompression(""); }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, defaultSelected.join(",")]);

  const toggle = (id: string, on: boolean) =>
    setSel((prev) => { const n = new Set(prev); on ? n.add(id) : n.delete(id); return n; });

  const summary = ["Local", ...usable.filter((d) => sel.has(d.id)).map((d) => d.name)].join(", ");

  return (
    <Modal
      open={open}
      onClose={onClose}
      title={title}
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" onClick={() => onConfirm(Array.from(sel), pause, consistent, compression)}>
            <CloudUpload size={16} /> {confirmLabel}
          </Button>
        </>
      }
    >
      {subtitle && <p className="mb-3 text-sm text-on-surface-variant">{subtitle}</p>}

      {/* Local is always written first — shown fixed, not toggleable. */}
      <div className="mb-2 flex items-center gap-3 rounded border border-outline-variant bg-surface-low px-3 py-2">
        <HardDrive size={16} className="text-on-surface-variant" />
        <span className="text-sm font-medium">Local</span>
        <span className="ml-auto text-xs text-on-surface-variant">always kept</span>
      </div>

      {usable.length === 0 ? (
        <p className="rounded border border-outline-variant bg-surface-low px-3 py-2 text-sm text-on-surface-variant">
          No external destinations yet — add Synology / Nextcloud / S3 in Settings to send an offsite copy.
        </p>
      ) : (
        <div className="space-y-1.5">
          {usable.map((d) => (
            <label key={d.id} className="flex cursor-pointer items-center gap-3 rounded border border-outline-variant bg-surface-low px-3 py-2 hover:bg-surface-highest">
              <input
                type="checkbox"
                checked={sel.has(d.id)}
                onChange={(e) => toggle(d.id, e.target.checked)}
              />
              <span className={`h-2 w-2 shrink-0 rounded-full ${d.reachable ? "bg-success" : "bg-error status-pulse"}`} title={d.reachable ? "Reachable" : "Unreachable"} />
              <span className="text-sm font-medium">{d.name}</span>
              <span className="text-xs uppercase tracking-wide text-on-surface-variant">{d.type}</span>
              {d.total_bytes > 0 && (
                <span className="ml-auto tnum text-xs text-on-surface-variant">{fmtBytes(d.free_bytes)} free</span>
              )}
            </label>
          ))}
        </div>
      )}

      {children && (
        <div className="mt-4 border-t border-outline-variant/50 pt-3">{children}</div>
      )}

      {showPause && (
        <div className="mt-4 border-t border-outline-variant/50 pt-3">
          {/* F33: opt into one coordinated, app-consistent snapshot of the whole stack. */}
          <label className="mb-2 flex cursor-pointer items-start gap-3 rounded border border-outline-variant bg-surface-low px-3 py-2 text-sm">
            <input
              type="checkbox"
              className="mt-0.5"
              checked={consistent}
              onChange={(e) => setConsistent(e.target.checked)}
            />
            <span className="min-w-0">
              <span className="font-medium">App-consistent snapshot (quiesce during capture)</span>
              <span className="mt-0.5 block text-xs text-on-surface-variant">
                Pauses the app while its database and volumes are captured together, for a single coherent
                point-in-time. Adds brief downtime. Off = each service is backed up independently and concurrently.
              </span>
            </span>
          </label>
          {/* F79: per-run compression override. Default = each service's own
              remembered choice, so a plain stack run matches a scheduled one. */}
          <div className="mb-2 flex items-center gap-3 rounded border border-outline-variant bg-surface-low px-3 py-2 text-sm">
            <span>Compression</span>
            <select
              value={compression}
              onChange={(e) => setCompression(e.target.value)}
              className="ml-auto rounded border border-outline-variant bg-surface-lowest px-2 py-1 text-xs focus:outline-none focus:ring-1 focus:ring-primary"
            >
              <option value="">Each service's saved setting</option>
              <option value="fast">Fast (zstd)</option>
              <option value="balanced">Balanced (zstd)</option>
              <option value="max">Max (zstd)</option>
              <option value="max-long">Max + long-range (zstd)</option>
              <option value="gzip">gzip (universal compatibility)</option>
              <option value="xz">xz (maximum ratio, slow)</option>
            </select>
          </div>
          <div className="flex items-center gap-3 rounded border border-outline-variant bg-surface-low px-3 py-2 text-sm">
            <span>{consistent ? "Quiesce mode for the app tier" : "Consistency during volume backup"}</span>
            <select
              value={pause}
              onChange={(e) => setPause(e.target.value)}
              className="ml-auto rounded border border-outline-variant bg-surface-lowest px-2 py-1 text-xs focus:outline-none focus:ring-1 focus:ring-primary"
            >
              <option value="">Each container's setting</option>
              <option value="none">Live copy (no pause)</option>
              <option value="pause">Pause during copy (default, recommended)</option>
              <option value="stop">Stop during copy (full quiesce)</option>
            </select>
          </div>
          <p className="mt-1 text-xs italic text-on-surface-variant">
            {pause === "stop"
              ? "Each app service's volumes are copied while it runs; it is stopped only to copy again what changed meanwhile, then restarted — short downtime, maximum consistency."
              : pause === "pause"
                ? "Each app service's volumes are copied while it runs; it is frozen only to copy again what changed meanwhile — seconds, consistent snapshot."
                : pause === "none"
                  ? "Files are copied live — fast, but a busy app may produce an inconsistent snapshot."
                  : "Applies each service's own remembered pause setting (default). Pick a mode above to force it for this run."}
          </p>
          {dbNames.length > 0 && (
            <div className="mt-2 flex items-start gap-2 rounded border border-secondary/30 bg-secondary/10 px-3 py-2 text-xs text-on-surface-variant">
              <Database size={14} className="mt-0.5 shrink-0 text-secondary" />
              <span>
                {dbNames.length === 1 ? <><span className="font-medium text-on-surface">{dbNames[0]}</span> is a database</> : <><span className="font-medium text-on-surface">{dbNames.join(", ")}</span> are databases</>}
                {" "}— dumped live with native tools (no downtime); never paused or stopped, so they must be running.
              </span>
            </div>
          )}
        </div>
      )}

      <p className="mt-3 text-xs text-on-surface-variant">
        Backing up to: <span className="font-medium text-on-surface">{summary}</span>
      </p>
    </Modal>
  );
}
