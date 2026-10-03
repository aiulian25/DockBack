/**
 * The server's password-length bounds (internal/api/auth.go). The floor is
 * enforced on every change whatever the stored setting says, so the client must
 * never promise a shorter password than this.
 */
export const PASSWORD_LEN_FLOOR = 12;
export const PASSWORD_LEN_CEILING = 128;

/**
 * minPasswordLength reads the configured minimum and clamps it exactly as the
 * server does.
 *
 * The client used to hard-code 12 in its refusal message. Once an operator
 * raised the minimum, the browser accepted a password the server then rejected,
 * and the two disagreed about why. Deriving both from the same setting is what
 * keeps the message true; clamping is what keeps it true when the setting is
 * absent, corrupt, or out of range.
 */
export function minPasswordLength(configured: string | undefined): number {
  const parsed = parseInt(configured ?? "", 10);
  if (!Number.isFinite(parsed)) return PASSWORD_LEN_FLOOR;
  return Math.min(Math.max(parsed, PASSWORD_LEN_FLOOR), PASSWORD_LEN_CEILING);
}
