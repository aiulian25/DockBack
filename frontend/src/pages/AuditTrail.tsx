// Audit Trail — append-only record of every action. Server-side searched,
// date-ranged, paged, and exportable to CSV/JSON (F7) so the whole (unbounded)
// log is browsable and an auditor can be handed a file.
import { useCallback, useEffect, useState } from "react";
import { Search, Loader2, Download, ShieldCheck, AlertTriangle, KeyRound } from "lucide-react";
import { api, AuditPage, AuditQuery } from "../api";
import { Card } from "../components/ui";
import { usePoll } from "../hooks/usePoll";

const PAGE_SIZE = 50;

function actionColor(a: string): string {
  if (a.endsWith(".failed") || a.endsWith(".delete")) return "bg-error/10 text-error";
  if (a.endsWith(".update") || a.startsWith("restore")) return "bg-warning/10 text-warning";
  if (a.startsWith("backup")) return "bg-secondary/10 text-secondary";
  return "bg-surface-high text-on-surface-variant";
}

// A date input (YYYY-MM-DD, local) → inclusive unix-second day boundary.
const dayStart = (s: string) => (s ? Math.floor(new Date(s + "T00:00:00").getTime() / 1000) : undefined);
const dayEnd = (s: string) => (s ? Math.floor(new Date(s + "T23:59:59").getTime() / 1000) : undefined);

export default function AuditTrail() {
  const [q, setQ] = useState("");
  const [fromStr, setFromStr] = useState("");
  const [toStr, setToStr] = useState("");
  const [page, setPage] = useState(1);
  const [data, setData] = useState<AuditPage | null>(null);
  const [err, setErr] = useState(false);
  // F68: tamper-evident chain verification result.
  const [verifying, setVerifying] = useState(false);
  const [verify, setVerify] = useState<Awaited<ReturnType<typeof api.auditVerify>> | null>(null);
  const [verifyErr, setVerifyErr] = useState("");

  // The active filter (shared by the table query and the export links).
  const filter = useCallback((): AuditQuery => ({
    q: q.trim() || undefined, from: dayStart(fromStr), to: dayEnd(toStr),
  }), [q, fromStr, toStr]);

  const load = useCallback(() => {
    api.auditPage({ ...filter(), page, page_size: PAGE_SIZE })
      .then((d) => { setData(d); setErr(false); })
      .catch(() => setErr(true));
  }, [filter, page]);

  // Reload on query/filter/page change (debounced), plus a gentle live refresh.
  useEffect(() => { const h = setTimeout(load, 200); return () => clearTimeout(h); }, [load]);
  usePoll(load, 15000, [load]);

  // Any filter change resets to the first page.
  const changeFilter = (fn: () => void) => { fn(); setPage(1); };

  // F200: check the trail against a checkpoint kept OUTSIDE this machine.
  const [beaconOpen, setBeaconOpen] = useState(false);
  const [beaconId, setBeaconId] = useState("");
  const [beaconChain, setBeaconChain] = useState("");
  const [beaconBusy, setBeaconBusy] = useState(false);
  const [beaconRes, setBeaconRes] = useState<{ ok: boolean; reason: string } | null>(null);
  const checkBeacon = () => {
    setBeaconBusy(true); setBeaconRes(null);
    api.auditVerifyBeacon(parseInt(beaconId, 10) || 0, beaconChain.trim())
      .then(setBeaconRes)
      .catch((e) => setBeaconRes({ ok: false, reason: (e as Error).message }))
      .finally(() => setBeaconBusy(false));
  };

  const total = data?.total ?? 0;
  const pageCount = Math.max(1, Math.ceil(total / PAGE_SIZE));
  const items = data?.items ?? [];
  const exportUrl = (format: "csv" | "json") => api.auditExportUrl({ ...filter(), format });

  return (
    <div>
      <div className="mb-6 flex flex-wrap items-end justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold">Audit Trail</h1>
          <p className="mt-1 text-on-surface-variant">Append-only record of every action — logins, backups, restores, node and settings changes.</p>
        </div>
        <div className="flex items-center gap-2">
          {/* F68: recompute the HMAC chain over every row since the anchor. */}
          <button
            onClick={() => {
              setVerifying(true); setVerify(null); setVerifyErr("");
              api.auditVerify().then(setVerify).catch((e) => setVerifyErr((e as Error).message)).finally(() => setVerifying(false));
            }}
            disabled={verifying}
            className="inline-flex items-center gap-1.5 rounded border border-outline-variant bg-surface-high/40 px-3 py-2 text-sm text-on-surface hover:bg-surface-high disabled:opacity-60"
          >
            {verifying ? <Loader2 size={15} className="animate-spin" /> : <ShieldCheck size={15} />} Verify integrity
          </button>
          {/* F200: the check that survives a compromise of THIS machine. */}
          <button
            onClick={() => setBeaconOpen((v) => !v)}
            className="inline-flex items-center gap-1.5 rounded border border-outline-variant bg-surface-high/40 px-3 py-2 text-sm text-on-surface hover:bg-surface-high"
          >
            <KeyRound size={15} /> Verify against a checkpoint
          </button>
          <a href={exportUrl("csv")} className="inline-flex items-center gap-1.5 rounded border border-outline-variant bg-surface-high/40 px-3 py-2 text-sm text-on-surface hover:bg-surface-high"><Download size={15} /> Export CSV</a>
          <a href={exportUrl("json")} className="inline-flex items-center gap-1.5 rounded border border-outline-variant bg-surface-high/40 px-3 py-2 text-sm text-on-surface hover:bg-surface-high"><Download size={15} /> Export JSON</a>
        </div>
      </div>

      {/* F200: verify against a checkpoint the operator kept off this machine.
          The built-in check above proves the chain is self-consistent — but
          anyone who reached this container holds the key that mints it, so they
          could rewrite the trail AND its own records and pass. A checkpoint that
          left the building is the one thing they could not edit. */}
      {beaconOpen && (
        <div className="mb-4 rounded border border-outline-variant bg-surface-lowest p-4">
          <div className="flex items-center gap-2 text-sm font-medium text-on-surface"><KeyRound size={15} className="text-primary" /> Verify against an off-host checkpoint</div>
          <p className="mt-1 max-w-3xl text-xs text-on-surface-variant">
            Paste the entry number and value from a <span className="font-medium text-on-surface">DockBack audit checkpoint</span> message — the one delivered to your email, webhook or push channel. Because that copy lives outside this machine, it catches a rewrite that the check above cannot: whoever can edit this database can also recompute every link in it.
          </p>
          {/* Stacks on narrow screens and for longer translated labels; the
              value field is monospace and wraps rather than overflowing. */}
          <div className="mt-3 flex flex-col gap-2 sm:flex-row sm:items-end">
            <div className="sm:w-40">
              <label className="mb-1 block text-xs text-on-surface-variant">Entry number</label>
              <input value={beaconId} onChange={(e) => setBeaconId(e.target.value)} inputMode="numeric" placeholder="1234"
                className="w-full rounded border border-outline-variant bg-surface px-2 py-1.5 font-mono text-sm text-on-surface" />
            </div>
            <div className="min-w-0 flex-1">
              <label className="mb-1 block text-xs text-on-surface-variant">Checkpoint value</label>
              <input value={beaconChain} onChange={(e) => setBeaconChain(e.target.value)} spellCheck={false} placeholder="the long value from the message"
                className="w-full rounded border border-outline-variant bg-surface px-2 py-1.5 font-mono text-sm text-on-surface" />
            </div>
            <button onClick={checkBeacon} disabled={beaconBusy || !beaconId.trim() || !beaconChain.trim()}
              className="inline-flex shrink-0 items-center justify-center gap-1.5 rounded bg-primary px-3 py-2 text-sm font-medium text-on-primary hover:bg-primary/90 disabled:opacity-60">
              {beaconBusy ? <Loader2 size={15} className="animate-spin" /> : <ShieldCheck size={15} />} Check
            </button>
          </div>
          {beaconRes && (
            <div className={`mt-3 flex items-start gap-2 rounded px-3 py-2 text-sm ${beaconRes.ok ? "bg-success/10 text-success" : "bg-error/10 text-error"}`}>
              {beaconRes.ok ? <ShieldCheck size={16} className="mt-0.5 shrink-0" /> : <AlertTriangle size={16} className="mt-0.5 shrink-0" />}
              <span className="min-w-0 break-words">{beaconRes.reason}</span>
            </div>
          )}
          {verify?.beacon_sent && (
            <p className="mt-3 text-xs text-on-surface-variant">
              This instance last sent a checkpoint for entry <span className="font-mono">#{verify.beacon_sent.head_id}</span> on {new Date(verify.beacon_sent.at * 1000).toLocaleString()}. That record is shown only to help you find the right message — it lives in this database, so it is not evidence on its own.
            </p>
          )}
        </div>
      )}

      {/* F68: chain-verification outcome. Each entry is HMAC-linked to its
          predecessor under the encryption key, so silent edits are detectable. */}
      {verify && verify.ok && (
        <div className="mb-4 flex items-center gap-2 rounded border border-success/40 bg-success/10 px-4 py-2.5 text-sm text-success">
          <ShieldCheck size={16} className="shrink-0" /> Checked {verify.checked} row{verify.checked === 1 ? "" : "s"} — chain intact. Entries since the chain anchor have not been modified, deleted, or reordered.
        </div>
      )}
      {verify && !verify.ok && (
        <div className="mb-4 rounded border border-error/40 bg-error/10 px-4 py-3 text-sm">
          <div className="flex items-center gap-2 font-semibold text-error"><AlertTriangle size={16} className="shrink-0" /> Audit trail integrity check FAILED at entry #{verify.first_bad_id}</div>
          <p className="mt-1 text-xs text-on-surface-variant">Rows were modified, deleted, or reordered after being written ({verify.checked} verified before the break). Treat everything after that entry as untrustworthy and investigate who has file access to the DockBack database.</p>
        </div>
      )}
      {verifyErr && <div className="mb-4 rounded bg-error/10 px-4 py-2.5 text-sm text-error">Couldn't verify the audit trail: {verifyErr}</div>}

      {/* Server-side search + date range (F7) */}
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <div className="relative">
          <Search size={16} className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-on-surface-variant" />
          <input
            value={q} onChange={(e) => changeFilter(() => setQ(e.target.value))} placeholder="Search actor, action, target or detail…"
            className="w-72 rounded border border-outline-variant bg-surface-lowest py-2 pl-9 pr-3 text-sm outline-none focus:border-docker-blue"
          />
        </div>
        <label className="flex items-center gap-1.5 text-xs text-on-surface-variant">
          From
          <input type="date" value={fromStr} onChange={(e) => changeFilter(() => setFromStr(e.target.value))}
            className="rounded border border-outline-variant bg-surface-lowest px-2 py-1.5 text-sm text-on-surface outline-none focus:border-docker-blue" />
        </label>
        <label className="flex items-center gap-1.5 text-xs text-on-surface-variant">
          To
          <input type="date" value={toStr} onChange={(e) => changeFilter(() => setToStr(e.target.value))}
            className="rounded border border-outline-variant bg-surface-lowest px-2 py-1.5 text-sm text-on-surface outline-none focus:border-docker-blue" />
        </label>
        {(q || fromStr || toStr) && (
          <button onClick={() => changeFilter(() => { setQ(""); setFromStr(""); setToStr(""); })}
            className="text-xs text-on-surface-variant hover:text-on-surface">Clear</button>
        )}
      </div>

      <Card className="overflow-hidden">
        <table className="w-full text-left text-sm">
          <thead className="border-b border-outline-variant/60 bg-surface-lowest/40 text-xs uppercase tracking-widest text-on-surface-variant">
            <tr>
              <th className="px-5 py-3 font-semibold">Time</th>
              <th className="px-5 py-3 font-semibold">Actor</th>
              <th className="px-5 py-3 font-semibold">Action</th>
              <th className="px-5 py-3 font-semibold">Target</th>
              <th className="px-5 py-3 font-semibold">Detail</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-outline-variant/30">
            {data === null && !err && (
              <tr><td colSpan={5} className="px-5 py-10 text-center text-on-surface-variant"><span className="inline-flex items-center gap-2"><Loader2 size={15} className="animate-spin" /> Loading audit trail…</span></td></tr>
            )}
            {err && data === null && (
              <tr><td colSpan={5} className="px-5 py-10 text-center text-error">Couldn't load the audit trail — retrying…</td></tr>
            )}
            {data !== null && items.map((a, i) => (
              <tr key={i} className="hover:bg-surface-highest/40">
                <td className="px-5 py-3 tnum whitespace-nowrap text-on-surface-variant">{new Date(a.ts * 1000).toLocaleString()}</td>
                <td className="px-5 py-3">{a.actor}</td>
                <td className="px-5 py-3"><span className={`rounded px-2 py-0.5 font-mono text-xs ${actionColor(a.action)}`}>{a.action}</span></td>
                <td className="px-5 py-3 font-mono text-xs text-on-surface-variant">{a.target}</td>
                <td className="px-5 py-3 text-on-surface-variant">{a.detail}</td>
              </tr>
            ))}
            {data !== null && items.length === 0 && <tr><td colSpan={5} className="px-5 py-10 text-center text-on-surface-variant">{total === 0 && !(q || fromStr || toStr) ? "No audit entries yet." : "No entries match your search."}</td></tr>}
          </tbody>
        </table>
        {pageCount > 1 && (
          <div className="flex items-center justify-between gap-3 border-t border-outline-variant px-5 py-3 text-xs text-on-surface-variant">
            <span>Showing {(page - 1) * PAGE_SIZE + 1}–{Math.min(page * PAGE_SIZE, total)} of {total}</span>
            <div className="flex items-center gap-2">
              <button onClick={() => setPage((p) => Math.max(1, p - 1))} disabled={page <= 1}
                className="rounded border border-outline-variant px-2.5 py-1 hover:bg-surface-highest disabled:opacity-40">Prev</button>
              <span>Page {page} / {pageCount}</span>
              <button onClick={() => setPage((p) => Math.min(pageCount, p + 1))} disabled={page >= pageCount}
                className="rounded border border-outline-variant px-2.5 py-1 hover:bg-surface-highest disabled:opacity-40">Next</button>
            </div>
          </div>
        )}
      </Card>
    </div>
  );
}
