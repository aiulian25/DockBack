// "Connect New Node" / "Edit Node" modal: name, transport,
// address, credentials, and a live Test Connection before saving. When `edit`
// is provided the form prefills and saves via PUT (blank secret keeps the
// stored key).
import { useEffect, useState } from "react";
import { Copy, Check, AlertTriangle } from "lucide-react";
import { api, Node } from "../api";
import { Button, Input, Label, Modal, Select } from "./ui";
import ClusterSelect from "./ClusterSelect";

const transports = [
  { v: "local-proxy", label: "Local socket-proxy (tcp)", ph: "tcp://socket-proxy:2375" },
  { v: "tcp-proxy", label: "Remote socket-proxy (tcp)", ph: "tcp://10.168.1.172:2375" },
  { v: "ssh", label: "SSH", ph: "ssh://user@10.168.1.172:22" },
  { v: "mtls", label: "Daemon mTLS", ph: "tcp://10.168.1.172:2376" },
];

// Ready-to-run docker-socket-proxy compose for a remote host / NAS. Grants
// exactly what DockBack needs. Keep port 2375 firewalled to the DockBack host.
const SOCKET_PROXY_COMPOSE = `services:
  socket-proxy:
    image: tecnativa/docker-socket-proxy:0.3.0
    restart: unless-stopped
    ports:
      - "2375:2375"
    environment:
      CONTAINERS: 1
      IMAGES: 1
      VOLUMES: 1
      NETWORKS: 1
      EXEC: 1
      VERSION: 1
      INFO: 1
      POST: 1
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    networks:
      - dback_internal
    security_opt:
      - "no-new-privileges:true"

networks:
  dback_internal:
    driver: bridge`;

function ComposeSnippet() {
  const [copied, setCopied] = useState(false);
  const copy = () => {
    navigator.clipboard?.writeText(SOCKET_PROXY_COMPOSE);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  };
  return (
    <details className="mt-2 rounded border border-outline-variant bg-surface-lowest text-xs">
      <summary className="cursor-pointer px-3 py-2 text-on-surface-variant hover:text-on-surface">
        Need a socket-proxy on that host? Show the compose file
      </summary>
      <div className="border-t border-outline-variant/60 p-2">
        <div className="mb-1 flex items-center justify-between">
          <a href="https://github.com/tecnativa/docker-socket-proxy" target="_blank" rel="noreferrer" className="text-docker-blue hover:underline">
            tecnativa/docker-socket-proxy ↗
          </a>
          <button onClick={copy} className="flex items-center gap-1 rounded border border-outline-variant px-2 py-0.5 text-on-surface-variant hover:text-on-surface">
            {copied ? <><Check size={12} className="text-success" /> Copied</> : <><Copy size={12} /> Copy</>}
          </button>
        </div>
        <pre className="max-h-56 overflow-auto rounded bg-surface px-3 py-2 font-mono text-[11px] leading-relaxed text-on-surface-variant">{SOCKET_PROXY_COMPOSE}</pre>
        <p className="mt-1 text-on-surface-variant">Run <span className="font-mono">docker compose up -d</span> on the host, firewall <span className="font-mono">2375</span> to this server, then set the address to <span className="font-mono">tcp://&lt;host-ip&gt;:2375</span>.</p>
      </div>
    </details>
  );
}

export default function AddNodeModal({
  open, onClose, onAdded, edit,
}: { open: boolean; onClose: () => void; onAdded: () => void; edit?: Node | null }) {
  const [name, setName] = useState("");
  const [cluster, setCluster] = useState("default");
  const [transport, setTransport] = useState("tcp-proxy");
  const [address, setAddress] = useState("");
  const [secret, setSecret] = useState("");
  const [passphrase, setPassphrase] = useState("");
  const [password, setPassword] = useState("");
  const [sshAuth, setSshAuth] = useState<"key" | "password">("key"); // SSH: key vs password auth
  const [test, setTest] = useState<{ ok: boolean; msg: string; fp?: string } | null>(null);
  const [busy, setBusy] = useState(false);

  // Prefill when opening in edit mode (or reset for add). For SSH we open on the
  // node's actual auth method (ssh_auth) so a password node edits as a password node.
  useEffect(() => {
    if (!open) return;
    setTest(null); setSecret(""); setPassphrase(""); setPassword("");
    if (edit) {
      setName(edit.name); setCluster(edit.cluster || "default");
      setTransport(edit.transport); setAddress(edit.address);
      setSshAuth(edit.ssh_auth === "password" ? "password" : "key");
    } else {
      setName(""); setCluster("default"); setTransport("tcp-proxy"); setAddress(""); setSshAuth("key");
    }
  }, [open, edit]);

  const placeholder = transports.find((t) => t.v === transport)?.ph || "";
  // Editing to a DIFFERENT transport than the node was added with: the stored
  // credential belongs to the old transport and won't carry over, so the fields start
  // fresh and prompt for new credentials rather than "leave blank to keep stored".
  const switchingTransport = !!edit && transport !== edit.transport;
  const credEdit = !!edit && !switchingTransport; // editing the SAME transport → keep-stored applies
  const changeTransport = (t: string) => {
    setTransport(t); setSecret(""); setPassphrase(""); setPassword(""); setSshAuth("key"); setTest(null);
  };

  // Credential fields for the chosen transport/auth method. SSH sends auth_method +
  // only the relevant field; a blank field on edit keeps the stored value.
  const creds = () =>
    transport === "ssh"
      ? { auth_method: sshAuth, ...(sshAuth === "key" ? { secret, passphrase } : { password }) }
      : { secret };

  const doTest = async () => {
    setBusy(true); setTest(null);
    try {
      const r = await api.testNode({ transport, address, ...creds(), ...(edit ? { id: edit.id } : {}) });
      setTest(r.ok
        ? { ok: true, msg: r.summary ? `Reachable · ${r.summary.total} containers, ${r.summary.stacks} stacks` : "Reachable", fp: r.host_key_fingerprint }
        : { ok: false, msg: r.error || "unreachable", fp: r.host_key_fingerprint });
    } catch (e) { setTest({ ok: false, msg: (e as Error).message }); }
    finally { setBusy(false); }
  };

  const save = async () => {
    setBusy(true);
    try {
      if (edit) await api.updateNode(edit.id, { name, cluster, transport, address, ...creds() });
      else await api.addNode({ name, cluster, transport, address, ...creds() });
      onAdded(); onClose();
    } catch (e) { setTest({ ok: false, msg: (e as Error).message }); }
    finally { setBusy(false); }
  };

  return (
    <Modal
      open={open} onClose={onClose} title={edit ? "Edit Node" : "Connect New Node"}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>Cancel</Button>
          <Button variant="secondary" onClick={doTest} disabled={busy || !address}>Test Connection</Button>
          <Button variant="primary" onClick={save} disabled={busy || !name || !address}>{edit ? "Save Changes" : "Add Node"}</Button>
        </>
      }
    >
      <div className="space-y-4">
        <div className="grid grid-cols-2 gap-3">
          <div><Label>Name</Label><Input value={name} onChange={(e) => setName(e.target.value)} placeholder="prod-east" /></div>
          {/* F104: a picker over the registered clusters instead of free text —
              typing a new name still creates one, but an existing cluster is now
              a click rather than a spelling test. */}
          <ClusterSelect value={cluster} onChange={setCluster} reloadKey={open} />
        </div>
        <div>
          <Label>Transport</Label>
          <Select value={transport} onChange={(e) => changeTransport(e.target.value)}>
            {transports.map((t) => <option key={t.v} value={t.v}>{t.label}</option>)}
          </Select>
          {switchingTransport && (
            <div className="mt-2 flex items-start gap-2 rounded border border-warning/40 bg-warning/10 px-3 py-2 text-xs text-warning">
              <AlertTriangle size={13} className="mt-0.5 shrink-0" />
              <span>You're changing this node's connection method (was <span className="font-mono">{edit?.transport}</span>). Enter fresh credentials for the new method below — the old ones don't carry over.</span>
            </div>
          )}
        </div>
        <div>
          <Label>Address (DOCKER_HOST)</Label>
          <Input value={address} onChange={(e) => setAddress(e.target.value)} placeholder={placeholder} />
          {transport === "ssh" && (
            <p className="mt-1 text-xs text-on-surface-variant">
              Include the SSH port if it isn't 22 — e.g. <span className="font-mono">ssh://user@host:2222</span>
            </p>
          )}
          {(transport === "tcp-proxy" || transport === "local-proxy") && <ComposeSnippet />}
        </div>
        {transport === "mtls" && (
          <div>
            <Label>TLS bundle: CA --- CERT --- KEY</Label>
            <textarea
              value={secret} onChange={(e) => setSecret(e.target.value)} rows={4}
              className="w-full rounded border border-outline-variant bg-surface-lowest px-3 py-2 font-mono text-xs outline-none focus:border-docker-blue"
              placeholder={credEdit ? "Leave blank to keep the stored credential" : "CA PEM\\n---\\nCERT PEM\\n---\\nKEY PEM"}
            />
          </div>
        )}
        {transport === "ssh" && (
          <>
            <div>
              <Label>Authentication</Label>
              <div className="flex gap-1 rounded-lg border border-outline-variant p-1">
                {([["key", "Private key"], ["password", "Password"]] as const).map(([m, label]) => (
                  <button
                    key={m} type="button" onClick={() => { setSshAuth(m); setTest(null); }}
                    className={`flex-1 rounded-md px-3 py-1.5 text-sm font-medium transition-colors ${sshAuth === m ? "bg-primary/15 text-primary" : "text-on-surface-variant hover:text-on-surface"}`}
                  >{label}</button>
                ))}
              </div>
            </div>
            {sshAuth === "key" ? (
              <>
                <div>
                  <Label>SSH private key (PEM)</Label>
                  <textarea
                    value={secret} onChange={(e) => setSecret(e.target.value)} rows={4}
                    className="w-full rounded border border-outline-variant bg-surface-lowest px-3 py-2 font-mono text-xs outline-none focus:border-docker-blue"
                    placeholder={credEdit ? "Leave blank to keep the stored key" : "-----BEGIN OPENSSH PRIVATE KEY-----"}
                  />
                </div>
                <div>
                  <Label>Key passphrase (if the key is encrypted)</Label>
                  <Input
                    type="password" value={passphrase} onChange={(e) => setPassphrase(e.target.value)}
                    placeholder={credEdit ? "Leave blank to keep the stored passphrase" : "passphrase for an encrypted SSH key"}
                  />
                </div>
              </>
            ) : (
              <div>
                <Label>SSH password</Label>
                <Input
                  type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="off"
                  placeholder={credEdit ? "Leave blank to keep the stored password" : "password for the SSH user"}
                />
                <p className="mt-1 text-xs text-on-surface-variant">
                  The username comes from the address above (<span className="font-mono">ssh://user@host</span>). A key is more secure than a password where you can use one — either way, <b>verify the host fingerprint</b> below.
                </p>
              </div>
            )}
            <p className="text-xs text-on-surface-variant">Credentials are encrypted at rest and remembered across restarts/rebuilds.</p>
          </>
        )}
        {test && (
          <div className={`rounded px-3 py-2 text-sm ${test.ok ? "bg-success/10 text-success" : "bg-error/10 text-error"}`}>
            {test.ok ? "✓ " : "✗ "}{test.msg}
          </div>
        )}
        {/* Host-key fingerprint — shown whenever the handshake reached it (even on an
            auth failure), so the user can verify the server before trusting it. This
            is what protects the connection, and any password, from a man-in-the-middle. */}
        {transport === "ssh" && test?.fp && (
          <div className="rounded border border-outline-variant bg-surface-lowest px-3 py-2 text-xs">
            <div className="font-medium text-on-surface">Host key fingerprint — verify this matches your server</div>
            <div className="mt-1 break-all font-mono text-on-surface">{test.fp}</div>
            <div className="mt-1 text-on-surface-variant">Pinned on first connect; a later change is refused until you reset the pinned key in the node's settings.</div>
          </div>
        )}
      </div>
    </Modal>
  );
}
