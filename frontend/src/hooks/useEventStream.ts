// useEventStream — subscribe to the app's Server-Sent Events channel for live
// deltas (Fable-UI-UX A7). The backend multiplexes named events on the same
// /api/logs/stream every console uses: "node.summary", "backup.status",
// "run.done".
//
// This is now a thin hook over the SHARED stream (lib/logStream). It used to
// open its own EventSource, which was one of twelve — past the browser's
// six-per-origin limit on plain HTTP, so ordinary API fetches queued behind the
// log streams until a console closed.
import { useEffect, useRef, useState } from "react";
import { isConnected, subscribeConnection, subscribeEvent } from "../lib/logStream";

// Handlers keyed by SSE event name; each receives the parsed JSON payload.
type Handlers = Record<string, (data: any) => void>;

export function useEventStream(handlers: Handlers): { connected: boolean } {
  const [connected, setConnected] = useState(isConnected);
  // Keep the latest handlers in a ref so re-renders don't re-subscribe.
  const ref = useRef(handlers);
  ref.current = handlers;

  useEffect(() => {
    const stops = Object.keys(ref.current).map((name) =>
      subscribeEvent(name, (data) => ref.current[name]?.(data)),
    );
    const stopStatus = subscribeConnection(setConnected);
    setConnected(isConnected());
    return () => {
      for (const stop of stops) stop();
      stopStatus();
    };
    // Handler identities are read via the ref, so this effect runs once per mount.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return { connected };
}
