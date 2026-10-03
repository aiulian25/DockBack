import { describe, it, expect } from "vitest";
import { stackProgress, stackVerdict } from "./stackRunProgress";

// The reported bug: a five-service stack restore showed `[1/5] Service "db"
// restored`, declared itself done, closed the stream, and left the operator to
// check the receiving host to learn whether the other four worked.
describe("stackVerdict", () => {
  it("a per-service line never ends the run", () => {
    // THE bug. This line ends with "restored" and the old pattern was `restored$`.
    expect(stackVerdict('[1/5] Service "db" restored', "INFO")).toBe("running");
    expect(stackVerdict('[5/5] Service "paperless" restored', "INFO")).toBe("running");
    expect(stackVerdict('[2/5] Restoring service "redis" (PaperlessNGX-REDIS)…', "INFO")).toBe("running");
    // Even a per-service WARN keeps the run alive — the engine continues.
    expect(stackVerdict('[3/5] Service "tika" restored but not healthy yet — continuing', "WARN")).toBe("running");
  });

  it("only the whole-stack line reports success", () => {
    expect(stackVerdict('Stack "paperlessngx" restored — all 5 services are running', "INFO")).toBe("ok");
    expect(stackVerdict("Stack snapshot complete", "INFO")).toBe("ok");
    expect(stackVerdict("Stack backup complete", "INFO")).toBe("ok");
  });

  it("failures end the run", () => {
    expect(stackVerdict("Stack restore refused: no backup for service redis", "ERR")).toBe("fail");
    // The level fold matters: this used to arrive as "ERROR" and match nothing.
    expect(stackVerdict("Stack restore refused: something", "ERROR")).toBe("fail");
    expect(stackVerdict('Stack "x" restored, but 2 service(s) did not become healthy: a, b', "ERR")).toBe("fail");
    expect(stackVerdict("Stack restore CANCELED after 2 of 5 service(s)", "WARN")).toBe("fail");
  });

  it("a failure inside a per-service line still does not end the run", () => {
    // The engine keeps going after an unhealthy service and reports at the end.
    expect(stackVerdict('[2/5] Service "redis" restored but not healthy yet', "WARN")).toBe("running");
  });

  it("ordinary chatter is not a verdict", () => {
    expect(stackVerdict("Restore order: db → redis → gotenberg → tika → paperless", "INFO")).toBe("running");
    expect(stackVerdict("Wrote the stack's compose file to /opt/docker/x", "INFO")).toBe("running");
  });
});

describe("stackProgress", () => {
  it("counts finished services, not started ones", () => {
    expect(stackProgress('[1/5] Restoring service "db" (PaperlessNGX-DB)…')).toEqual({ done: 0, total: 5 });
    expect(stackProgress('[1/5] Service "db" restored')).toEqual({ done: 1, total: 5 });
    expect(stackProgress('[5/5] Service "paperless" restored')).toEqual({ done: 5, total: 5 });
  });

  it("an unhealthy-but-restored service still counts", () => {
    // Its data is in place and the run moves on; the final line reports health.
    expect(stackProgress('[3/5] Service "tika" restored but not healthy yet — continuing')).toEqual({ done: 3, total: 5 });
  });

  it("lines without a bracket prefix carry no progress", () => {
    expect(stackProgress("Restore order: db → redis")).toBeNull();
    expect(stackProgress('Stack "x" restored — all 5 services are running')).toBeNull();
    expect(stackProgress("")).toBeNull();
  });

  it("a malformed prefix is ignored rather than half-read", () => {
    expect(stackProgress("[a/5] Service x restored")).toBeNull();
    expect(stackProgress("[1/0] Service x restored")).toBeNull();
  });
});
