// Visibility-aware polling (perf Fix 9). A drop-in for the bare setInterval
// pollers: ticks are SKIPPED while the tab is hidden (a backgrounded dashboard
// stops hitting the server entirely), and returning to the tab fires one
// immediate refresh so the page is never stale on focus. Foreground cadence is
// unchanged. Pass ms=null to pause the poller; extra deps restart the interval
// (e.g. a route param the poll closure reads).
//
// The latest fn is kept in a ref, so callers may pass inline closures without
// resetting the interval every render. This intentionally does NOT run fn on
// mount — call sites keep their existing initial load.
import { useEffect, useRef } from "react";

export function usePoll(fn: () => void, ms: number | null, deps: unknown[] = []) {
  const fnRef = useRef(fn);
  fnRef.current = fn;
  useEffect(() => {
    if (ms == null || ms <= 0) return;
    const t = setInterval(() => { if (!document.hidden) fnRef.current(); }, ms);
    const onVis = () => { if (!document.hidden) fnRef.current(); };
    document.addEventListener("visibilitychange", onVis);
    return () => { clearInterval(t); document.removeEventListener("visibilitychange", onVis); };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ms, ...deps]);
}
