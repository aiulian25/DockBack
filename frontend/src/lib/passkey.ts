// The browser's side of a passkey ceremony. The server speaks WebAuthn's JSON
// (base64url strings); navigator.credentials wants binary, so this converts both
// ways. The browsers' own parse…FromJSON / toJSON would do it, but are not yet
// in every browser DockBack supports.

const PUBLIC_KEY = "public-key";
// What a browser throws when this device already holds a passkey for this site.
const ALREADY_REGISTERED = "InvalidStateError";

export function fromBase64url(value: string): ArrayBuffer {
  const base64 = value.replace(/-/g, "+").replace(/_/g, "/");
  const binary = atob(base64.padEnd(Math.ceil(base64.length / 4) * 4, "="));
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  return bytes.buffer;
}

export function toBase64url(buffer: ArrayBuffer): string {
  let binary = "";
  for (const byte of new Uint8Array(buffer)) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

type CredentialRef = { id: string; type: string; transports?: string[] };

// The JSON shapes the server sends (WebAuthn's own, with binary fields as base64url).
export interface CreationOptionsJSON {
  rp: PublicKeyCredentialRpEntity;
  user: { id: string; name: string; displayName: string };
  challenge: string;
  pubKeyCredParams: PublicKeyCredentialParameters[];
  timeout?: number;
  excludeCredentials?: CredentialRef[];
  authenticatorSelection?: AuthenticatorSelectionCriteria;
  attestation?: AttestationConveyancePreference;
}

export interface RequestOptionsJSON {
  challenge: string;
  rpId?: string;
  timeout?: number;
  allowCredentials?: CredentialRef[];
  userVerification?: UserVerificationRequirement;
}

const descriptorOf = (ref: CredentialRef): PublicKeyCredentialDescriptor => ({
  id: fromBase64url(ref.id), type: PUBLIC_KEY, transports: ref.transports as AuthenticatorTransport[] | undefined,
});

export function creationOptions(json: CreationOptionsJSON): PublicKeyCredentialCreationOptions {
  return {
    ...json,
    challenge: fromBase64url(json.challenge),
    user: { ...json.user, id: fromBase64url(json.user.id) },
    excludeCredentials: json.excludeCredentials?.map(descriptorOf),
  };
}

export function requestOptions(json: RequestOptionsJSON): PublicKeyCredentialRequestOptions {
  return { ...json, challenge: fromBase64url(json.challenge), allowCredentials: json.allowCredentials?.map(descriptorOf) };
}

const isAttestation = (r: AuthenticatorResponse): r is AuthenticatorAttestationResponse => "attestationObject" in r;

// credentialJSON is the passkey's answer in the JSON the server parses.
export function credentialJSON(credential: PublicKeyCredential): Record<string, unknown> {
  const r = credential.response;
  const response = isAttestation(r)
    ? { clientDataJSON: toBase64url(r.clientDataJSON), attestationObject: toBase64url(r.attestationObject), transports: r.getTransports?.() ?? [] }
    : {
        clientDataJSON: toBase64url(r.clientDataJSON),
        authenticatorData: toBase64url((r as AuthenticatorAssertionResponse).authenticatorData),
        signature: toBase64url((r as AuthenticatorAssertionResponse).signature),
        userHandle: (r as AuthenticatorAssertionResponse).userHandle ? toBase64url((r as AuthenticatorAssertionResponse).userHandle!) : undefined,
      };
  return {
    id: credential.id, rawId: toBase64url(credential.rawId), type: credential.type,
    authenticatorAttachment: credential.authenticatorAttachment ?? undefined,
    clientExtensionResults: credential.getClientExtensionResults(), response,
  };
}

/** The browser's own prompt ended without a passkey: cancelled, timed out, nothing to offer, or one already here. */
export class PasskeyPromptStopped extends Error {
  constructor(readonly alreadyHere: boolean) {
    super(alreadyHere ? "This device already has a passkey for DockBack." : "The browser stopped without a passkey: cancelled, timed out, or nothing to offer for this site.");
  }
}

export const passkeysSupported = () => typeof window !== "undefined" && "PublicKeyCredential" in window && !!navigator.credentials;

async function prompted(ask: () => Promise<Credential | null>): Promise<Record<string, unknown>> {
  try {
    const credential = await ask();
    if (!credential) throw new PasskeyPromptStopped(false);
    return credentialJSON(credential as PublicKeyCredential);
  } catch (e) {
    if (e instanceof PasskeyPromptStopped) throw e;
    throw new PasskeyPromptStopped((e as Error)?.name === ALREADY_REGISTERED);
  }
}

export const makePasskey = (options: CreationOptionsJSON) =>
  prompted(() => navigator.credentials.create({ publicKey: creationOptions(options) }));

export const answerWithPasskey = (options: RequestOptionsJSON) =>
  prompted(() => navigator.credentials.get({ publicKey: requestOptions(options) }));

// browserName is the browser's own guess at a name, for a passkey whose password
// manager doesn't say ("Firefox on Linux").
export function browserName(userAgent: string): string {
  const browser = /Edg\//.test(userAgent) ? "Edge" : /Firefox\//.test(userAgent) ? "Firefox"
    : /Chrome\//.test(userAgent) ? "Chrome" : /Safari\//.test(userAgent) ? "Safari" : "Browser";
  const os = /Windows/.test(userAgent) ? "Windows" : /Android/.test(userAgent) ? "Android"
    : /iPhone|iPad/.test(userAgent) ? "iOS" : /Mac OS X/.test(userAgent) ? "macOS" : /Linux/.test(userAgent) ? "Linux" : "";
  return os ? `${browser} on ${os}` : browser;
}
