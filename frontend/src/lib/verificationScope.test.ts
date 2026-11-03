import { describe, expect, it } from "vitest";
import { INTEGRITY_VERIFIED_LABEL, verificationScope } from "./verificationScope";

// DBK-022: a verified badge read as "this backup was test-restored", which the
// default always-on check never does.
describe("verificationScope", () => {
  it("says what verified proves and what it does not", () => {
    const verified = verificationScope("verified");
    expect(verified).toMatch(/read back end to end/);
    expect(verified).toMatch(/not a test restore/);
  });

  it("keeps failed and unverified apart, and treats anything else as unchecked", () => {
    expect(verificationScope("failed")).toMatch(/failed/);
    expect(verificationScope("unverified")).toMatch(/Not checked yet/);
    expect(verificationScope("something-new")).toBe(verificationScope("unverified"));
  });

  it("never labels an integrity check as a restore", () => {
    expect(INTEGRITY_VERIFIED_LABEL).not.toMatch(/by test-restore/);
    expect(INTEGRITY_VERIFIED_LABEL).toMatch(/not test-restored/);
  });
});
