// In-app documentation. Articles are authored as Markdown files under
// ../docs/content and rendered with react-markdown. The left rail lists
// categories → articles; a search box filters by title + content.
import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { Search, Copy, Check, BookOpen, CalendarClock, CloudUpload, HardDrive, Rocket, RotateCcw, Server, ShieldCheck, type LucideIcon } from "lucide-react";
import { categories, DocArticle } from "../docs/manifest";

// Category icons, keyed by the manifest's `icon:` names. A star-import of
// lucide-react here used to drag all 1,545 icon modules into the bundle
// (~600 KB minified) — named imports keep tree-shaking intact. Every `icon:`
// value in ../docs/manifest.ts MUST have an entry in this map (unknown names
// fall back to BookOpen).
const docIcons: Record<string, LucideIcon> = {
  BookOpen, CalendarClock, CloudUpload, HardDrive, Rocket, RotateCcw, Server, ShieldCheck,
};

// A <pre> with a one-click Copy button — used for every fenced code block so
// commands and compose files are trivially copy-pasteable.
function CodeBlock({ children }: { children?: React.ReactNode }) {
  const ref = useRef<HTMLPreElement>(null);
  const [copied, setCopied] = useState(false);
  const copy = () => {
    const text = ref.current?.innerText ?? "";
    navigator.clipboard?.writeText(text);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  };
  return (
    <div className="group relative">
      <button
        onClick={copy}
        className="absolute right-2 top-2 z-10 flex items-center gap-1 rounded border border-outline-variant bg-surface-high/90 px-2 py-1 text-[11px] text-on-surface-variant opacity-0 transition-opacity hover:text-on-surface group-hover:opacity-100"
      >
        {copied ? <><Check size={12} className="text-success" /> Copied</> : <><Copy size={12} /> Copy</>}
      </button>
      <pre ref={ref}>{children}</pre>
    </div>
  );
}

// Markdown bodies load ON DEMAND (perf Fix 3): the lazy glob keeps ~220 KB of
// docs content out of the route chunk — each article fetches its own tiny
// module when opened. Keys look like "../docs/content/nodes/synology.md".
const raw = import.meta.glob("../docs/content/**/*.md", { query: "?raw", import: "default" }) as Record<string, () => Promise<string>>;
const bodyFor = (file: string): Promise<string> => {
  const load = raw[`../docs/content/${file}.md`];
  return load ? load() : Promise.resolve("");
};

interface Flat extends DocArticle { catSlug: string; catTitle: string; }
const flat: Flat[] = categories.flatMap((c) => c.articles.map((a) => ({ ...a, catSlug: c.slug, catTitle: c.title })));

export default function Docs() {
  const { cat, art } = useParams();
  const navigate = useNavigate();
  const [q, setQ] = useState("");
  const articleRef = useRef<HTMLElement>(null);

  // Resolve the current article (default to the very first one).
  const current = useMemo(() => {
    return flat.find((a) => a.catSlug === cat && a.slug === art) || flat[0];
  }, [cat, art]);

  useEffect(() => {
    if (!cat || !art) navigate(`/docs/${flat[0].catSlug}/${flat[0].slug}`, { replace: true });
  }, [cat, art, navigate]);

  // Reset the article pane to the top when switching articles.
  useEffect(() => { articleRef.current?.scrollTo(0, 0); }, [current.file]);

  // The current article's body, loaded on demand (null = loading skeleton).
  const [body, setBody] = useState<string | null>(null);
  useEffect(() => {
    let gone = false;
    setBody(null);
    bodyFor(current.file).then((b) => { if (!gone) setBody(b); }).catch(() => { if (!gone) setBody(""); });
    return () => { gone = true; };
  }, [current.file]);

  // Full-text search corpus, loaded ONCE the first time the user types — the
  // same results as the old eager bundle, just fetched lazily (a few dozen
  // small same-origin modules; imperceptible). Until it lands, matches are
  // title/category-only and the memo below re-runs when the corpus arrives.
  const [allBodies, setAllBodies] = useState<Record<string, string> | null>(null);
  useEffect(() => {
    if (!q.trim() || allBodies !== null) return;
    let gone = false;
    Promise.all(flat.map((a) => bodyFor(a.file).then((b) => [a.file, b] as const)))
      .then((entries) => { if (!gone) setAllBodies(Object.fromEntries(entries)); })
      .catch(() => { if (!gone) setAllBodies({}); });
    return () => { gone = true; };
  }, [q, allBodies]);

  const results = useMemo(() => {
    const term = q.trim().toLowerCase();
    if (!term) return null;
    return flat.filter((a) =>
      a.title.toLowerCase().includes(term) ||
      a.catTitle.toLowerCase().includes(term) ||
      (allBodies?.[a.file] || "").toLowerCase().includes(term)
    );
  }, [q, allBodies]);

  return (
    // Fixed-height page so each column scrolls on its own (the page itself
    // doesn't scroll). Height = viewport minus the top bar + main padding.
    <div className="flex h-[calc(100vh-7rem)] flex-col">
      <div className="shrink-0">
        <h1 className="text-2xl font-bold">Documentation</h1>
        <p className="mb-4 mt-1 text-on-surface-variant">Step-by-step guides for every part of DockBack.</p>
      </div>

      <div className="grid min-h-0 flex-1 grid-cols-1 gap-6 lg:grid-cols-[280px_1fr]">
        {/* Navigation rail — search pinned, list scrolls independently */}
        <aside className="flex min-h-0 flex-col">
          <div className="relative mb-4 shrink-0">
            <Search size={15} className="absolute left-3 top-1/2 -translate-y-1/2 text-on-surface-variant" />
            <input
              value={q} onChange={(e) => setQ(e.target.value)}
              placeholder="Search the docs…"
              className="w-full rounded border border-outline-variant bg-surface-lowest py-2 pl-9 pr-3 text-sm outline-none focus:border-docker-blue"
            />
          </div>

          <div className="min-h-0 flex-1 overflow-y-auto pr-1">
            {results ? (
              <div className="space-y-1">
                <div className="px-2 pb-1 text-xs uppercase tracking-wider text-on-surface-variant">{results.length} result{results.length === 1 ? "" : "s"}</div>
                {results.map((a) => (
                  <NavItem key={a.catSlug + a.slug} active={a.catSlug === current.catSlug && a.slug === current.slug}
                    onClick={() => { setQ(""); navigate(`/docs/${a.catSlug}/${a.slug}`); }}>
                    <span className="block">{a.title}</span>
                    <span className="block text-[11px] text-on-surface-variant">{a.catTitle}</span>
                  </NavItem>
                ))}
                {results.length === 0 && <div className="px-2 py-3 text-sm text-on-surface-variant">No matches.</div>}
              </div>
            ) : (
              <nav className="space-y-5">
                {categories.map((c) => {
                  const Icon = docIcons[c.icon] || BookOpen;
                  return (
                    <div key={c.slug}>
                      <div className="mb-2 flex items-center gap-2 px-2 text-sm font-bold uppercase tracking-wide text-on-surface">
                        <Icon size={15} className="text-primary" /> {c.title}
                      </div>
                      <div className="space-y-0.5">
                        {c.articles.map((a) => (
                          <NavItem key={a.slug} active={current.catSlug === c.slug && current.slug === a.slug}
                            onClick={() => navigate(`/docs/${c.slug}/${a.slug}`)}>
                            {a.title}
                          </NavItem>
                        ))}
                      </div>
                    </div>
                  );
                })}
              </nav>
            )}
          </div>
        </aside>

        {/* Article — scrolls independently of the nav */}
        <article ref={articleRef} className="min-h-0 min-w-0 overflow-y-auto rounded-lg border border-outline-variant/50 bg-surface-low p-5">
          <div className="mb-4 text-xs uppercase tracking-wider text-on-surface-variant">{current.catTitle}</div>
          <div className="doc-md">
            {body === null ? (
              <div className="animate-pulse space-y-3" aria-hidden>
                <div className="h-6 w-2/3 rounded bg-surface-highest" />
                <div className="h-3 w-full rounded bg-surface-highest" />
                <div className="h-3 w-11/12 rounded bg-surface-highest" />
                <div className="h-3 w-4/5 rounded bg-surface-highest" />
              </div>
            ) : (
              <ReactMarkdown remarkPlugins={[remarkGfm]} components={{ pre: CodeBlock }}>{body}</ReactMarkdown>
            )}
          </div>
        </article>
      </div>
    </div>
  );
}

function NavItem({ active, onClick, children }: { active: boolean; onClick: () => void; children: React.ReactNode }) {
  return (
    <button onClick={onClick}
      className={`block w-full rounded px-2.5 py-1.5 text-left text-sm transition-colors ${
        active ? "bg-docker-blue/15 text-primary" : "text-on-surface-variant hover:bg-surface-high/50 hover:text-on-surface"
      }`}>
      {children}
    </button>
  );
}
