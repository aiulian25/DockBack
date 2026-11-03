import { describe, expect, it } from "vitest";
import addNodeDialog from "../components/AddNodeModal.tsx?raw";

// A Docker API port published with no address listens on every interface. The
// socket-proxy DockBack asks for allows container creation, so whoever reaches
// that port owns the host, and Docker's own rules for published ports skip a host
// firewall such as UFW. Every copy-ready snippet must name the address.
const DOCKER_API_PORT = "237[56]";
const PUBLISH_ON_EVERY_INTERFACE = new RegExp(
  `(^\\s*-\\s*["']?|\\s-p\\s+)(0\\.0\\.0\\.0:|\\[::\\]:)?${DOCKER_API_PORT}:${DOCKER_API_PORT}\\b`,
  "m",
);

// The same in-app articles Docs.tsx loads, plus the Add Node dialog's Copy snippet.
const guidance: Record<string, string> = {
  ...(import.meta.glob("./content/**/*.md", { query: "?raw", import: "default", eager: true }) as Record<string, string>),
  "components/AddNodeModal.tsx": addNodeDialog,
};

describe("remote node guidance", () => {
  it("never publishes the Docker API on every interface", () => {
    const offenders = Object.keys(guidance).filter((file) => PUBLISH_ON_EVERY_INTERFACE.test(guidance[file]));
    expect(offenders).toEqual([]);
  });

  it("recognises the publications it forbids", () => {
    for (const unsafe of [`      - "2375:2375"`, `      - 2375:2375`, `      - "0.0.0.0:2375:2375"`, `docker run -p 2376:2376 x`]) {
      expect(PUBLISH_ON_EVERY_INTERFACE.test(unsafe), unsafe).toBe(true);
    }
    expect(PUBLISH_ON_EVERY_INTERFACE.test(`      - "<overlay-ip>:2375:2375"`)).toBe(false);
    expect(PUBLISH_ON_EVERY_INTERFACE.test(`      - "10.10.0.2:2375:2375"`)).toBe(false);
  });
});
