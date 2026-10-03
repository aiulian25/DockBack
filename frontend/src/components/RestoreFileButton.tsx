// F96: put ONE recovered file back into the running container.
//
// Browse (F21) and cross-backup search (F70) can find any version of any file in
// seconds — and then the recovery ended in the Downloads folder, with a manual
// `docker cp` and a guess at ownership. This is the missing last step, sitting
// directly beside the download it replaces.
import { useState } from "react";
import { Loader2, Upload } from "lucide-react";
import { api } from "../api";
import { useToast } from "./Toast";

export default function RestoreFileButton({
  backupId, path, container, nodeId, targetId, source, onDone,
}: {
  backupId: string;
  path: string;
  container: string;   // container name, for the confirmation text
  nodeId?: string;
  targetId: string;    // container id or name to write into
  source?: string;     // which stored copy to read from (matches the drawer's choice)
  onDone?: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const toast = useToast();

  const run = async () => {
    if (busy) return;
    // Ask ONCE, and say exactly what will happen to the file that is there now —
    // this overwrites live application data, so the wording has to be concrete
    // rather than a generic "are you sure".
    const keep = window.confirm(
      `Write this file back into ${container}?\n\n` +
      `  /${path.replace(/^\/+/, "")}\n\n` +
      `The file currently at that path will be overwritten.\n\n` +
      `OK — keep a copy of the current file alongside it (.dockback-<time>.bak)\n` +
      `Cancel — stop, change nothing`,
    );
    if (!keep) return;

    setBusy(true);
    try {
      await api.restoreFile(backupId, { path, target_id: targetId, node_id: nodeId, source, keep_backup: true });
      toast.success(`Restored ${path} into ${container}`);
      onDone?.();
    } catch (e) {
      toast.error(`Couldn't restore ${path}: ${(e as Error).message}`);
    } finally {
      setBusy(false);
    }
  };

  return (
    <button
      onClick={run}
      disabled={busy}
      title={`Write back into the container — restores this file to /${path.replace(/^\/+/, "")} in ${container}, keeping a copy of the current one`}
      aria-label="Restore this file"
      className="shrink-0 rounded p-1 text-on-surface-variant hover:bg-warning/10 hover:text-warning disabled:opacity-50"
    >
      {busy ? <Loader2 size={14} className="animate-spin" /> : <Upload size={14} />}
    </button>
  );
}
