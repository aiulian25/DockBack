// ClusterSelect — the cluster field on the node form (F104).
//
// This replaces a plain text input, and that is the whole point: a free-text box
// is why `prod` and `Prod` could become two fleets. Existing clusters are
// offered from the registry; typing a name that doesn't exist yet still works in
// one keystroke and creates it on save, so the fast path is unchanged.
//
// The server canonicalizes on save regardless, so this control is a convenience,
// never the enforcement point.
import { useEffect, useMemo, useRef, useState } from "react";
import { Check, ChevronDown } from "lucide-react";
import { Cluster, api } from "../api";
import { clusterAccent } from "./ClusterBar";
import { Input, Label } from "./ui";

export default function ClusterSelect({ value, onChange, reloadKey }: {
  value: string;
  onChange: (v: string) => void;
  reloadKey?: unknown; // change to refetch (e.g. the modal reopening)
}) {
  const [clusters, setClusters] = useState<Cluster[]>([]);
  const [open, setOpen] = useState(false);
  const box = useRef<HTMLDivElement>(null);

  useEffect(() => {
    api.listClusters().then((r) => setClusters(r.clusters)).catch(() => setClusters([]));
  }, [reloadKey]);

  // Close on an outside click or Escape, matching the other menus in the app.
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (box.current && !box.current.contains(e.target as globalThis.Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => { if (e.key === "Escape") setOpen(false); };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    return () => { document.removeEventListener("mousedown", onDown); document.removeEventListener("keydown", onKey); };
  }, [open]);

  const typed = value.trim();
  // Suggestions narrow as you type; an exact (case-insensitive) match means the
  // typed name IS an existing cluster, so no "create" row is offered.
  const matches = useMemo(
    () => clusters.filter((c) => c.name.toLowerCase().includes(typed.toLowerCase())),
    [clusters, typed],
  );
  const exact = clusters.some((c) => c.name.toLowerCase() === typed.toLowerCase());

  const pick = (name: string) => { onChange(name); setOpen(false); };

  return (
    <div ref={box} className="relative">
      <Label>Cluster</Label>
      <div className="relative">
        <Input
          value={value}
          onChange={(e) => { onChange(e.target.value); setOpen(true); }}
          onFocus={() => setOpen(true)}
          placeholder="default"
          role="combobox"
          aria-expanded={open}
          aria-autocomplete="list"
          className="pr-8"
        />
        <button
          type="button"
          onClick={() => setOpen((o) => !o)}
          aria-label={open ? "Hide clusters" : "Show clusters"}
          className="absolute inset-y-0 right-0 flex w-8 items-center justify-center text-outline hover:text-on-surface"
        >
          <ChevronDown size={14} className={open ? "rotate-180 transition-transform" : "transition-transform"} />
        </button>
      </div>

      {open && (matches.length > 0 || (typed && !exact)) && (
        <div
          role="listbox"
          className="absolute z-20 mt-1 max-h-56 w-full overflow-auto rounded-lg border border-outline-variant bg-surface-lowest shadow-lg"
        >
          {matches.map((c) => {
            const selected = c.name.toLowerCase() === typed.toLowerCase();
            return (
              <button
                key={c.name}
                type="button"
                role="option"
                aria-selected={selected}
                onClick={() => pick(c.name)}
                className={`flex w-full items-center gap-2.5 px-3 py-2 text-left text-sm transition-colors hover:bg-surface-high ${selected ? "bg-primary/10" : ""}`}
              >
                <span className="min-w-0 flex-1 truncate">
                  {/* Identity colour as a rule under the name — never a swatch. */}
                  <span className="border-b-2 pb-px" style={{ borderColor: clusterAccent(c.name, c.color) }}>{c.name}</span>
                </span>
                <span className="tnum shrink-0 text-[11px] text-outline">
                  {c.nodes} node{c.nodes === 1 ? "" : "s"}
                </span>
                {selected && <Check size={13} className="shrink-0 text-primary" />}
              </button>
            );
          })}
          {typed && !exact && (
            <button
              type="button"
              onClick={() => pick(typed)}
              className="flex w-full items-center gap-2 border-t border-outline-variant px-3 py-2 text-left text-sm font-medium text-primary transition-colors hover:bg-surface-high"
            >
              Create “{typed}”
            </button>
          )}
        </div>
      )}
      <p className="mt-1 text-[11px] text-outline">
        Groups nodes into a failure domain. Backup policy can be set per cluster.
      </p>
    </div>
  );
}
