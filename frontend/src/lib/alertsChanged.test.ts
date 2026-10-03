import { expect, it, vi } from "vitest";
import { notifyAlertsChanged, onAlertsChanged } from "./alertsChanged";

// The alert bell (Layout) and the acknowledge action (Logs) share no ancestor;
// this window-event bridge is what makes an ack update the badge without a
// navigation. If it stops firing, the badge silently shows a stale count (F229).

it("delivers each notify to a subscriber and stops after unsubscribe", () => {
  const fn = vi.fn();
  const off = onAlertsChanged(fn);

  notifyAlertsChanged();
  notifyAlertsChanged();
  expect(fn).toHaveBeenCalledTimes(2);

  off();
  notifyAlertsChanged();
  expect(fn).toHaveBeenCalledTimes(2); // no delivery after unsubscribe
});

it("fans out to multiple independent subscribers", () => {
  const a = vi.fn();
  const b = vi.fn();
  const offA = onAlertsChanged(a);
  const offB = onAlertsChanged(b);

  notifyAlertsChanged();
  expect(a).toHaveBeenCalledTimes(1);
  expect(b).toHaveBeenCalledTimes(1);

  // Unsubscribing one must not affect the other.
  offA();
  notifyAlertsChanged();
  expect(a).toHaveBeenCalledTimes(1);
  expect(b).toHaveBeenCalledTimes(2);
  offB();
});
