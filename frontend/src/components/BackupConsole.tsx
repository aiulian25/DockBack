import { useEffect, useState } from "react";
import { subscribeLines, LogLine } from "../lib/logStream";
import { useStickyScroll } from "../hooks/useStickyScroll";
import { Card } from "./ui";
import { levelClass } from "./RunLog";

// Enough to scroll back through a long run without the page holding an
// unbounded log in memory.
const MAX_CONSOLE_LINES = 500;

/**
 * BackupConsole is the live output of one container's backups.
 *
 * The stream carries every node's and container's backup output, so this shows
 * only the given container's lines — without the filter, a backup of another
 * container, even on another node, would bleed into it. It follows what happens
 * while it is on screen; earlier runs are in each backup's stored log.
 */
export default function BackupConsole({ containerId, className = "" }: { containerId: string; className?: string }) {
  const [lines, setLines] = useState<LogLine[]>([]);
  const log = useStickyScroll(lines.length);

  useEffect(() => {
    setLines([]); // drop the previous container's output when switching containers
    return subscribeLines((line) => {
      if (line.container_id !== containerId) return;
      setLines((previous) => [...previous, line].slice(-MAX_CONSOLE_LINES));
    });
  }, [containerId]);

  return (
    <Card className={`overflow-hidden ${className}`}>
      <div className="flex items-center justify-between border-b border-outline-variant/60 bg-surface-low px-4 py-2">
        <div className="flex items-center gap-2">
          <span className="h-3 w-3 rounded-full bg-error/70" /><span className="h-3 w-3 rounded-full bg-warning/70" /><span className="h-3 w-3 rounded-full bg-success/70" />
          <span className="ml-2 font-mono text-xs text-on-surface-variant">Console Output — backup_stream.log</span>
        </div>
        <button onClick={() => setLines([])} className="text-xs font-semibold uppercase tracking-wider text-on-surface-variant hover:text-on-surface">Clear</button>
      </div>
      <div ref={log.ref} onScroll={log.onScroll} className="h-[28rem] overflow-y-auto bg-surface-lowest p-4 font-mono text-xs leading-relaxed">
        {lines.length === 0 && <div className="text-on-surface-variant">Awaiting system events… initiate a backup to see live output.</div>}
        {lines.map((line, index) => (
          <div key={index} className="flex gap-3 py-0.5">
            <span className="shrink-0 text-on-surface-variant">{line.time.replace("T", " ").replace("Z", "")}</span>
            <span className={`shrink-0 font-semibold ${levelClass(line.level)}`}>{line.level}</span>
            <span className="text-on-surface">{line.msg}</span>
          </div>
        ))}
      </div>
    </Card>
  );
}
