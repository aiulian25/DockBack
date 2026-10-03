// Settings -> Clusters (F104). The registry that turns a cluster from a typed
// string into a real object: named, described, coloured, renameable, deletable —
// and the owner of one tier of the backup-policy chain.
//
// The delete flow is the part written most carefully, because it is the one a
// user has to trust. Removing a cluster removes a GROUPING: no node is
// disconnected, no container touched, no backup or archive deleted. A cluster
// with nodes cannot be removed until you say where those nodes go, and the
// dialog states all of this before the button is live.
import { useEffect, useMemo, useState } from "react";
import {
  Layers, Plus, Trash2, Pencil, Loader2, CheckCircle2, AlertTriangle,
  CloudUpload, HardDrive, ChevronDown, ChevronRight, Server, Wifi, WifiOff,
} from "lucide-react";
import { api, Cluster, ClusterMember, ClusterPolicy, Destination, PolicyOverride } from "../api";
import { Button, Card, Input, Label, Modal, Select } from "./ui";
import { CLUSTER_ACCENTS, clusterAccent } from "./ClusterBar";
import { useToast } from "./Toast";

// The identity ramp offered in the colour picker: the shared muted set, plus ""
// for "pick one for me". Disjoint from the reserved status palette (green/amber/
// red) so a cluster's colour can never be mistaken for a health signal.
const SWATCHES = ["", ...CLUSTER_ACCENTS];

function ColorPicker({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      {SWATCHES.map((c) => {
        const active = (value || "") === c;
        return (
          <button
            key={c || "auto"}
            type="button"
            onClick={() => onChange(c)}
            aria-label={c ? `Colour ${c}` : "Automatic colour"}
            aria-pressed={active}
            title={c || "Automatic"}
            className={`h-6 w-9 border transition-colors ${active ? "border-primary ring-1 ring-primary/50" : "border-outline-variant"}`}
            style={c ? { background: c } : undefined}
          >
            {!c && <span className="text-[9px] font-bold uppercase text-outline">Auto</span>}
          </button>
        );
      })}
    </div>
  );
}

// ---------------------------------------------------------------- delete flow

function DeleteClusterModal({ cluster, others, onClose, onDone }: {
  cluster: Cluster;
  others: Cluster[];
  onClose: () => void;
  onDone: () => void;
}) {
  const toast = useToast();
  const [target, setTarget] = useState(others[0]?.name || "");
  const [busy, setBusy] = useState(false);
  const blocked = cluster.nodes > 0 && others.length === 0;

  const remove = async () => {
    setBusy(true);
    try {
      const r = await api.deleteCluster(cluster.name, cluster.nodes > 0 ? target : undefined);
      toast.success(r.nodes_reassigned > 0
        ? `Removed “${cluster.name}” · ${r.nodes_reassigned} node${r.nodes_reassigned === 1 ? "" : "s"} moved to “${target}”`
        : `Removed “${cluster.name}”`);
      onDone(); onClose();
    } catch (e) {
      toast.error((e as Error).message);
    } finally { setBusy(false); }
  };

  return (
    <Modal
      open onClose={onClose} title={`Remove cluster “${cluster.name}”?`}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>Cancel</Button>
          <Button variant="danger" onClick={remove} disabled={busy || blocked || (cluster.nodes > 0 && !target)}>
            {busy ? <Loader2 size={15} className="animate-spin" /> : <Trash2 size={15} />} Remove cluster
          </Button>
        </>
      }
    >
      <div className="space-y-4 text-sm">
        {/* State what is NOT destroyed first — it is the question every user has
            at this moment, and burying it under a warning icon reads as danger. */}
        <div className="rounded border border-success/25 bg-success/[0.07] p-3">
          <div className="mb-1 flex items-center gap-2 font-semibold text-success">
            <CheckCircle2 size={15} /> Nothing is deleted
          </div>
          <p className="text-on-surface-variant">
            A cluster is a grouping of servers. Removing it does <b className="text-on-surface">not</b> delete or
            disconnect any server, container, volume, backup or archive. Your backup history and every copy on every
            destination stay exactly as they are.
          </p>
        </div>

        <div>
          <div className="mb-1 font-semibold">What does change</div>
          <ul className="list-inside list-disc space-y-1 text-on-surface-variant">
            <li>The cluster disappears from the dashboard grouping and filters.</li>
            <li>
              Any <b className="text-on-surface">backup policy set on this cluster</b> is removed. Its servers fall back
              to the global policy — unless they have their own per-server or per-container override, which is untouched.
            </li>
            {cluster.nodes > 0 && <li>Its {cluster.nodes} server{cluster.nodes === 1 ? "" : "s"} move to the cluster you pick below.</li>}
          </ul>
        </div>

        {cluster.nodes > 0 && (
          blocked ? (
            <div className="flex items-start gap-2 rounded border border-warning/30 bg-warning/[0.08] p-3 text-warning">
              <AlertTriangle size={15} className="mt-0.5 shrink-0" />
              <span>
                This is your only cluster and it still has {cluster.nodes} server{cluster.nodes === 1 ? "" : "s"}.
                Create another cluster first, or move these servers to it — servers always belong to a cluster.
              </span>
            </div>
          ) : (
            <div>
              <Label>Move its {cluster.nodes} server{cluster.nodes === 1 ? "" : "s"} to</Label>
              <Select value={target} onChange={(e) => setTarget(e.target.value)}>
                {others.map((c) => <option key={c.name} value={c.name}>{c.name}</option>)}
              </Select>
              <p className="mt-1 text-xs text-outline">
                The move and the removal happen together, so a server is never left pointing at a cluster that no longer exists.
              </p>
            </div>
          )
        )}
      </div>
    </Modal>
  );
}

// ------------------------------------------------------------------ members

// A cluster's servers, and the means to move more in. This is the answer to
// "I made a cluster — now how do I put servers in it?": assignment used to live
// only on each server's own Edit form, which is the wrong place to look for it.
//
// A server is never CREATED here. Clusters group servers you have already
// connected; adding a new machine is still Dashboard -> Connect New Node,
// because that is where a transport and a credential are supplied.
function ClusterMembers({ name, onChanged }: { name: string; onChanged: () => void }) {
  const toast = useToast();
  const [members, setMembers] = useState<ClusterMember[] | null>(null);
  const [available, setAvailable] = useState<ClusterMember[]>([]);
  const [picking, setPicking] = useState(false);
  const [sel, setSel] = useState<Set<string>>(new Set());
  const [busy, setBusy] = useState(false);

  const load = () =>
    api.clusterMembers(name)
      .then((r) => { setMembers(r.members); setAvailable(r.available); })
      .catch(() => setMembers([]));
  useEffect(() => { load(); /* eslint-disable-next-line react-hooks/exhaustive-deps */ }, [name]);

  const toggle = (id: string) =>
    setSel((prev) => {
      const n = new Set(prev);
      if (n.has(id)) n.delete(id); else n.add(id);
      return n;
    });

  const assign = async () => {
    setBusy(true);
    try {
      const r = await api.assignClusterMembers(name, [...sel]);
      toast.success(`${r.moved} server${r.moved === 1 ? "" : "s"} moved to “${name}”`);
      setSel(new Set()); setPicking(false);
      await load(); onChanged();
    } catch (e) { toast.error((e as Error).message); }
    finally { setBusy(false); }
  };

  return (
    <div className="rounded border border-outline-variant bg-surface-lowest p-4">
      <div className="mb-1 flex items-center gap-2 text-sm font-semibold">
        <Server size={15} className="text-primary" /> Servers in this cluster
      </div>
      <p className="mb-3 text-xs text-on-surface-variant">
        Clusters group servers you have already connected. To add a new machine, use
        <b className="text-on-surface"> Dashboard → Connect New Node</b> — that is where its address and credentials are set.
      </p>

      {members === null ? (
        <div className="text-sm text-on-surface-variant">Loading servers…</div>
      ) : members.length === 0 ? (
        <div className="rounded border border-dashed border-outline-variant px-3 py-4 text-center text-sm text-on-surface-variant">
          No servers in this cluster yet.
        </div>
      ) : (
        <div className="space-y-1.5">
          {members.map((m) => (
            <div key={m.id} className="flex items-center gap-2 rounded border border-outline-variant bg-surface px-3 py-1.5 text-sm">
              {m.reachable
                ? <Wifi size={13} className="shrink-0 text-success" aria-label="Reachable" />
                : <WifiOff size={13} className="shrink-0 text-error" aria-label="Unreachable" />}
              <span className="min-w-0 truncate font-medium">{m.name}</span>
              <span className="ml-auto min-w-0 shrink truncate font-mono text-[11px] text-outline">{m.address}</span>
            </div>
          ))}
        </div>
      )}

      <div className="mt-3">
        {picking ? (
          <div className="rounded border border-outline-variant bg-surface p-3">
            <div className="mb-2 text-xs font-semibold uppercase tracking-wider text-on-surface-variant">
              Move into “{name}”
            </div>
            <div className="max-h-48 space-y-1.5 overflow-auto">
              {available.map((m) => (
                <label key={m.id} className="flex cursor-pointer items-center gap-2 rounded border border-outline-variant bg-surface-lowest px-3 py-1.5 text-sm hover:bg-surface-high">
                  <input type="checkbox" checked={sel.has(m.id)} onChange={() => toggle(m.id)} />
                  <span className="min-w-0 truncate font-medium">{m.name}</span>
                  <span className="ml-auto shrink-0 text-[11px] text-outline">currently in {m.cluster}</span>
                </label>
              ))}
            </div>
            <p className="mt-2 text-[11px] text-outline">
              Moving a server changes only which cluster it belongs to — its connection, credentials and backups are untouched.
            </p>
            <div className="mt-3 flex items-center gap-2">
              <Button variant="primary" size="sm" onClick={assign} disabled={busy || sel.size === 0}>
                {busy ? <Loader2 size={14} className="animate-spin" /> : <CheckCircle2 size={14} />} Move {sel.size || ""}
              </Button>
              <Button variant="ghost" size="sm" onClick={() => { setPicking(false); setSel(new Set()); }}>Cancel</Button>
            </div>
          </div>
        ) : available.length > 0 ? (
          <Button variant="secondary" size="sm" onClick={() => setPicking(true)}>
            <Plus size={14} /> Move servers here
          </Button>
        ) : members !== null && (
          <p className="text-xs text-on-surface-variant">Every server in the fleet is already in this cluster.</p>
        )}
      </div>
    </div>
  );
}

// -------------------------------------------------------------- policy editor

function ClusterPolicyEditor({ name, dests, onSaved }: {
  name: string;
  dests: Destination[];
  onSaved: () => void;
}) {
  const toast = useToast();
  const [cp, setCp] = useState<ClusterPolicy | null>(null);
  const [ov, setOv] = useState<PolicyOverride | null>(null);
  const [saving, setSaving] = useState(false);

  // Normalize so `destinations` is never null: the API returns a nil slice as
  // JSON null when no override exists, which would crash .includes/.filter.
  const norm = (p: ClusterPolicy): ClusterPolicy => ({
    ...p,
    override: { ...p.override, destinations: p.override?.destinations ?? [] },
    global: { ...p.global, destinations: p.global?.destinations ?? [] },
    resolved: { ...p.resolved, destinations: p.resolved?.destinations ?? [] },
  });

  useEffect(() => {
    api.getClusterPolicy(name).then((p) => { const n = norm(p); setCp(n); setOv(n.override); }).catch(() => {});
  }, [name]);

  if (!cp || !ov) return <div className="p-4 text-sm text-on-surface-variant">Loading policy…</div>;
  const g = cp.global;
  const set = (patch: Partial<PolicyOverride>) => setOv({ ...ov, ...patch });
  const dirty = JSON.stringify(ov) !== JSON.stringify(cp.override);

  const destName = (id: string) => dests.find((d) => d.id === id)?.name || id;
  const inheritedDests = g.destinations.length ? g.destinations.map(destName).join(", ") : "Local only";

  const toggleDests = (on: boolean) =>
    set(on
      ? { override_destinations: true, destinations: ov.destinations.length ? ov.destinations : g.destinations }
      : { override_destinations: false });
  const toggleRetention = (on: boolean) =>
    set(on
      ? {
        override_retention: true, generations: g.generations || 0, keep_daily: g.keep_daily || 0,
        keep_weekly: g.keep_weekly || 0, keep_monthly: g.keep_monthly || 0, keep_yearly: g.keep_yearly || 0,
        autoprune: g.autoprune,
      }
      : { override_retention: false });
  const toggleDest = (id: string, checked: boolean) => {
    const base = ov.override_destinations ? ov.destinations : g.destinations;
    set({ override_destinations: true, destinations: checked ? [...base, id] : base.filter((x) => x !== id) });
  };

  const save = async () => {
    setSaving(true);
    try {
      const n = norm(await api.setClusterPolicy(name, ov));
      setCp(n); setOv(n.override);
      toast.success(`Policy saved for “${name}”`);
      onSaved();
    } catch (e) { toast.error((e as Error).message); }
    finally { setSaving(false); }
  };

  const numField = (label: string, val: number, key: keyof PolicyOverride) => (
    <label className="flex items-center justify-between gap-2 text-sm">
      <span className="text-on-surface-variant">{label}</span>
      <input
        type="number" min={0} value={val}
        onChange={(e) => set({ [key]: Math.max(0, parseInt(e.target.value || "0", 10)) } as Partial<PolicyOverride>)}
        className="w-20 shrink-0 rounded border border-outline-variant bg-surface-lowest px-2 py-1 text-right text-on-surface outline-none focus:border-docker-blue"
      />
    </label>
  );

  return (
    <div>
      <p className="mb-3 text-sm text-on-surface-variant">
        Applies to every server in this cluster. A server or container with its own override still wins —
        most specific first: <b className="text-on-surface">container → server → cluster → global</b>.
      </p>
      <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
        <div className="rounded border border-outline-variant bg-surface-lowest p-4">
          <label className="flex cursor-pointer items-center gap-2 text-sm font-medium">
            <input type="checkbox" checked={ov.override_destinations} onChange={(e) => toggleDests(e.target.checked)} />
            Override destinations
          </label>
          <div className="mt-3 space-y-2">
            <div className="flex items-center gap-2 rounded border border-outline-variant bg-surface px-3 py-1.5 text-sm text-on-surface-variant">
              <HardDrive size={14} /> Local <span className="ml-auto text-xs">always</span>
            </div>
            {dests.map((d) => {
              const checked = ov.override_destinations ? ov.destinations.includes(d.id) : g.destinations.includes(d.id);
              return (
                <label key={d.id} className="flex cursor-pointer items-center gap-2 rounded border border-outline-variant bg-surface px-3 py-1.5 text-sm hover:bg-surface-highest">
                  <input type="checkbox" checked={checked} onChange={(e) => toggleDest(d.id, e.target.checked)} />
                  <CloudUpload size={14} className="text-secondary" />
                  <span className="min-w-0 truncate">{d.name}</span>
                  <span className="ml-auto shrink-0 text-xs uppercase text-on-surface-variant">{d.type}</span>
                </label>
              );
            })}
            {dests.length === 0 && <p className="text-xs text-on-surface-variant">No external destinations configured yet.</p>}
            <p className="text-xs italic text-on-surface-variant">
              {ov.override_destinations
                ? "This cluster overrides the global destinations. Untick to go back to inheriting."
                : <>Inheriting global: <span className="not-italic text-on-surface">{inheritedDests}</span>.</>}
            </p>
          </div>
        </div>

        <div className="rounded border border-outline-variant bg-surface-lowest p-4">
          <label className="flex cursor-pointer items-center gap-2 text-sm font-medium">
            <input type="checkbox" checked={ov.override_retention} onChange={(e) => toggleRetention(e.target.checked)} />
            Override retention
          </label>
          {ov.override_retention ? (
            <div className="mt-3 space-y-2">
              {numField("Generations (keep newest N)", ov.generations, "generations")}
              {numField("Daily (GFS)", ov.keep_daily, "keep_daily")}
              {numField("Weekly (GFS)", ov.keep_weekly, "keep_weekly")}
              {numField("Monthly (GFS)", ov.keep_monthly, "keep_monthly")}
              {numField("Yearly (GFS)", ov.keep_yearly, "keep_yearly")}
              <label className="flex cursor-pointer items-center gap-2 pt-1 text-sm">
                <input type="checkbox" checked={ov.autoprune} onChange={(e) => set({ autoprune: e.target.checked })} />
                Auto-prune after each backup
              </label>
            </div>
          ) : (
            <p className="mt-3 text-sm text-on-surface-variant">
              Inherited: <span className="text-on-surface">keep {g.generations || 0}</span>
              {(g.keep_daily || g.keep_weekly || g.keep_monthly || g.keep_yearly)
                ? <span className="text-on-surface"> · GFS {g.keep_daily || 0}/{g.keep_weekly || 0}/{g.keep_monthly || 0}/{g.keep_yearly || 0}</span>
                : null}
              {g.autoprune ? " · auto-prune on" : ""}
            </p>
          )}
        </div>
      </div>
      <div className="mt-4">
        <Button variant="primary" onClick={save} disabled={saving || !dirty}>
          {saving ? <Loader2 size={15} className="animate-spin" /> : <CheckCircle2 size={15} />} Save cluster policy
        </Button>
      </div>
    </div>
  );
}

// ------------------------------------------------------------------- one row

function ClusterRow({ c, others, dests, onChanged }: {
  c: Cluster;
  others: Cluster[];
  dests: Destination[];
  onChanged: () => void;
}) {
  const toast = useToast();
  const [open, setOpen] = useState(false);
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState(c.name);
  const [description, setDescription] = useState(c.description);
  const [color, setColor] = useState(c.color);
  const [saving, setSaving] = useState(false);
  const [confirmDel, setConfirmDel] = useState(false);

  useEffect(() => { setName(c.name); setDescription(c.description); setColor(c.color); }, [c.name, c.description, c.color]);

  const saveIdentity = async () => {
    setSaving(true);
    try {
      await api.updateCluster(c.name, { name: name.trim(), description: description.trim(), color });
      if (name.trim() !== c.name) toast.success(`Renamed to “${name.trim()}” · servers and policy moved with it`);
      else toast.success("Cluster updated");
      setEditing(false); onChanged();
    } catch (e) { toast.error((e as Error).message); }
    finally { setSaving(false); }
  };

  return (
    <div className="border-t border-outline-variant first:border-t-0">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2 px-4 py-3">
        <button
          onClick={() => setOpen((o) => !o)}
          aria-expanded={open}
          className="flex min-w-0 flex-1 items-center gap-2.5 text-left"
        >
          {open ? <ChevronDown size={15} className="shrink-0 text-outline" /> : <ChevronRight size={15} className="shrink-0 text-outline" />}
          <span className="min-w-0">
            {/* Identity colour as a rule under the name — never a swatch. */}
            <span className="block truncate text-sm font-semibold">
              <span className="border-b-2 pb-0.5" style={{ borderColor: clusterAccent(c.name, c.color) }}>{c.name}</span>
            </span>
            {c.description && <span className="block truncate text-xs text-on-surface-variant">{c.description}</span>}
          </span>
        </button>
        <span className="tnum shrink-0 rounded-full bg-surface-highest px-2 py-0.5 text-[11px] text-on-surface-variant">
          {c.nodes} server{c.nodes === 1 ? "" : "s"}
        </span>
        <div className="flex shrink-0 items-center gap-1">
          <Button variant="ghost" size="sm" onClick={() => { setEditing((e) => !e); setOpen(true); }} aria-label={`Edit ${c.name}`}>
            <Pencil size={14} />
          </Button>
          <Button variant="ghost" size="sm" onClick={() => setConfirmDel(true)} aria-label={`Remove ${c.name}`}>
            <Trash2 size={14} />
          </Button>
        </div>
      </div>

      {open && (
        <div className="space-y-5 bg-surface-lowest/40 px-4 pb-5 pt-1">
          {editing && (
            <div className="rounded border border-outline-variant bg-surface-lowest p-4">
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <div>
                  <Label>Name</Label>
                  <Input value={name} onChange={(e) => setName(e.target.value)} />
                  <p className="mt-1 text-[11px] text-outline">
                    Renaming moves its servers and its cluster policy too — nothing is lost.
                  </p>
                </div>
                <div>
                  <Label>Description</Label>
                  <Input value={description} onChange={(e) => setDescription(e.target.value)} placeholder="Customer-facing. Change window Sun 02:00." />
                </div>
              </div>
              <div className="mt-3">
                <Label>Colour</Label>
                <ColorPicker value={color} onChange={setColor} />
              </div>
              <div className="mt-4 flex items-center gap-2">
                <Button variant="primary" onClick={saveIdentity} disabled={saving || !name.trim()}>
                  {saving ? <Loader2 size={15} className="animate-spin" /> : <CheckCircle2 size={15} />} Save
                </Button>
                <Button variant="ghost" onClick={() => { setEditing(false); setName(c.name); setDescription(c.description); setColor(c.color); }}>
                  Cancel
                </Button>
              </div>
            </div>
          )}
          <ClusterMembers name={c.name} onChanged={onChanged} />
          <ClusterPolicyEditor name={c.name} dests={dests} onSaved={onChanged} />
        </div>
      )}

      {confirmDel && (
        <DeleteClusterModal cluster={c} others={others} onClose={() => setConfirmDel(false)} onDone={onChanged} />
      )}
    </div>
  );
}

// ------------------------------------------------------------------ the card

export default function ClustersCard({ dests }: { dests: Destination[] }) {
  const toast = useToast();
  const [clusters, setClusters] = useState<Cluster[] | null>(null);
  const [adding, setAdding] = useState(false);
  const [newName, setNewName] = useState("");
  const [newDesc, setNewDesc] = useState("");
  const [newColor, setNewColor] = useState("");
  const [busy, setBusy] = useState(false);

  const load = () => api.listClusters().then((r) => setClusters(r.clusters)).catch(() => setClusters([]));
  useEffect(() => { load(); }, []);

  const create = async () => {
    setBusy(true);
    try {
      await api.createCluster(newName.trim(), newDesc.trim(), newColor);
      toast.success(`Cluster “${newName.trim()}” created`);
      setNewName(""); setNewDesc(""); setNewColor(""); setAdding(false); load();
    } catch (e) { toast.error((e as Error).message); }
    finally { setBusy(false); }
  };

  const total = useMemo(() => (clusters || []).reduce((n, c) => n + c.nodes, 0), [clusters]);

  return (
    <Card className="mt-5 p-5">
      <div className="mb-1 flex items-center gap-2 text-lg font-semibold">
        <Layers size={18} className="text-primary" /> Clusters
      </div>
      <p className="mb-4 text-sm text-on-surface-variant">
        Group your servers into failure domains — production, homelab, a remote site. The dashboard rolls health and
        backup coverage up per cluster, and each cluster can carry its own backup policy.
        Removing a cluster removes a grouping only: servers, containers and backups are never deleted.
      </p>

      <div className="overflow-hidden rounded border border-outline-variant">
        {clusters === null ? (
          <div className="px-4 py-6 text-sm text-on-surface-variant">Loading clusters…</div>
        ) : clusters.length === 0 ? (
          <div className="px-4 py-6 text-sm text-on-surface-variant">No clusters yet.</div>
        ) : (
          clusters.map((c) => (
            <ClusterRow
              key={c.name} c={c} dests={dests}
              others={clusters.filter((o) => o.name !== c.name)}
              onChanged={load}
            />
          ))
        )}
      </div>

      <div className="mt-3 flex flex-wrap items-center gap-3">
        {!adding && (
          <Button variant="secondary" onClick={() => setAdding(true)}>
            <Plus size={15} /> New cluster
          </Button>
        )}
        {clusters !== null && clusters.length > 0 && (
          <span className="text-xs text-on-surface-variant">
            {clusters.length} cluster{clusters.length === 1 ? "" : "s"} · {total} server{total === 1 ? "" : "s"}
          </span>
        )}
      </div>

      {adding && (
        <div className="mt-3 rounded border border-outline-variant bg-surface-lowest p-4">
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <div>
              <Label>Name</Label>
              <Input value={newName} onChange={(e) => setNewName(e.target.value)} placeholder="production" autoFocus />
            </div>
            <div>
              <Label>Description (optional)</Label>
              <Input value={newDesc} onChange={(e) => setNewDesc(e.target.value)} placeholder="Customer-facing services" />
            </div>
          </div>
          <div className="mt-3">
            <Label>Colour</Label>
            <ColorPicker value={newColor} onChange={setNewColor} />
          </div>
          <p className="mt-3 text-xs text-on-surface-variant">
            Creating a cluster changes nothing on its own — assign servers to it from each server’s Edit form.
          </p>
          <div className="mt-3 flex items-center gap-2">
            <Button variant="primary" onClick={create} disabled={busy || !newName.trim()}>
              {busy ? <Loader2 size={15} className="animate-spin" /> : <Plus size={15} />} Create cluster
            </Button>
            <Button variant="ghost" onClick={() => { setAdding(false); setNewName(""); setNewDesc(""); setNewColor(""); }}>
              Cancel
            </Button>
          </div>
        </div>
      )}
    </Card>
  );
}
