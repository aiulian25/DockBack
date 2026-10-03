import { describe, it, expect } from "vitest";
import { minPasswordLength, PASSWORD_LEN_FLOOR, PASSWORD_LEN_CEILING } from "./passwordPolicy";

describe("minPasswordLength", () => {
  it("uses the configured value", () => {
    expect(minPasswordLength("20")).toBe(20);
    expect(minPasswordLength("128")).toBe(128);
  });

  it("falls back to the floor when nothing is configured", () => {
    expect(minPasswordLength(undefined)).toBe(PASSWORD_LEN_FLOOR);
    expect(minPasswordLength("")).toBe(PASSWORD_LEN_FLOOR);
  });

  // A corrupt setting must not turn the check into NaN, which every comparison
  // fails — that would silently accept any password in the browser.
  it("falls back to the floor on a value that is not a number", () => {
    expect(minPasswordLength("abc")).toBe(PASSWORD_LEN_FLOOR);
    expect(minPasswordLength("NaN")).toBe(PASSWORD_LEN_FLOOR);
  });

  it("never promises less than the server enforces", () => {
    expect(minPasswordLength("4")).toBe(PASSWORD_LEN_FLOOR);
    expect(minPasswordLength("-10")).toBe(PASSWORD_LEN_FLOOR);
    expect(minPasswordLength("0")).toBe(PASSWORD_LEN_FLOOR);
  });

  it("clamps to the ceiling, so the form cannot demand the impossible", () => {
    expect(minPasswordLength("9999")).toBe(PASSWORD_LEN_CEILING);
  });
});
