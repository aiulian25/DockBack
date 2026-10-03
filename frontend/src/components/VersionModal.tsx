// Version / releases modal. DockBack does NOT check for updates automatically or
// send any telemetry (privacy posture — see the egress doc), so instead of a
// data-driven "update available" badge this points the operator to the GitHub
// releases page, where new features, bug fixes and UI changes are listed.
import { Github, ExternalLink, ShieldCheck } from "lucide-react";
import { Modal, Button } from "./ui";

// Public repository. Constant config (not user data), like the external doc links.
const REPO_URL = "https://github.com/aiulian25/DockBack";
const RELEASES_URL = `${REPO_URL}/releases`;

// Open an external page safely in a new tab (no window.opener handle back to us).
function openExternal(url: string) {
  window.open(url, "_blank", "noopener,noreferrer");
}

export default function VersionModal({ open, version, onClose }: { open: boolean; version: string; onClose: () => void }) {
  const shown = version || "unknown";
  return (
    <Modal
      open={open}
      onClose={onClose}
      title="DockBack version"
      footer={
        <>
          <Button onClick={onClose}>Close</Button>
          <Button variant="primary" onClick={() => openExternal(RELEASES_URL)}>
            <Github size={16} /> View releases on GitHub
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        <div className="flex items-center gap-3 rounded-lg border border-outline-variant bg-surface-low px-4 py-3">
          <img src="/dockback-mark.png" alt="DockBack" className="h-10 w-10 shrink-0 rounded-md object-contain" />
          <div className="min-w-0">
            <div className="text-[11px] font-medium uppercase tracking-wider text-on-surface-variant">You're running</div>
            <div className="tnum text-lg font-bold text-on-surface">DockBack {shown}</div>
          </div>
        </div>

        <p className="text-sm text-on-surface-variant">
          See the latest release — new features, bug fixes and UI changes — on the
          project's GitHub releases page, and compare it with the version you're running above.
        </p>

        <button
          onClick={() => openExternal(RELEASES_URL)}
          className="flex w-full items-center gap-2 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-left text-sm transition-colors hover:border-docker-blue/50 hover:bg-surface-high/40"
        >
          <Github size={16} className="shrink-0 text-on-surface-variant" />
          <span className="min-w-0 flex-1 truncate font-mono text-xs text-primary">{RELEASES_URL}</span>
          <ExternalLink size={14} className="shrink-0 text-on-surface-variant" />
        </button>

        <div className="flex items-start gap-2 rounded border border-outline-variant bg-surface-high/50 px-3 py-2 text-xs text-on-surface-variant">
          <ShieldCheck size={14} className="mt-0.5 shrink-0 text-success" />
          <span>
            DockBack never checks for updates automatically and sends no telemetry. This page
            makes no network calls — the link above opens GitHub in a new tab only when you click it.
          </span>
        </div>
      </div>
    </Modal>
  );
}
