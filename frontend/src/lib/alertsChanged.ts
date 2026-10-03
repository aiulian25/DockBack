// Telling the header's alert bell that the alert count moved (F229).
//
// The bell lives in Layout; alerts are acknowledged on the Logs page. They are
// unrelated components with no shared ancestor holding this state, so the bell
// only knew the count had changed the next time something made it re-read:
// its 30-second poll, the tab regaining focus, or a route change. That last one
// is why acknowledging everything appeared to do nothing until you navigated
// away — the badge sat there showing 11 alerts that were already acknowledged.
//
// A window event rather than a context: one number, read by one component,
// written from two call sites. Lifting it into a provider would be more
// machinery than the problem, and a provider still would not cover a future
// third caller without being threaded through it. Anything that changes the
// acknowledged state fires this and the bell re-reads — from the server, so the
// badge is never a local guess about what the count now is.

const ALERTS_CHANGED = "dback:alerts-changed";

// notifyAlertsChanged announces that the unacknowledged-alert count may have
// moved. Safe to call after a failed acknowledge too: the listener re-reads the
// authoritative count rather than adjusting a local one, so a no-op costs one
// cheap COUNT query and never leaves the badge wrong.
export function notifyAlertsChanged() {
  window.dispatchEvent(new Event(ALERTS_CHANGED));
}

// onAlertsChanged subscribes, returning its own unsubscribe for effect cleanup.
export function onAlertsChanged(fn: () => void): () => void {
  window.addEventListener(ALERTS_CHANGED, fn);
  return () => window.removeEventListener(ALERTS_CHANGED, fn);
}
