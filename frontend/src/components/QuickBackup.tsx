// "Backup Now" quick-pick (Fable-UI-UX A2). The sidebar's headline button used to
// just navigate to /backups; now it opens a popover of the operator's FAVORITE
// backup targets (stacks or containers) and fires each with its saved defaults in
// one click. Backups run with the container's saved policy/destinations
// server-side (we omit destinations, so DestinationsExplicit=false → policy
// applies).
//
// F222 fixed two things about the list itself.
//
// A container favorite stored the container ID, so `docker compose up -d`
// silently broke it: the id it held no longer existed, and the click failed with
// a generic error. It now stores the NAME, which is what the rest of this app has
// always keyed on for exactly this reason — schedules, per-container options,
// pause modes are all name-keyed because ids churn.
//
// And the list lived only in localStorage, so a second browser started empty. It
// now lives in a settings row on the server. localStorage stays as the cache that
// paints the menu instantly before the server answers.
import { useCallback, useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import {
  CloudUpload, ChevronDown, Layers, Box, Pencil, Plus, Loader2,
  Check, AlertTriangle, ScrollText, Server as ServerIcon, X,
} from "lucide-react";
import { api, Container, Favorite, Node, Stack } from "../api";
import { Button, Modal } from "./ui";
import BackupCloudIcon from "./BackupCloudIcon";

export type { Favorite };

const FAV_KEY = "dback.backup_favorites";
const favKey = (f: { kind: string; nodeId: string; ref: string }) => `${f.kind}:${f.nodeId}:${f.ref}`;

// legacyIDRef spots a favorite still holding a container id rather than a name.
// Docker ids are hex, at least 12 characters; a container name that is also 12+
// hex characters and nothing else is possible but not something anybody types.
// The migration below only rewrites a ref that ALSO matches a live container's
// id, so a false positive here costs nothing.
const legacyIDRef = (ref: string) => /^[0-9a-f]{12,64}$/.test(ref);

function loadCachedFavorites(): Favorite[] {
  try {
    const raw = localStorage.getItem(FAV_KEY);
    const arr = raw ? JSON.parse(raw) : [];
    return Array.isArray(arr) ? arr.filter((f) => f && f.kind && f.nodeId && f.ref) : [];
  } catch { return []; }
}
function cacheFavorites(list: Favorite[]) {
  try { localStorage.setItem(FAV_KEY, JSON.stringify(list)); } catch { /* quota/private mode — best effort */ }
}

type RunState = "idle" | "starting" | "started" | "error";

export default function QuickBackup() {
  const navigate = useNavigate();
  const [open, setOpen] = useState(false);
  const [editing, setEditing] = useState(false);
  // Painted from the local cache immediately, then reconciled with the server —
  // the menu must not wait on a round trip to show a list it already knows.
  const [favorites, setFavorites] = useState<Favorite[]>(loadCachedFavorites);
  const [runState, setRunState] = useState<Record<string, RunState>>({});
  const [runErr, setRunErr] = useState<Record<string, string>>({});
  const [anyStarted, setAnyStarted] = useState(false);
  const [migrating, setMigrating] = useState(false);
  const migrated = useRef(false);
  const wrapRef = useRef<HTMLDivElement>(null);

  const persist = useCallback((list: Favorite[]) => {
    setFavorites(list);
    cacheFavorites(list);
    // Best-effort: a preference that failed to reach the server is still correct
    // in this browser, and failing the interaction over it would be worse.
    api.saveFavorites(list).catch(() => {});
  }, []);

  // Reconcile with the server once per app load. The server is authoritative —
  // that is what makes the list the same on the laptop and the desktop — except
  // when it has nothing and this browser does, which is the one-time upgrade
  // from the localStorage-only era.
  useEffect(() => {
    let alive = true;
    api.getFavorites().then((r) => {
      if (!alive) return;
      const remote = r.favorites || [];
      if (remote.length === 0) {
        const local = loadCachedFavorites();
        if (local.length > 0) { api.saveFavorites(local).catch(() => {}); return; }
      }
      setFavorites(remote);
      cacheFavorites(remote);
    }).catch(() => { /* offline or unauthenticated — the cache still paints */ });
    return () => { alive = false; };
  }, []);

  // AC3 — rewrite favorites still holding a container ID to the container's NAME.
  //
  // Deferred to the first time the menu is OPENED, never done on mount: this
  // component is in the sidebar of every page, and a migration nobody asked for
  // must not cost a fleet-wide container listing on page load. Until it runs,
  // runOne still sends an id-shaped ref as an id, so a legacy favorite keeps
  // working exactly as it did.
  const migrateLegacyRefs = useCallback(async () => {
    if (migrated.current) return;
    const stale = favorites.filter((f) => f.kind === "container" && legacyIDRef(f.ref));
    if (stale.length === 0) { migrated.current = true; return; }
    migrated.current = true;
    setMigrating(true);
    try {
      const nodeIDs = Array.from(new Set(stale.map((f) => f.nodeId)));
      const byNode = new Map<string, Container[]>();
      await Promise.all(nodeIDs.map(async (nid) => {
        try { byNode.set(nid, (await api.containersPage(nid, { page_size: 500 })).containers || []); }
        catch { /* node down: leave its favorites as they are, and try again next session */ }
      }));
      let changed = false;
      const next = favorites.map((f) => {
        if (f.kind !== "container" || !legacyIDRef(f.ref)) return f;
        const hit = (byNode.get(f.nodeId) || []).find((c) => c.id === f.ref || c.id.startsWith(f.ref));
        if (!hit) return f; // NEVER dropped — the editor flags it instead
        changed = true;
        return { ...f, ref: hit.name, name: hit.name };
      });
      if (changed) persist(next);
      else if (byNode.size < nodeIDs.length) migrated.current = false; // a node was down; retry later
    } finally { setMigrating(false); }
  }, [favorites, persist]);

  useEffect(() => { if (open) void migrateLegacyRefs(); }, [open, migrateLegacyRefs]);

  // Close the popover on any outside click / Escape (matches the Dashboard menu).
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => { if (wrapRef.current && !wrapRef.current.contains(e.target as HTMLElement)) setOpen(false); };
    const onKey = (e: KeyboardEvent) => { if (e.key === "Escape") setOpen(false); };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    return () => { document.removeEventListener("mousedown", onDown); document.removeEventListener("keydown", onKey); };
  }, [open]);

  const runOne = async (f: Favorite) => {
    const k = favKey(f);
    setRunState((s) => ({ ...s, [k]: "starting" }));
    setRunErr((e) => ({ ...e, [k]: "" }));
    try {
      if (f.kind === "stack") await api.backupStack(f.nodeId, f.ref);
      // F222: by NAME, so the favorite survives a recreate. A ref that still
      // looks like an id is a favorite the migration has not reached yet (an
      // offline node, a menu never opened) — sent as an id, exactly as before,
      // so nothing regresses while it waits.
      else if (legacyIDRef(f.ref)) await api.createBackup(f.nodeId, f.ref, false);
      else await api.createBackupByName(f.nodeId, f.ref); // saved defaults (policy destinations, all mounts)
      setRunState((s) => ({ ...s, [k]: "started" }));
      setAnyStarted(true);
    } catch (err) {
      setRunState((s) => ({ ...s, [k]: "error" }));
      setRunErr((e) => ({ ...e, [k]: err instanceof Error ? err.message : "Failed to start" }));
    }
  };

  const runAll = async () => { for (const f of favorites) await runOne(f); };

  const onSaved = (list: Favorite[]) => { persist(list); setEditing(false); };

  const viewLogs = () => { setOpen(false); navigate("/logs"); };

  return (
    <div className="relative" ref={wrapRef}>
      <Button
        variant="primary" className="w-full"
        aria-haspopup="menu" aria-expanded={open}
        onClick={() => setOpen((o) => !o)}
      >
        <CloudUpload size={15} /> Backup Now <ChevronDown size={14} className={`ml-auto transition-transform ${open ? "rotate-180" : ""}`} />
      </Button>

      {open && (
        <div className="absolute bottom-full left-0 z-40 mb-2 w-72 overflow-hidden rounded-lg border border-outline-variant bg-surface-high shadow-2xl">
          <div className="flex items-center gap-2 border-b border-outline-variant/60 px-3 py-2">
            <span className="text-[11px] font-semibold uppercase tracking-[0.08em] text-on-surface-variant">Favorite backups</span>
            {/* F222: the one-time id→name rewrite, which only runs when there is
                something to rewrite. Shown because it briefly costs a fetch. */}
            {migrating && <Loader2 size={12} className="animate-spin text-on-surface-variant" aria-label="Updating favorites" />}
            <button onClick={() => setEditing(true)} title="Edit favorites" className="ml-auto flex items-center gap-1 rounded px-1.5 py-1 text-xs text-on-surface-variant hover:bg-surface-highest hover:text-on-surface">
              <Pencil size={13} /> Edit
            </button>
          </div>

          {favorites.length === 0 ? (
            <div className="px-3 py-6 text-center">
              <p className="mb-3 text-sm text-on-surface-variant">No favorites yet. Pick the stacks and containers you back up most.</p>
              <Button variant="secondary" className="mx-auto" onClick={() => setEditing(true)}><Plus size={15} /> Create favorite list</Button>
            </div>
          ) : (
            <>
              <div className="max-h-72 overflow-y-auto py-1">
                {favorites.map((f) => {
                  const k = favKey(f);
                  const st = runState[k] ?? "idle";
                  return (
                    <button
                      key={k} onClick={() => runOne(f)} disabled={st === "starting"}
                      className="flex w-full items-center gap-2.5 px-3 py-2 text-left hover:bg-surface-highest/60 disabled:opacity-60"
                      title={`Back up ${f.name} now`}
                    >
                      <span className="text-on-surface-variant">{f.kind === "stack" ? <Layers size={16} /> : <Box size={16} />}</span>
                      <span className="min-w-0 flex-1">
                        <span className="block truncate text-sm text-on-surface">{f.name}</span>
                        <span className="block truncate text-xs text-on-surface-variant">{f.kind === "stack" ? "stack" : "container"} · {f.nodeName}</span>
                      </span>
                      {st === "starting" && <BackupCloudIcon size={15} active className="shrink-0 text-secondary" />}
                      {st === "started" && <Check size={15} className="shrink-0 text-success" />}
                      {st === "error" && <span className="shrink-0" title={runErr[k]}><AlertTriangle size={15} className="text-error" /></span>}
                    </button>
                  );
                })}
              </div>
              <div className="flex items-center gap-2 border-t border-outline-variant/60 px-3 py-2">
                {favorites.length > 1 && (
                  <button onClick={runAll} className="flex items-center gap-1.5 rounded px-2 py-1 text-xs font-medium text-primary hover:bg-docker-blue/10">
                    <CloudUpload size={13} /> Back up all
                  </button>
                )}
                {anyStarted && (
                  <button onClick={viewLogs} className="ml-auto flex items-center gap-1.5 rounded px-2 py-1 text-xs font-medium text-on-surface-variant hover:text-on-surface">
                    <ScrollText size={13} /> View live logs
                  </button>
                )}
              </div>
            </>
          )}
        </div>
      )}

      <FavoritesEditor open={editing} current={favorites} onClose={() => setEditing(false)} onSave={onSaved} />
    </div>
  );
}

// FavoritesEditor: a node-grouped checklist of every stack and container across
// the fleet, used to build/edit the favorite list. Fetches live data only when
// opened (no per-page cost).
function FavoritesEditor({ open, current, onClose, onSave }: {
  open: boolean; current: Favorite[]; onClose: () => void; onSave: (list: Favorite[]) => void;
}) {
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState(false);
  const [nodes, setNodes] = useState<{ node: Node; containers: Container[]; stacks: Stack[] }[]>([]);
  const [sel, setSel] = useState<Map<string, Favorite>>(new Map());

  useEffect(() => {
    if (!open) return;
    setSel(new Map(current.map((f) => [favKey(f), f])));
    setLoading(true); setErr(false);
    let alive = true;
    (async () => {
      try {
        const ns = await api.nodes();
        const per = await Promise.all(ns.map((n) =>
          api.containersPage(n.id, { page_size: 500 })
            .then((p) => ({ node: n, containers: p.containers, stacks: p.stacks }))
            .catch(() => ({ node: n, containers: [] as Container[], stacks: [] as Stack[] })),
        ));
        if (alive) { setNodes(per); setLoading(false); }
      } catch { if (alive) { setErr(true); setLoading(false); } }
    })();
    return () => { alive = false; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const toggle = (f: Favorite, on: boolean) =>
    setSel((prev) => { const m = new Map(prev); on ? m.set(favKey(f), f) : m.delete(favKey(f)); return m; });

  const isOn = (kind: Favorite["kind"], nodeId: string, ref: string) => sel.has(`${kind}:${nodeId}:${ref}`);

  // Favorites whose target is not in the fleet listing — surfaced rather than
  // dropped. Only computed once the listing has actually loaded, or every
  // favorite would be "unresolved" for as long as the fetch takes.
  const unresolved = loading || err || nodes.length === 0 ? [] : current.filter((f) => {
    const n = nodes.find((x) => x.node.id === f.nodeId);
    if (!n) return false; // a node that is not in the fleet at all is a different problem
    return f.kind === "stack"
      ? !n.stacks.some((st) => st.name === f.ref)
      : !n.containers.some((c) => c.name === f.ref);
  });

  return (
    <Modal
      open={open} onClose={onClose} title="Edit favorite backups"
      footer={<>
        <Button onClick={onClose}>Cancel</Button>
        <Button variant="primary" onClick={() => onSave([...sel.values()])}><Check size={16} /> Save favorites ({sel.size})</Button>
      </>}
    >
      <p className="mb-3 text-sm text-on-surface-variant">Tick the stacks and containers you back up most often. They'll appear in the Backup Now menu for one-click backups with each target's saved settings.</p>

      {/* AC3 — a favorite whose container could not be found is SHOWN, never
          quietly dropped. It is either on a node that is down right now, or it
          points at a container that has been renamed or removed; the operator is
          the only one who can tell which, so the row stays until they say. */}
      {unresolved.length > 0 && (
        <div className="mb-3 rounded border border-warning/40 bg-warning/10 p-3">
          <div className="flex items-center gap-2 text-sm font-semibold text-warning">
            <AlertTriangle size={15} className="shrink-0" /> {unresolved.length} favorite{unresolved.length === 1 ? "" : "s"} could not be matched to a container
          </div>
          <p className="mt-1 break-words text-xs text-on-surface-variant">
            These were saved before favorites followed container names. Their container is not in the list below &mdash; it may have been renamed or removed, or its node may be offline right now. They are kept as they are; untick one to drop it.
          </p>
          <div className="mt-2 space-y-1">
            {unresolved.map((f) => (
              <label key={favKey(f)} className="flex cursor-pointer flex-wrap items-center gap-2 rounded border border-outline-variant bg-surface-low px-3 py-1.5 text-sm">
                <input type="checkbox" checked={sel.has(favKey(f))} onChange={(e) => toggle(f, e.target.checked)} />
                <Box size={15} className="shrink-0 text-on-surface-variant" />
                <span className="min-w-0 break-all">{f.name}</span>
                <span className="ml-auto shrink-0 text-xs text-on-surface-variant">{f.nodeName || f.nodeId}</span>
              </label>
            ))}
          </div>
        </div>
      )}

      {loading ? (
        <div className="flex items-center justify-center gap-2 py-10 text-on-surface-variant"><Loader2 size={16} className="animate-spin" /> Loading nodes…</div>
      ) : err ? (
        <div className="py-10 text-center text-error">Couldn't load nodes. Close and try again.</div>
      ) : nodes.length === 0 ? (
        <div className="py-10 text-center text-on-surface-variant">No nodes connected yet.</div>
      ) : (
        <div className="space-y-4">
          {nodes.map(({ node, containers, stacks }) => (
            <div key={node.id}>
              <div className="mb-1.5 flex items-center gap-2">
                <ServerIcon size={14} className="text-on-surface-variant" />
                <span className="text-sm font-semibold">{node.name}</span>
                {!node.reachable && <span className="flex items-center gap-1 text-xs text-error"><X size={11} /> offline</span>}
              </div>

              {stacks.length > 0 && (
                <div className="mb-2">
                  <div className="mb-1 px-1 text-[10px] font-semibold uppercase tracking-wider text-on-surface-variant/70">Stacks</div>
                  <div className="space-y-1">
                    {stacks.map((s) => (
                      <label key={"s" + s.name} className="flex cursor-pointer items-center gap-2.5 rounded border border-outline-variant bg-surface-low px-3 py-1.5 hover:bg-surface-highest">
                        <input type="checkbox" checked={isOn("stack", node.id, s.name)} onChange={(e) => toggle({ kind: "stack", nodeId: node.id, nodeName: node.name, ref: s.name, name: s.name }, e.target.checked)} />
                        <Layers size={15} className="text-on-surface-variant" />
                        <span className="text-sm">{s.name}</span>
                        <span className="ml-auto text-xs text-on-surface-variant">{s.running}/{s.total}</span>
                      </label>
                    ))}
                  </div>
                </div>
              )}

              {containers.length > 0 && (
                <div>
                  <div className="mb-1 px-1 text-[10px] font-semibold uppercase tracking-wider text-on-surface-variant/70">Containers</div>
                  <div className="space-y-1">
                    {containers.map((c) => (
                      <label key={"c" + c.id} className="flex cursor-pointer items-center gap-2.5 rounded border border-outline-variant bg-surface-low px-3 py-1.5 hover:bg-surface-highest">
                        {/* F222: keyed by NAME, not id — an id dies on every
                            `docker compose up -d` and took the favorite with it. */}
                        <input type="checkbox" checked={isOn("container", node.id, c.name)} onChange={(e) => toggle({ kind: "container", nodeId: node.id, nodeName: node.name, ref: c.name, name: c.name }, e.target.checked)} />
                        <Box size={15} className="text-on-surface-variant" />
                        <span className="min-w-0 truncate text-sm">{c.name}</span>
                        {c.stack && <span className="ml-auto shrink-0 text-xs text-on-surface-variant">{c.stack}</span>}
                      </label>
                    ))}
                  </div>
                </div>
              )}
            </div>
          ))}
        </div>
      )}
    </Modal>
  );
}
