// DeletionIncomplete: what is left of a backup whose delete could not remove every
// copy. DockBack finishes the delete by itself; this says what it is waiting for,
// and lets the owner try now or stop tracking the leftovers.
import { useEffect, useState } from "react";
import { Loader2, RefreshCw } from "lucide-react";
import { api, Backup } from "../api";
import { keptCopiesNotice, keptCopyReason } from "../lib/backupDeletion";
import { locations } from "../lib/backupRows";
import { Button, Modal } from "./ui";
import { useToast } from "./Toast";

type Pending = "" | "retry" | "forget";

export default function DeletionIncomplete({ backup, nodeName, onClose, onChanged }: {
  backup: Backup;
  nodeName: string;
  onClose: () => void;
  onChanged: () => void;
}) {
  const toast = useToast();
  // List rows are slim: the reason each copy is still stored is on the full row.
  const [full, setFull] = useState<Backup | null>(null);
  const [pending, setPending] = useState<Pending>("");
  const [confirmingForget, setConfirmingForget] = useState(false);

  useEffect(() => {
    let gone = false;
    api.backup(backup.id).then((row) => { if (!gone) setFull(row); }).catch(() => {});
    return () => { gone = true; };
  }, [backup.id]);

  const copies = full ? locations(full) : [];
  const busy = pending !== "";

  const finish = (message: string) => {
    toast.success(message);
    onChanged();
    onClose();
  };

  const retry = async () => {
    setPending("retry");
    try {
      const result = await api.deleteBackup(backup.id);
      if (result.status === "deleted") { finish("Backup deleted."); return; }
      toast.info(keptCopiesNotice(result.kept || []));
      setFull(await api.backup(backup.id));
    } catch (error) {
      toast.error(`Couldn't finish the delete: ${(error as Error).message}`);
    } finally {
      setPending("");
    }
  };

  const forget = async () => {
    setPending("forget");
    try {
      await api.forgetBackup(backup.id);
      finish("No longer tracked. The remaining copies were left where they are.");
    } catch (error) {
      toast.error(`Couldn't stop tracking: ${(error as Error).message}`);
    } finally {
      setPending("");
    }
  };

  const footer = confirmingForget ? (
    <>
      <Button onClick={() => setConfirmingForget(false)} disabled={busy}>Keep tracking</Button>
      <Button variant="danger" onClick={forget} disabled={busy}>
        {pending === "forget" && <Loader2 size={15} className="animate-spin" />} Stop tracking
      </Button>
    </>
  ) : (
    <>
      <Button variant="ghost" onClick={() => setConfirmingForget(true)} disabled={busy}>Stop tracking…</Button>
      <Button onClick={onClose} disabled={busy}>Close</Button>
      <Button variant="primary" onClick={retry} disabled={busy}>
        {pending === "retry" ? <Loader2 size={15} className="animate-spin" /> : <RefreshCw size={15} />} Try again now
      </Button>
    </>
  );

  return (
    <Modal open onClose={onClose} title="Deletion incomplete" footer={footer} dismissible={!busy}>
      <p className="text-sm text-on-surface">
        You deleted this backup of <span className="font-semibold">{backup.target_name}</span> on {nodeName}. Everything that could be removed is gone.
      </p>
      <p className="mt-3 text-sm font-medium text-on-surface">Still stored:</p>
      {!full && <p className="mt-1 text-sm text-on-surface-variant">Loading…</p>}
      <ul className="mt-1 flex flex-col gap-2 text-sm">
        {copies.map((copy) => (
          <li key={`${copy.kind}:${copy.dest_id || copy.name}`} className="rounded border border-outline-variant/60 bg-surface-lowest px-3 py-2">
            <span className="font-semibold text-on-surface">{copy.name}</span>
            <span className="block text-on-surface-variant">{keptCopyReason(copy)}</span>
          </li>
        ))}
      </ul>
      {confirmingForget ? (
        <p role="alert" className="mt-3 rounded bg-warning/10 px-3 py-2 text-sm text-on-surface">
          DockBack will forget this backup. The copies above stay where they are, and you would have to delete them by hand.
        </p>
      ) : (
        <p className="mt-3 text-sm text-on-surface-variant">
          DockBack finishes the delete by itself. This backup can no longer be restored.
        </p>
      )}
    </Modal>
  );
}
