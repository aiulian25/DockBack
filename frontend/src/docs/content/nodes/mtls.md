# Connect over daemon mTLS

If your Docker daemon is configured to listen on a TLS port (`2376`) with mutual authentication, DockBack can connect to it directly. This gives you an **encrypted, mutually-authenticated** connection over the wire — the strongest option — without SSH or a socket-proxy.

## Prerequisites

You need dockerd configured for TLS, and the matching client credentials:

- the **CA** certificate that signed the daemon's cert,
- a **client certificate**, and
- the **client private key**.

Setting up TLS on the daemon is a host-side task (see the Docker documentation for protecting the daemon socket with TLS).

## Steps

1. **Servers → Add Node**.
2. **Transport** — *Daemon mTLS*.
3. **Address** — `tcp://host:2376`.
4. **TLS bundle** — paste the three PEM blocks (CA, client cert, client key) into the credential field, separated by a line containing `---`:

```
-----BEGIN CERTIFICATE-----
... CA ...
-----END CERTIFICATE-----
---
-----BEGIN CERTIFICATE-----
... client cert ...
-----END CERTIFICATE-----
---
-----BEGIN PRIVATE KEY-----
... client key ...
-----END PRIVATE KEY-----
```

5. **Test Connection** → **Add Node**.

The bundle is encrypted at rest. When editing the node later, leave the field blank to keep the stored bundle.
