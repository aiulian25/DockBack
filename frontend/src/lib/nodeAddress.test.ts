import { describe, it, expect } from "vitest";
import { ipFromNodeAddr } from "./nodeAddress";

// Two pages carried their own copy and they had diverged: one required a
// lowercase scheme of letters only, so an uppercase or digit-bearing scheme
// silently produced no prefill.
describe("ipFromNodeAddr", () => {
  it("pulls the IP out of the usual forms", () => {
    expect(ipFromNodeAddr("ssh://user@192.0.2.10:22")).toBe("192.0.2.10");
    expect(ipFromNodeAddr("tcp://192.0.2.10:2375")).toBe("192.0.2.10");
    expect(ipFromNodeAddr("ssh://192.0.2.10")).toBe("192.0.2.10");
  });

  it("accepts schemes with digits and any case", () => {
    expect(ipFromNodeAddr("SSH://user@192.0.2.10:22")).toBe("192.0.2.10");
    expect(ipFromNodeAddr("tcp6://192.0.2.10:2375")).toBe("192.0.2.10");
  });

  // The important refusal: a hostname prefilled as an IP would rewrite a
  // restored container to an address that does not resolve.
  it("returns nothing for a host that is not a literal IPv4", () => {
    expect(ipFromNodeAddr("tcp://socket-proxy:2375")).toBe("");
    expect(ipFromNodeAddr("ssh://user@backup.example:22")).toBe("");
    expect(ipFromNodeAddr("unix:///var/run/docker.sock")).toBe("");
  });

  it("returns nothing for missing or unparseable input", () => {
    expect(ipFromNodeAddr()).toBe("");
    expect(ipFromNodeAddr("")).toBe("");
    expect(ipFromNodeAddr("192.0.2.10")).toBe("");
  });
});
