/**
 * Which run-log lines belong in the restore progress panel.
 *
 * The same expression lived inline in two pages. It is here because it is a
 * CONTRACT, not a display detail: a line the engine writes for the operator and
 * this filter drops is a line the operator never sees, and nothing fails. #17's
 * neutralisation block is the case that made that worth pinning — a feature
 * whose entire value is "show exactly what was neutralised" is worth nothing if
 * a wording change quietly stops it being shown.
 */
const RESTORE_PROGRESS =
  /restor|neutralis|volume data|sidecar|starting container|container is running|re-import|database|integrity|safety snapshot/i;

export function isRestoreProgressLine(msg: string): boolean {
  return RESTORE_PROGRESS.test(msg);
}
