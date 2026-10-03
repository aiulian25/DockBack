// KeyEscrowBanner — the persistent "back up your encryption key" reminder
// (PLAN §9.2). Shows until the admin acknowledges saving the key; an ephemeral
// key (no persistent key set) shows a hard, non-dismissible warning because the
// situation is genuinely unsafe (the fix is configuring DOCKBACK_ENCRYPTION_KEY).
import { useEffect, useState } from "react";
import { ShieldAlert, AlertTriangle, X } from "lucide-react";
import { api } from "../api";
import KeyEscrowModal from "./KeyEscrowModal";

export default function KeyEscrowBanner() {
  const [status, setStatus] = useState<{ acknowledged: boolean; ephemeral: boolean } | null>(null);
  const [open, setOpen] = useState(false);

  const load = () => api.keyStatus().then((s) => setStatus({ acknowledged: s.acknowledged, ephemeral: s.ephemeral })).catch(() => {});
  useEffect(() => { load(); }, []);

  if (!status) return null;
  // Ephemeral keys always warn (can't be acknowledged away); a real key warns
  // only until the admin confirms they saved it.
  if (!status.ephemeral && status.acknowledged) return null;

  const urgent = status.ephemeral;
  return (
    <>
      <div className={`mb-5 flex items-center gap-3 rounded-lg border p-3 ${urgent ? "border-error/30 bg-error/10 text-error" : "border-warning/30 bg-warning/10 text-warning"}`}>
        {urgent ? <ShieldAlert size={18} className="shrink-0" /> : <AlertTriangle size={18} className="shrink-0" />}
        <div className="min-w-0 flex-1 text-sm">
          {urgent ? (
            <span><strong>No persistent encryption key set.</strong> Backups made now are <strong>unrecoverable after a restart</strong> — set <code className="font-mono">DOCKBACK_ENCRYPTION_KEY</code> and restart.</span>
          ) : (
            <span><strong>Back up your encryption key.</strong> Without it, your backups can never be restored — there is no reset.</span>
          )}
        </div>
        <button onClick={() => setOpen(true)} className="shrink-0 rounded border border-current/30 px-3 py-1 text-xs font-semibold hover:bg-current/10">
          {urgent ? "Show key & details" : "Back up now"}
        </button>
      </div>
      <KeyEscrowModal open={open} onClose={() => setOpen(false)} onAck={load} />
    </>
  );
}
