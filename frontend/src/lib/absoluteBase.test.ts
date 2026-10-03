import { describe, it, expect } from "vitest";
import { baseDirProblem, baseDirInvalid } from "./absoluteBase";

// The nas01 → server2 case that prompted this: "home/user/docker" was refused by
// the server with a message that named neither the field nor the value, after a
// round trip, while a separate card reported every service as having no backup.
describe("baseDirProblem", () => {
  it("names the fix for a relative path", () => {
    expect(baseDirProblem("home/user/docker")).toContain("/home/user/docker");
    expect(baseDirInvalid("home/user/docker")).toBe(true);
  });

  it("accepts an absolute path", () => {
    expect(baseDirProblem("/home/user/docker")).toBe("");
    expect(baseDirProblem("  /opt/stacks  ")).toBe("");
    expect(baseDirInvalid("/volume1/docker")).toBe(false);
  });

  it("leaves an empty field alone", () => {
    // Blank means "derive it" (From) or "use the saved default" (To) — the
    // server owns that, and flagging it here would refuse a valid dialog.
    expect(baseDirProblem("")).toBe("");
    expect(baseDirProblem("   ")).toBe("");
    expect(baseDirInvalid("")).toBe(false);
  });

  it("refuses / as a base", () => {
    expect(baseDirProblem("/")).toContain("root directory");
    expect(baseDirProblem("//")).toContain("root directory");
  });
});
