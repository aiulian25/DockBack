/**
 * ipFromNodeAddr pulls a literal IPv4 out of a DOCKER_HOST-style node address
 * (e.g. "ssh://user@192.0.2.10:22" -> "192.0.2.10").
 *
 * Returns "" when the node is addressed by name (a socket-proxy hostname, say)
 * or has no address at all — the operator then types the machine's IP by hand.
 * Mirrors the backend's HostIPFromDockerAddress.
 *
 * A hostname must come back empty rather than being offered as an IP: this
 * prefills the IP-remap fields of a restore, and a hostname there rewrites a
 * container's configuration to an address that does not resolve on the target.
 */
export function ipFromNodeAddr(addr?: string): string {
  if (!addr) return "";
  const match = /^[a-z0-9]+:\/\/(?:[^@/]*@)?([^:/]+)/i.exec(addr);
  const host = match?.[1] || "";
  return /^\d{1,3}(\.\d{1,3}){3}$/.test(host) ? host : "";
}
