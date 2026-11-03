import { describe, expect, it } from "vitest";
import { browserName, creationOptions, credentialJSON, fromBase64url, requestOptions, toBase64url } from "./passkey";

const bytes = (...values: number[]) => new Uint8Array(values).buffer;

describe("passkey JSON", () => {
  it("round-trips base64url, with and without padding or the url-safe characters", () => {
    for (const value of [bytes(), bytes(0), bytes(0xfb, 0xff), bytes(1, 2, 3, 4, 5)]) {
      expect(new Uint8Array(fromBase64url(toBase64url(value)))).toEqual(new Uint8Array(value));
    }
    expect(toBase64url(bytes(0xfb, 0xff))).toBe("-_8");
  });

  it("turns the server's options into the binary the browser wants", () => {
    const creation = creationOptions({
      rp: { id: "dockback.test", name: "DockBack" }, user: { id: "MQ", name: "admin", displayName: "admin" },
      challenge: "AQID", pubKeyCredParams: [{ type: "public-key", alg: -7 }], excludeCredentials: [{ id: "BAU", type: "public-key" }],
    });
    expect(new Uint8Array(creation.challenge as ArrayBuffer)).toEqual(new Uint8Array([1, 2, 3]));
    expect(new TextDecoder().decode(creation.user.id as ArrayBuffer)).toBe("1");
    expect(new Uint8Array(creation.excludeCredentials![0].id as ArrayBuffer)).toEqual(new Uint8Array([4, 5]));

    const request = requestOptions({ challenge: "AQID", rpId: "dockback.test", userVerification: "required" });
    expect(new Uint8Array(request.challenge as ArrayBuffer)).toEqual(new Uint8Array([1, 2, 3]));
    expect(request.rpId).toBe("dockback.test");
  });

  it("hands the server an assertion as base64url JSON", () => {
    const assertion = {
      id: "AQ", rawId: bytes(1), type: "public-key", authenticatorAttachment: "platform",
      getClientExtensionResults: () => ({}),
      response: { clientDataJSON: bytes(2), authenticatorData: bytes(3), signature: bytes(4), userHandle: bytes(0x31) },
    } as unknown as PublicKeyCredential;
    expect(credentialJSON(assertion)).toEqual({
      id: "AQ", rawId: "AQ", type: "public-key", authenticatorAttachment: "platform", clientExtensionResults: {},
      response: { clientDataJSON: "Ag", authenticatorData: "Aw", signature: "BA", userHandle: "MQ" },
    });
  });

  it("guesses a readable name from the browser", () => {
    expect(browserName("Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0")).toBe("Firefox on Linux");
    expect(browserName("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0 Safari/537.36 Edg/141.0")).toBe("Edge on Windows");
    expect(browserName("Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1")).toBe("Safari on iOS");
  });
});
