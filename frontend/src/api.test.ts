import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, csrf, StepUpError } from "./api";

// Override document.cookie with a controlled string so we test the CSRF regex
// (api.ts) deterministically, independent of jsdom's cookie jar and its
// __Host-/Secure rules.
function setCookie(value: string) {
  Object.defineProperty(document, "cookie", { configurable: true, get: () => value });
}

describe("csrf()", () => {
  it("reads dback_csrf and URL-decodes the value", () => {
    setCookie("dback_csrf=a%20b");
    expect(csrf()).toBe("a b");
  });

  it("reads the __Host- prefixed cookie (behind TLS)", () => {
    setCookie("__Host-dback_csrf=tok123");
    expect(csrf()).toBe("tok123");
  });

  it("finds it among other cookies", () => {
    setCookie("theme=dark; __Host-dback_csrf=xyz; sidebar=1");
    expect(csrf()).toBe("xyz");
  });

  it("returns empty string when neither cookie is present", () => {
    setCookie("theme=dark; sidebar=1");
    expect(csrf()).toBe("");
  });
});

// One fake Response shape covering what req() reads: status, ok, text().
function resp(status: number, body: string): Response {
  return {
    status,
    ok: status >= 200 && status < 300,
    statusText: "",
    text: async () => body,
  } as unknown as Response;
}

describe("req() via api.*", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    setCookie("dback_csrf=CSRFVAL");
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    // A location we can inspect for the redirect branch.
    vi.stubGlobal("location", { pathname: "/settings", href: "" });
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  // The capacity verdict is sized server-side, so the selection has to travel
  // with the request. It used to be built into the options object and then
  // silently dropped, which made the panel describe the whole stack while the
  // operator was restoring part of it.
  it("sends the kept services on the stack restore plan", async () => {
    fetchMock.mockResolvedValue(resp(200, JSON.stringify({ services: [] })));
    await api.stackRestorePlan("n1", "my stack", { services: ["db", "web"] });
    const [path] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toContain("/api/nodes/n1/stacks/my%20stack/restore-plan");
    expect(decodeURIComponent(path)).toContain("services=db,web");
  });

  it("omits the services filter when the whole stack is selected", async () => {
    fetchMock.mockResolvedValue(resp(200, JSON.stringify({ services: [] })));
    await api.stackRestorePlan("n1", "proj");
    const [path] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).not.toContain("services=");
  });

  // During an outage the body is whatever sits in front of the app. Reporting
  // the parser's complaint instead of the server's status is worst exactly when
  // a clear message matters most.
  describe("non-JSON error bodies", () => {
    it("reports the status, not a parse error, for an HTML error page", async () => {
      fetchMock.mockResolvedValue({
        status: 502, ok: false, statusText: "Bad Gateway",
        text: async () => "<html><body>502 Bad Gateway</body></html>",
      } as unknown as Response);
      await expect(api.me()).rejects.toThrow("Bad Gateway");
    });

    // HTTP/2 carries no reason phrase, so statusText is empty behind exactly the
    // proxies that serve these pages. Without a fallback the message is blank.
    it("falls back to the status code when there is no reason phrase", async () => {
      fetchMock.mockResolvedValue({
        status: 502, ok: false, statusText: "",
        text: async () => "<html>oops</html>",
      } as unknown as Response);
      await expect(api.me()).rejects.toThrow("HTTP 502");
    });

    it("still prefers the app's own error message", async () => {
      fetchMock.mockResolvedValue({
        status: 400, ok: false, statusText: "Bad Request",
        text: async () => JSON.stringify({ error: "node not found" }),
      } as unknown as Response);
      await expect(api.me()).rejects.toThrow("node not found");
    });

    it("survives an empty body", async () => {
      fetchMock.mockResolvedValue({
        status: 503, ok: false, statusText: "Service Unavailable", text: async () => "",
      } as unknown as Response);
      await expect(api.me()).rejects.toThrow("Service Unavailable");
    });

    // A body that parses but is not an object (a bare "null", a quoted string)
    // must not become the error message or crash the property read.
    it("ignores a non-object JSON body", async () => {
      fetchMock.mockResolvedValue({
        status: 500, ok: false, statusText: "Internal Server Error", text: async () => '"boom"',
      } as unknown as Response);
      await expect(api.me()).rejects.toThrow("Internal Server Error");
    });

    it("login reports the status rather than a parse error", async () => {
      fetchMock.mockResolvedValue({
        status: 502, ok: false, statusText: "Bad Gateway", text: async () => "<html>down</html>",
      } as unknown as Response);
      await expect(api.login("admin", "pw")).rejects.toThrow("Bad Gateway");
    });
  });

  it("omits X-CSRF-Token on GET", async () => {
    fetchMock.mockResolvedValue(resp(200, JSON.stringify({ username: "admin" })));
    await api.me();
    const init = fetchMock.mock.calls[0][1] as RequestInit;
    expect(init.method).toBe("GET");
    expect((init.headers as Record<string, string>)["X-CSRF-Token"]).toBeUndefined();
  });

  it("attaches the CSRF token on a POST mutation", async () => {
    fetchMock.mockResolvedValue(resp(200, JSON.stringify({ status: "ok" })));
    await api.logout();
    const [path, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe("/api/logout");
    expect(init.method).toBe("POST");
    expect((init.headers as Record<string, string>)["X-CSRF-Token"]).toBe("CSRFVAL");
  });

  it("attaches the CSRF token on a DELETE mutation", async () => {
    fetchMock.mockResolvedValue(resp(200, JSON.stringify({ status: "deleted" })));
    await api.deleteAppDestination("d1");
    const init = fetchMock.mock.calls[0][1] as RequestInit;
    expect(init.method).toBe("DELETE");
    expect((init.headers as Record<string, string>)["X-CSRF-Token"]).toBe("CSRFVAL");
  });

  it("throws StepUpError (and does NOT redirect) on a 401 step_up_required", async () => {
    fetchMock.mockResolvedValue(
      resp(401, JSON.stringify({ error: "re-auth", step_up_required: true, totp_required: true })),
    );
    const loc = globalThis.location as unknown as { href: string };
    await expect(api.logout()).rejects.toBeInstanceOf(StepUpError);
    await api.logout().catch((e: StepUpError) => {
      expect(e.step_up_required).toBe(true);
      expect(e.totp_required).toBe(true);
    });
    expect(loc.href).toBe(""); // a live session must not be bounced to /login
  });

  it("redirects to /login on a plain 401", async () => {
    fetchMock.mockResolvedValue(resp(401, JSON.stringify({ error: "session expired" })));
    const loc = globalThis.location as unknown as { href: string };
    await expect(api.logout()).rejects.toThrow("unauthenticated");
    expect(loc.href).toBe("/login");
  });

  it("does not redirect when already on /login", async () => {
    vi.stubGlobal("location", { pathname: "/login", href: "" });
    fetchMock.mockResolvedValue(resp(401, JSON.stringify({ error: "bad creds" })));
    const loc = globalThis.location as unknown as { href: string };
    await expect(api.logout()).rejects.toThrow("unauthenticated");
    expect(loc.href).toBe(""); // stayed put — no self-redirect loop
  });

  it("surfaces the server error message on a non-2xx (non-401) response", async () => {
    fetchMock.mockResolvedValue(resp(400, JSON.stringify({ error: "name is required" })));
    await expect(api.logout()).rejects.toThrow("name is required");
  });
});
