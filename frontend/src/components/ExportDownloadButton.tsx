// ExportDownloadButton — re-authenticate, then download (F199).
//
// A decrypted export is the most sensitive thing this app hands out, and it used
// to be a plain <a href>: one click on an open session and every secret in a
// backup was on disk. It is now gated by the same step-up the master key needs.
//
// The mechanics exist because a download is a browser NAVIGATION — a GET with no
// body, so there is nowhere to put a password. The button asks the server for a
// one-shot ticket first (a POST, which CAN carry credentials), and only then
// navigates. A recent step-up satisfies the grant without prompting, so an
// operator pulling five files in a row is asked once, not five times.
import { useState } from "react";
import { Loader2 } from "lucide-react";
import { api, StepUpError } from "../api";
import StepUpPrompt from "./StepUpPrompt";
import { Button } from "./ui";

export default function ExportDownloadButton({
  backupId = "", purpose = "download", request, path, label, title, variant = "secondary", icon, iconOnly = false, className,
}: {
  backupId?: string;
  purpose?: "download" | "extract";
  // Any other guarded export (a whole stack, a node's evidence): obtain the
  // one-shot ticket with these credentials and return the URL to open.
  request?: (password?: string, code?: string) => Promise<string>;
  path?: string; // required for purpose="extract"
  label?: string;
  title?: string;
  variant?: "primary" | "secondary" | "danger";
  icon?: React.ReactNode;
  iconOnly?: boolean;
  className?: string;
}) {
  const [busy, setBusy] = useState(false);
  const [stepUp, setStepUp] = useState<{ totp: boolean; err: string } | null>(null);
  const [err, setErr] = useState("");

  const go = async (password?: string, code?: string) => {
    setBusy(true); setErr("");
    try {
      const url = request ? await request(password, code) : await backupExportURL(backupId, purpose, path || "", password, code);
      setStepUp(null);
      // Navigate rather than fetch: the browser's own download manager handles a
      // multi-gigabyte archive without holding it in memory.
      window.location.href = url;
    } catch (e) {
      if (e instanceof StepUpError) {
        // First click with no password is the EXPECTED path, not an error — only
        // show a message once they have actually tried one.
        setStepUp({ totp: e.totp_required, err: password ? e.message : "" });
      } else {
        setErr((e as Error).message);
      }
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      {iconOnly ? (
        <button type="button" title={title} onClick={() => go()} disabled={busy}
          className={className ?? "shrink-0 rounded p-1 text-on-surface-variant hover:bg-primary/10 hover:text-primary disabled:opacity-50"}>
          {busy ? <Loader2 size={15} className="animate-spin" /> : icon}
        </button>
      ) : (
        <Button variant={variant} disabled={busy} onClick={() => go()} title={title}>
          {busy ? <Loader2 size={16} className="animate-spin" /> : icon} {label}
        </Button>
      )}
      {/* The prompt sits BELOW the control that opened it and stays in the same
          flow, so the file being exported is still on screen while the password
          is typed. min-w-0 + break-words keep a long path from forcing the
          surrounding row wider. */}
      {stepUp && (
        <div className="mt-2 min-w-0 break-words">
          <StepUpPrompt
            totp={stepUp.totp}
            busy={busy}
            error={stepUp.err}
            confirmLabel="Confirm and download"
            onConfirm={(p, c) => go(p, c)}
          />
        </div>
      )}
      {err && <p className="mt-1 text-xs text-error">{err}</p>}
    </>
  );
}

// backupExportURL is the one-shot URL for a backup's archive, or one file of it.
async function backupExportURL(backupId: string, purpose: "download" | "extract", path: string, password?: string, code?: string): Promise<string> {
  const { ticket } = await api.exportGrant(backupId, purpose, password, code);
  return purpose === "extract" ? api.backupExtractURL(backupId, path, ticket) : api.downloadURL(backupId, ticket);
}
