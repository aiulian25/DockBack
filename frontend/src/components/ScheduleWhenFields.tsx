import type { Schedule } from "../api";
import { Input, Label, Select } from "./ui";
import { WEEKDAYS } from "../lib/format";

export type ScheduleWhen = Pick<Schedule, "kind" | "time" | "weekday" | "monthday" | "cron">;

/** ScheduleWhenFields edits when a schedule fires, with the choices its Settings card offers. */
export default function ScheduleWhenFields({ value, onChange }: { value: ScheduleWhen; onChange: (patch: Partial<ScheduleWhen>) => void }) {
  return (
    <div className="grid grid-cols-2 gap-3">
      <div>
        <Label>Frequency</Label>
        <Select value={value.kind} onChange={(e) => onChange({ kind: e.target.value })}>
          <option value="daily">Daily</option>
          <option value="weekly">Weekly</option>
          <option value="monthly">Monthly</option>
          <option value="custom">Custom (cron)</option>
        </Select>
      </div>
      {value.kind !== "custom" && (
        <div><Label>Time</Label><Input type="time" value={value.time} onChange={(e) => onChange({ time: e.target.value })} /></div>
      )}
      {value.kind === "weekly" && (
        <div>
          <Label>Day of week</Label>
          <Select value={value.weekday} onChange={(e) => onChange({ weekday: parseInt(e.target.value, 10) })}>
            {WEEKDAYS.map((day, index) => <option key={day} value={index}>{day}</option>)}
          </Select>
        </div>
      )}
      {value.kind === "monthly" && (
        <div><Label>Day of month (1–28)</Label><Input type="number" min={1} max={28} value={value.monthday} onChange={(e) => onChange({ monthday: parseInt(e.target.value || "1", 10) })} /></div>
      )}
      {value.kind === "custom" && (
        <div className="col-span-2"><Label>Cron (min hour dom mon dow)</Label><Input value={value.cron} onChange={(e) => onChange({ cron: e.target.value })} placeholder="0 3 * * 0" /></div>
      )}
    </div>
  );
}
