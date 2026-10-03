import { useCallback, useEffect, useRef } from "react";
import type { UIEvent } from "react";

// useStickyScroll pins a streaming log window to its newest line — so the
// current stage is always in view — WITHOUT fighting a user who scrolls up to
// read history. Pass the rendered line count (or any value that changes as lines
// arrive) as `dep`, then attach the result to the scroll container:
//
//   const log = useStickyScroll(lines.length);
//   <div ref={log.ref} onScroll={log.onScroll} className="overflow-y-auto …">
//
// It follows while the viewport is at (or near) the bottom; scrolling further up
// pauses the follow so you can read, and scrolling back down resumes it. When
// `dep` resets to 0 (a new run clears the log) it re-follows automatically.
export function useStickyScroll(dep: number) {
  const ref = useRef<HTMLDivElement>(null);
  const stick = useRef(true);

  useEffect(() => {
    if (dep === 0) stick.current = true; // log cleared for a new run — follow again
    if (stick.current && ref.current) ref.current.scrollTop = ref.current.scrollHeight;
  }, [dep]);

  const onScroll = useCallback((e: UIEvent<HTMLDivElement>) => {
    const el = e.currentTarget;
    // Within 40px of the bottom counts as "following"; scrolling higher pauses it.
    stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
  }, []);

  return { ref, onScroll };
}
