/**
 * What a backup's verification state actually proves. Always-on verification reads
 * the archive back end to end; it does not restore it. A test restore or a restore
 * drill is a separate, opt-in check, and every label that says "verified" must not
 * suggest otherwise.
 */
export type VerificationState = "verified" | "failed" | "unverified";

const SCOPE: Record<VerificationState, string> = {
  verified:
    "Integrity verified: read back end to end (stored hash, full decrypt and decompress, every archive member, " +
    "and each database dump's header). This is not a test restore; a restore drill is what proves a restore works.",
  failed: "Integrity check failed: the archive could not be read back intact. Do not rely on this backup.",
  unverified: "Not checked yet: stored, but not yet read back.",
};

/** The sentence a "Verified / Failed / Unverified" badge stands for. */
export function verificationScope(state: string): string {
  return SCOPE[state as VerificationState] ?? SCOPE.unverified;
}

/** The short timeline label for an integrity-verified backup. */
export const INTEGRITY_VERIFIED_LABEL = "Integrity-verified — not test-restored";
