// Command palette (Cmd/Ctrl-K) — a fuzzy jump-to for nodes, containers,
// destinations, and pages, plus a small set of safe global actions. Replaces the
// former dead header search box (Fable-UI-UX A1). Read-only over the existing
// authenticated list endpoints; the only mutation is runSchedule (the same
// CSRF-protected "Run now" already offered in Settings). No new dependency — a
// small subsequence scorer does the fuzzy matching.
import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import {
  Search, Loader2, CornerDownLeft, Server, Box, HardDrive, Play,
  LayoutDashboard, CloudUpload, Terminal, ScrollText, Settings as Cog, FileText,
} from "lucide-react";
import { api, Container, Destination, Node } from "../api";

// --- fuzzy scoring (subsequence match with word-boundary / prefix bonuses) ---
function fuzzyScore(query: string, text: string): number {
  const q = query.toLowerCase();
  const s = text.toLowerCase();
  let si = 0, score = 0, streak = 0;
  for (let qi = 0; qi < q.length; qi++) {
    let found = -1;
    for (let k = si; k < s.length; k++) {
      if (s[k] === q[qi]) { found = k; break; }
    }
    if (found === -1) return -1;
    let b = 1;
    if (found === si) { streak++; b += streak; } else streak = 0;
    if (found === 0 || /[\s\-_./:]/.test(s[found - 1])) b += 3; // word boundary
    score += b;
    si = found + 1;
  }
  return score - s.length * 0.01; // gently prefer denser (shorter) matches
}

// rank scores an item against the query (title-prefix strongly boosted). Returns
// -1 when the query isn't a subsequence of any searchable field.
function rank(query: string, item: Cmd): number {
  if (!query) return 0;
  const t = item.title.toLowerCase();
  const q = query.toLowerCase();
  if (t.startsWith(q)) return 10000 - item.title.length;
  const hay = `${item.title} ${item.subtitle ?? ""} ${item.keywords ?? ""}`;
  return fuzzyScore(query, hay);
}

type Group = "Actions" | "Pages" | "Servers" | "Containers" | "Destinations";
interface Cmd {
  id: string;
  title: string;
  subtitle?: string;
  keywords?: string;
  group: Group;
  icon: React.ReactNode;
  run: () => void | Promise<void>;
}

const GROUP_ORDER: Group[] = ["Actions", "Pages", "Servers", "Containers", "Destinations"];
const MAX_RESULTS = 60;

// Raw index cache (already-authorized GET data), refreshed at most every 10s so
// reopening the palette is instant without going stale.
interface RawIndex { at: number; nodes: Node[]; containers: { c: Container; nodeId: string; nodeName: string }[]; destinations: Destination[] }
let rawCache: RawIndex | null = null;

async function buildRawIndex(): Promise<RawIndex> {
  if (rawCache && Date.now() - rawCache.at < 10000) return rawCache;
  const nodes = await api.nodes().catch(() => [] as Node[]);
  const perNode = await Promise.all(
    nodes.map((n) =>
      api.containersPage(n.id, { page_size: 500 })
        .then((p) => ({ n, cs: p.containers }))
        .catch(() => ({ n, cs: [] as Container[] })), // a down node must not break the palette
    ),
  );
  const containers = perNode.flatMap(({ n, cs }) => cs.map((c) => ({ c, nodeId: n.id, nodeName: n.name })));
  const destinations = await api.destinations().catch(() => [] as Destination[]);
  rawCache = { at: Date.now(), nodes, containers, destinations };
  return rawCache;
}

const isMac = typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform);

export default function CommandPalette({ open, onClose }: { open: boolean; onClose: () => void }) {
  const navigate = useNavigate();
  const [q, setQ] = useState("");
  const [sel, setSel] = useState(0);
  const [raw, setRaw] = useState<RawIndex | null>(rawCache);
  const [loading, setLoading] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);

  // Reset + focus + (re)build the index each time the palette opens.
  useEffect(() => {
    if (!open) return;
    setQ("");
    setSel(0);
    inputRef.current?.focus();
    let alive = true;
    if (!rawCache || Date.now() - rawCache.at >= 10000) setLoading(true);
    buildRawIndex().then((r) => { if (alive) { setRaw(r); setLoading(false); } });
    return () => { alive = false; };
  }, [open]);

  const go = (path: string) => { onClose(); navigate(path); };

  // Build the command list from the raw index + static pages + safe actions.
  const commands = useMemo<Cmd[]>(() => {
    const cmds: Cmd[] = [];
    // Global actions (kept minimal + safe; targeted backups belong to A2).
    cmds.push({
      id: "act:run-schedule", group: "Actions", title: "Run scheduled backups now",
      subtitle: "Trigger the schedule immediately, then watch the logs", keywords: "schedule run trigger backup now",
      icon: <Play size={16} />,
      run: async () => { onClose(); try { await api.runSchedule(); } catch { /* surfaced on the logs page */ } navigate("/logs"); },
    });
    // Pages.
    const pages: [string, string, string, React.ReactNode][] = [
      ["/", "Dashboard", "nodes overview home", <LayoutDashboard size={16} />],
      ["/servers", "Servers", "nodes hosts machines", <Server size={16} />],
      ["/backups", "Backups", "history archives restore", <CloudUpload size={16} />],
      ["/logs", "Logs", "stream events output", <Terminal size={16} />],
      ["/audit", "Audit Trail", "history actions security", <ScrollText size={16} />],
      ["/settings", "Settings", "policy retention destinations notifications schedule 2fa", <Cog size={16} />],
      ["/docs", "Documentation", "help docs guide", <FileText size={16} />],
    ];
    for (const [path, title, keywords, icon] of pages) {
      cmds.push({ id: "page:" + path, group: "Pages", title, subtitle: "Go to " + title, keywords, icon, run: () => go(path) });
    }
    if (raw) {
      for (const n of raw.nodes) {
        cmds.push({
          id: "node:" + n.id, group: "Servers", title: n.name,
          subtitle: n.address + (n.reachable ? "" : " · offline"), keywords: "node server host " + n.cluster,
          icon: <Server size={16} />, run: () => go(`/servers/${n.id}`),
        });
      }
      for (const { c, nodeId, nodeName } of raw.containers) {
        cmds.push({
          id: "ctr:" + nodeId + ":" + c.id, group: "Containers", title: c.name,
          subtitle: `${c.image} · on ${nodeName}${c.stack ? " · " + c.stack : ""}`,
          keywords: `container ${c.stack} ${c.service} ${c.state}`,
          icon: <Box size={16} />, run: () => go(`/servers/${nodeId}/containers/${c.id}`),
        });
      }
      for (const d of raw.destinations) {
        cmds.push({
          id: "dest:" + d.id, group: "Destinations", title: d.name,
          subtitle: `${d.type} destination`, keywords: "destination offsite storage " + d.type,
          icon: <HardDrive size={16} />, run: () => go("/settings"),
        });
      }
    }
    return cmds;
  }, [raw]); // eslint-disable-line react-hooks/exhaustive-deps

  // Filter + sort. Empty query keeps group order; otherwise rank by score.
  const results = useMemo(() => {
    if (!q.trim()) {
      return [...commands]
        .sort((a, b) => GROUP_ORDER.indexOf(a.group) - GROUP_ORDER.indexOf(b.group))
        .slice(0, MAX_RESULTS);
    }
    return commands
      .map((c) => ({ c, s: rank(q.trim(), c) }))
      .filter((x) => x.s > -1)
      .sort((a, b) => b.s - a.s)
      .slice(0, MAX_RESULTS)
      .map((x) => x.c);
  }, [commands, q]);

  useEffect(() => { setSel(0); }, [q]);
  useEffect(() => {
    // keep the selected row in view
    const el = listRef.current?.querySelector<HTMLElement>(`[data-idx="${sel}"]`);
    el?.scrollIntoView({ block: "nearest" });
  }, [sel, results]);

  if (!open) return null;

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "ArrowDown" || (e.ctrlKey && (e.key === "j" || e.key === "n"))) {
      e.preventDefault(); setSel((i) => Math.min(i + 1, results.length - 1));
    } else if (e.key === "ArrowUp" || (e.ctrlKey && (e.key === "k" || e.key === "p"))) {
      e.preventDefault(); setSel((i) => Math.max(i - 1, 0));
    } else if (e.key === "Enter") {
      e.preventDefault(); results[sel]?.run();
    } else if (e.key === "Escape") {
      e.preventDefault(); onClose();
    }
  };

  // Flatten with group headers while keeping a stable selection index.
  let lastGroup: Group | null = null;

  return (
    <div className="fixed inset-0 z-[60] flex items-start justify-center bg-black/60 p-4 pt-[14vh]" onMouseDown={onClose}>
      <div
        className="flex max-h-[70vh] w-full max-w-xl flex-col overflow-hidden rounded-xl border border-outline-variant bg-surface-high shadow-2xl"
        onMouseDown={(e) => e.stopPropagation()}
        onKeyDown={onKeyDown}
        role="dialog" aria-modal="true" aria-label="Command palette"
      >
        <div className="flex items-center gap-2.5 border-b border-outline-variant/60 px-4 py-3">
          <Search size={17} className="shrink-0 text-on-surface-variant" />
          <input
            ref={inputRef} value={q} onChange={(e) => setQ(e.target.value)}
            placeholder="Search nodes, containers, destinations, actions…"
            className="w-full bg-transparent text-sm text-on-surface outline-none placeholder:text-on-surface-variant/70"
            autoComplete="off" spellCheck={false}
          />
          {loading && <Loader2 size={15} className="shrink-0 animate-spin text-on-surface-variant" />}
        </div>

        <div ref={listRef} className="min-h-0 flex-1 overflow-y-auto py-1.5">
          {results.length === 0 ? (
            <div className="px-4 py-8 text-center text-sm text-on-surface-variant">
              {loading ? "Building index…" : q.trim() ? "No matches." : "Type to search."}
            </div>
          ) : (
            results.map((r, i) => {
              const header = r.group !== lastGroup ? r.group : null;
              lastGroup = r.group;
              return (
                <div key={r.id}>
                  {header && (
                    <div className="px-4 pb-1 pt-2 text-[10px] font-semibold uppercase tracking-[0.1em] text-on-surface-variant/70">{header}</div>
                  )}
                  <button
                    data-idx={i}
                    onMouseMove={() => setSel(i)}
                    onClick={() => r.run()}
                    className={`flex w-full items-center gap-3 px-4 py-2 text-left ${i === sel ? "bg-docker-blue/15" : "hover:bg-surface-highest/50"}`}
                  >
                    <span className={i === sel ? "text-primary" : "text-on-surface-variant"}>{r.icon}</span>
                    <span className="min-w-0 flex-1">
                      <span className="block truncate text-sm text-on-surface">{r.title}</span>
                      {r.subtitle && <span className="block truncate text-xs text-on-surface-variant">{r.subtitle}</span>}
                    </span>
                    {i === sel && <CornerDownLeft size={14} className="shrink-0 text-on-surface-variant" />}
                  </button>
                </div>
              );
            })
          )}
        </div>

        <div className="flex items-center gap-3 border-t border-outline-variant/60 px-4 py-2 text-[11px] text-on-surface-variant">
          <span><kbd className="rounded bg-surface-highest px-1.5 py-0.5 font-mono">↑↓</kbd> navigate</span>
          <span><kbd className="rounded bg-surface-highest px-1.5 py-0.5 font-mono">↵</kbd> open</span>
          <span><kbd className="rounded bg-surface-highest px-1.5 py-0.5 font-mono">esc</kbd> close</span>
          <span className="ml-auto">{isMac ? "⌘K" : "Ctrl K"} to toggle</span>
        </div>
      </div>
    </div>
  );
}
