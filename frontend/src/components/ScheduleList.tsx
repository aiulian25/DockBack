import type { ReactNode } from "react";
import type { NamedSchedule } from "../api";
import { Chip } from "./ui";
import { fmtDuration, scheduleWhen } from "../lib/format";

/**
 * ScheduleList shows the schedules that back something up: each one's name,
 * whether it is on, when it fires, how it reaches the thing (`describe`) and
 * how long until it runs. Switched-off schedules are listed too, so one set
 * earlier never goes invisible.
 */
export default function ScheduleList<T extends NamedSchedule>({ schedules, describe, action }: {
  schedules: T[];
  describe: (schedule: T) => string;
  action?: (schedule: T) => ReactNode;
}) {
  return (
    <ul className="space-y-1.5">
      {schedules.map((schedule) => (
        <li key={schedule.id} className="rounded border border-outline-variant bg-surface-lowest px-3 py-2 text-sm">
          <div className="flex flex-wrap items-center gap-2">
            <span className="min-w-0 break-words font-medium text-on-surface">{schedule.name}</span>
            <Chip kind={schedule.enabled ? "ok" : "muted"}>{schedule.enabled ? "on" : "off"}</Chip>
            <span className="text-on-surface-variant">{scheduleWhen(schedule)}</span>
            {action && <span className="ml-auto shrink-0">{action(schedule)}</span>}
          </div>
          <div className="mt-0.5 break-words text-xs text-on-surface-variant">
            {describe(schedule)} · <NextRun schedule={schedule} />
          </div>
        </li>
      ))}
    </ul>
  );
}

/** NextRun says how long until a schedule fires, or that it is switched off. */
export function NextRun({ schedule }: { schedule: NamedSchedule }) {
  if (!schedule.enabled || !schedule.next_run) return <>switched off, so it does not run</>;
  // Relative, because the schedule's own time is the server's clock and an
  // absolute date here would be the browser's.
  return (
    <span title={new Date(schedule.next_run * 1000).toLocaleString()}>
      next run in <span className="text-on-surface">{fmtDuration(schedule.next_run - Date.now() / 1000)}</span>
    </span>
  );
}
