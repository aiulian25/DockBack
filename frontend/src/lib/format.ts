// Seconds thresholds shared by every duration label, so the three pages that
// render one cannot drift into disagreeing about where "minutes" becomes
// "hours".
const ONE_MINUTE = 60;
const ONE_HOUR = 3600;
const ONE_DAY = 86400;
const SECONDS_CUTOFF = 90;
const MINUTES_CUTOFF = 90 * ONE_MINUTE;
const HOURS_CUTOFF = 2 * ONE_DAY;
// Below this, an hour count is worth a decimal place: "3.5h" says more than
// "4h" when the compact form is the only thing in the cell.
const FRACTIONAL_HOURS_CUTOFF = 10 * ONE_HOUR;

/**
 * How a duration is spelled.
 *
 * "long" ("15 min", "6 h") reads as prose beside a sentence. "compact"
 * ("15m", "3.5h") fits the dense metric tables on Insights, where the label
 * shares a cell with a number. They were separate copies of this function
 * before, which is how they came to round hours differently.
 */
export type DurationStyle = "long" | "compact";

/** fmtDuration renders a second count as a short human label. */
export function fmtDuration(seconds: number, style: DurationStyle = "long"): string {
  if (seconds < 0) return "never";
  const compact = style === "compact";
  if (seconds < SECONDS_CUTOFF) return `${Math.round(seconds)}${compact ? "s" : " sec"}`;
  if (seconds < MINUTES_CUTOFF) return `${Math.round(seconds / ONE_MINUTE)}${compact ? "m" : " min"}`;
  if (seconds < HOURS_CUTOFF) {
    const hours = seconds / ONE_HOUR;
    const value = compact ? hours.toFixed(hours < FRACTIONAL_HOURS_CUTOFF / ONE_HOUR ? 1 : 0) : Math.round(hours);
    return `${value}${compact ? "h" : " h"}`;
  }
  return `${Math.round(seconds / ONE_DAY)}${compact ? "d" : " d"}`;
}
