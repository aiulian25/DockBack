// The visual "it's finished" for a run the operator started: a toast with what
// ran and how it ended, so nobody has to scroll a console to find out.
import type { ToastApi } from "../components/Toast";
import type { RunOutcome } from "./runDone";

// Long enough to be seen after looking away from the screen for a moment.
export const RUN_NOTICE_MS = 8000;

export type RunNotice = { kind: "success" | "error" | "info"; text: string };

/**
 * runEndNotice words a finished run: `what` names it ("Stack “blog” backup"),
 * `done` says what success means ("finished"). A failure carries the server's
 * reason when there is one.
 */
export function runEndNotice(what: string, done: string, outcome: RunOutcome, message = ""): RunNotice {
  const why = message.trim().replace(/\.+$/, "");
  const reason = why ? `: ${why}` : "";
  if (outcome === "ok") return { kind: "success", text: `${what} ${done}.` };
  if (outcome === "canceled") return { kind: "info", text: `${what} was canceled.` };
  if (outcome === "interrupted") return { kind: "error", text: `${what} was interrupted because DockBack stopped${reason}.` };
  return { kind: "error", text: `${what} failed${reason}.` };
}

export function announceRunEnd(toast: ToastApi, notice: RunNotice) {
  toast[notice.kind](notice.text, RUN_NOTICE_MS);
}
