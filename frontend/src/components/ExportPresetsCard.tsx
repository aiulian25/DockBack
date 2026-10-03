// F101: the fleet-level app-native export preset library.
//
// The per-container mechanism always worked; it just wasn't reusable. An
// operator who worked out the right `vaultwarden` or `nextcloud occ` invocation
// had to retype it for every container and couldn't share it. This is where a
// recipe is written once, named, and then applied from any container's page.
//
// These commands RUN INSIDE containers, so the card is deliberately explicit
// about that — the commands are always visible, never collapsed behind a name,
// and importing someone else's library is a confirmed, destructive-sounding
// action rather than a quiet merge.
import { useCallback, useEffect, useRef, useState } from "react";
import { Plus, Trash2, Download, Upload, Loader2, Pencil, Check, X, Lock, Layers } from "lucide-react";
import { api, ExportPreset } from "../api";
import { Button, Card, Label } from "./ui";
import { useToast } from "./Toast";

const blank = (): ExportPreset => ({ id: "", name: "", match: "", dir: "", export_cmd: "", import_cmd: "", verify_cmd: "", user: "" });

export default function ExportPresetsCard() {
  const [presets, setPresets] = useState<ExportPreset[]>([]);
  const [draft, setDraft] = useState<ExportPreset | null>(null);
  const [busy, setBusy] = useState(false);
  const fileRef = useRef<HTMLInputElement | null>(null);
  const toast = useToast();

  const load = useCallback(() => {
    api.exportPresets().then((r) => setPresets(r.presets || [])).catch(() => setPresets([]));
  }, []);
  useEffect(() => { load(); }, [load]);

  const mine = presets.filter((p) => !p.builtin);
  const builtins = presets.filter((p) => p.builtin);

  const save = async () => {
    if (!draft) return;
    setBusy(true);
    try {
      await api.saveExportPreset(draft);
      setDraft(null); load();
      toast.success(`Saved preset "${draft.name.trim()}"`);
    } catch (e) { toast.error(`Couldn't save preset: ${(e as Error).message}`); }
    finally { setBusy(false); }
  };

  const remove = async (p: ExportPreset) => {
    if (!confirm(`Delete the preset "${p.name}"?\n\nContainers already configured from it keep their own copy — this only stops it being applied to new ones.`)) return;
    try { await api.deleteExportPreset(p.id); load(); }
    catch (e) { toast.error(`Couldn't delete preset: ${(e as Error).message}`); }
  };

  // Export: the operator's own presets only. The built-ins ship with DockBack,
  // so including them would make an imported file look like it carries recipes
  // it did not actually author.
  const exportJSON = () => {
    const blob = new Blob([JSON.stringify(mine, null, 2)], { type: "application/json" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = "dockback-export-presets.json";
    a.click();
    URL.revokeObjectURL(url);
  };

  // Import: REPLACES the library, and says so. These are shell commands that
  // will run inside containers, so the confirm names the count and points at the
  // review the operator should already have done.
  const importJSON = async (file: File) => {
    let parsed: ExportPreset[];
    try {
      const raw = JSON.parse(await file.text());
      parsed = Array.isArray(raw) ? raw : raw?.presets;
      if (!Array.isArray(parsed)) throw new Error("expected a list of presets");
    } catch (e) {
      toast.error(`That file isn't a preset list: ${(e as Error).message}`);
      return;
    }
    if (!confirm(
      `Import ${parsed.length} preset(s)?\n\n` +
      `This REPLACES your current ${mine.length} preset(s).\n\n` +
      `Presets contain commands that DockBack runs inside your containers. ` +
      `Only import a file you have read and trust — they will be listed in full after importing, and nothing runs until you enable app-native export for a backup.`
    )) return;
    setBusy(true);
    try {
      await api.importExportPresets(parsed);
      load();
      toast.success(`Imported ${parsed.length} preset(s) — review the commands below`);
    } catch (e) { toast.error(`Couldn't import presets: ${(e as Error).message}`); }
    finally { setBusy(false); }
  };

  const field = (label: string, key: keyof ExportPreset, placeholder: string, mono = true) => (
    <div>
      <Label>{label}</Label>
      <input
        value={(draft?.[key] as string) || ""}
        onChange={(e) => setDraft((d) => (d ? { ...d, [key]: e.target.value } : d))}
        placeholder={placeholder}
        className={`w-full rounded border border-outline-variant bg-surface-lowest px-3 py-2 ${mono ? "font-mono text-xs" : "text-sm"} outline-none focus:border-docker-blue`}
      />
    </div>
  );

  return (
    <Card className="mt-5 p-5">
      <div className="mb-1 flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2 text-lg font-semibold"><Layers size={18} className="text-primary" /> App-native export presets</div>
        <div className="flex flex-wrap gap-2">
          <Button variant="ghost" onClick={exportJSON} disabled={mine.length === 0} title="Download your presets as JSON to reuse on another DockBack">
            <Download size={15} /> Export presets
          </Button>
          <Button variant="ghost" onClick={() => fileRef.current?.click()} disabled={busy} title="Replace your presets from a JSON file">
            <Upload size={15} /> Import presets
          </Button>
          <input ref={fileRef} type="file" accept="application/json,.json" className="hidden"
            onChange={(e) => { const f = e.target.files?.[0]; e.target.value = ""; if (f) importJSON(f); }} />
          <Button variant="secondary" onClick={() => setDraft(blank())} disabled={!!draft}><Plus size={16} /> Add preset</Button>
        </div>
      </div>
      <p className="mb-4 text-xs text-on-surface-variant">
        A named export/import recipe you can apply to any container, instead of retyping the same commands. A preset whose <span className="font-mono">match</span> appears
        in a container's image is offered automatically; the container's own saved profile always wins over it.
        These commands run <span className="font-medium">inside</span> the container — but only when you enable app-native export for a backup.
      </p>

      {draft && (
        <div className="mb-4 space-y-2 rounded border border-primary/40 bg-primary/5 p-3">
          <div className="grid gap-2 sm:grid-cols-2">
            {field("Name", "name", "Nextcloud occ export", false)}
            {field("Image match (optional)", "match", "nextcloud")}
          </div>
          {field("Export directory", "dir", "/var/www/html/export")}
          {field("Export command", "export_cmd", "php occ maintenance:mode --on && tar -cf /export/data.tar /var/www/html/data")}
          {field("Import command", "import_cmd", "tar -xf /export/data.tar -C /")}
          {/* F152: optional, and only useful when the command can actually fail. */}
          {field("Verify command (optional)", "verify_cmd", "the app's own consistency check, if it exits non-zero on a problem")}
          {field("Run as user (optional)", "user", "www-data", false)}
          <div className="flex flex-wrap gap-2 pt-1">
            <Button variant="secondary" onClick={save} disabled={busy || !draft.name.trim()}>
              {busy ? <Loader2 size={15} className="animate-spin" /> : <Check size={15} />} Save preset
            </Button>
            <Button variant="ghost" onClick={() => setDraft(null)}><X size={15} /> Cancel</Button>
          </div>
        </div>
      )}

      <div className="space-y-2">
        {mine.length === 0 && !draft && (
          <div className="rounded border border-outline-variant bg-surface-lowest px-3 py-4 text-center text-xs text-on-surface-variant">
            No presets yet. Add one, or copy a built-in below as a starting point.
          </div>
        )}
        {mine.map((p) => (
          <PresetRow key={p.id} p={p} onEdit={() => setDraft({ ...p })} onDelete={() => remove(p)} />
        ))}
      </div>

      {builtins.length > 0 && (
        <div className="mt-4 border-t border-outline-variant/40 pt-3">
          <div className="mb-2 flex items-center gap-1.5 text-xs font-medium uppercase tracking-wider text-on-surface-variant">
            <Lock size={12} /> Built in
          </div>
          <p className="mb-2 text-xs text-on-surface-variant">
            Shipped with DockBack and used when nothing of yours matches. They can't be edited — use <span className="font-medium">Copy to my presets</span> to start from one.
          </p>
          <div className="space-y-2">
            {builtins.map((p) => (
              <PresetRow key={p.id} p={p} onEdit={() => setDraft({ ...p, id: "", name: `${p.name} (copy)`, builtin: false })} />
            ))}
          </div>
        </div>
      )}
    </Card>
  );
}

function PresetRow({ p, onEdit, onDelete }: { p: ExportPreset; onEdit: () => void; onDelete?: () => void }) {
  return (
    <div className="rounded border border-outline-variant bg-surface-lowest px-3 py-2 text-sm">
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
        <span className="min-w-0 break-words font-medium">{p.name}</span>
        {p.match
          ? <span className="shrink-0 rounded bg-surface-high px-1.5 py-0.5 font-mono text-[11px] text-on-surface-variant">match: {p.match}</span>
          : <span className="shrink-0 text-[11px] text-on-surface-variant">applied by hand</span>}
        {p.user && <span className="shrink-0 text-[11px] text-on-surface-variant">as {p.user}</span>}
        <div className="ml-auto flex shrink-0 gap-1">
          <Button variant="ghost" className="h-7 px-2 py-0 text-xs" onClick={onEdit}>
            {p.builtin ? <><Plus size={13} /> Copy to my presets</> : <><Pencil size={13} /> Edit</>}
          </Button>
          {onDelete && (
            <Button variant="ghost" className="h-7 px-2 py-0 text-xs text-error" onClick={onDelete}><Trash2 size={13} /> Delete</Button>
          )}
        </div>
      </div>
      {/* The commands are always visible: what will run inside a container must
          never be hidden behind a friendly name. */}
      <dl className="mt-1.5 space-y-0.5 text-[11px] text-on-surface-variant">
        <div className="flex gap-2"><dt className="w-16 shrink-0">dir</dt><dd className="min-w-0 break-all font-mono">{p.dir || "—"}</dd></div>
        <div className="flex gap-2"><dt className="w-16 shrink-0">export</dt><dd className="min-w-0 break-all font-mono">{p.export_cmd || "—"}</dd></div>
        <div className="flex gap-2"><dt className="w-16 shrink-0">import</dt><dd className="min-w-0 break-all font-mono">{p.import_cmd || "—"}</dd></div>
        {p.verify_cmd && <div className="flex gap-2"><dt className="w-16 shrink-0">verify</dt><dd className="min-w-0 break-all font-mono">{p.verify_cmd}</dd></div>}
      </dl>
    </div>
  );
}
