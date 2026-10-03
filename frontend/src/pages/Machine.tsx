// Machine — a node's hardware and what it is actually doing (F105).
//
// Layout is the "component tiles" direction: ONE card per subsystem, each
// pairing the static spec with its own live number, so the CPU model and the CPU
// load sit together instead of at opposite ends of the page. It reflows from
// four columns to one on the existing grid rules with no special-casing.
//
// The load-bearing behaviour is what happens when a host reports nothing. A VM,
// a Synology, or a locked-down firmware simply has no GPU, no sensors, no DMI —
// so those tiles are OMITTED rather than rendered empty, and the page explains
// why instead of showing a confident zero.
//
// COST: every refresh spawns a short-lived read-only container on the node, so
// this page polls slowly, only while visible (usePoll pauses on a hidden tab),
// and the server caches + de-duplicates probes across tabs.
import { useEffect, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import {
  ArrowLeft, Cpu, MemoryStick, MonitorCog, HardDrive, Network, Server,
  RefreshCw, Loader2, AlertTriangle, Info,
} from "lucide-react";
import { api, MachineResp, fmtBytes } from "../api";
import { Button, Card } from "../components/ui";
import { usePoll } from "../hooks/usePoll";

// Subsystem identity colours. Muted, disjoint from the status palette, and drawn
// ONLY as a rule under the subsystem name — never a dot or a swatch.
const ACCENT = {
  cpu: "#6f9fd8",
  mem: "#4fa39a",
  gpu: "#9184c9",
  disk: "#5fa8c4",
  net: "#7f92a8",
  platform: "#b98aa8",
} as const;

// Refresh cadence. Deliberately slower than the server's cache TTL so a visible
// page gets fresh numbers roughly every interval without ever queuing probes.
const POLL_MS = 15000;

function pct(used: number, total: number): number {
  return total > 0 ? Math.min(100, Math.max(0, (used / total) * 100)) : 0;
}

// fmtRate renders bytes/sec. Composes a number with a unit rather than
// formatting a locale-specific quantity, so it stays short in any translation.
function fmtRate(bps: number): string {
  if (!bps) return "0 B/s";
  return `${fmtBytes(bps)}/s`;
}

function fmtUptime(sec: number): string {
  if (!sec) return "—";
  const d = Math.floor(sec / 86400);
  const h = Math.floor((sec % 86400) / 3600);
  if (d > 0) return `${d} d ${h} h`;
  const m = Math.floor((sec % 3600) / 60);
  return `${h} h ${m} m`;
}

function Bar({ value, color }: { value: number; color: string }) {
  return (
    <div className="mt-1 flex h-[4px] overflow-hidden bg-surface-highest" aria-hidden>
      <span style={{ width: `${value}%`, background: color }} />
    </div>
  );
}

// Tile is the unit of this page: a subsystem name (carrying its colour as a
// rule), one headline live number, a proportion bar, and the static spec beneath.
function Tile({ name, color, icon: Icon, value, unit, fill, children }: {
  name: string;
  color: string;
  icon: typeof Cpu;
  value: string;
  unit?: string;
  fill: number;
  children: React.ReactNode;
}) {
  return (
    <Card className="p-4">
      <div className="mb-2 flex items-baseline justify-between gap-3">
        <div className="flex items-center gap-2 text-[12.5px] font-bold">
          <Icon size={14} className="shrink-0 text-on-surface-variant" />
          <span className="border-b-2 pb-0.5" style={{ borderColor: color }}>{name}</span>
        </div>
        <div className="tnum shrink-0 text-[19px] font-bold leading-none tracking-tight">
          {value}
          {unit && <span className="ml-0.5 text-[11px] font-semibold text-on-surface-variant">{unit}</span>}
        </div>
      </div>
      <Bar value={fill} color={color} />
      <div className="mt-2.5 font-mono text-[11.5px] leading-[1.7] text-on-surface-variant [word-break:break-word]">
        {children}
      </div>
    </Card>
  );
}

export default function Machine() {
  const { id = "" } = useParams();
  const navigate = useNavigate();
  const [d, setD] = useState<MachineResp | null>(null);
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);

  const load = async (force = false) => {
    if (force) setBusy(true);
    try {
      setD(await api.nodeMachine(id, force));
      setErr("");
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      if (force) setBusy(false);
    }
  };

  useEffect(() => { load(); /* eslint-disable-next-line react-hooks/exhaustive-deps */ }, [id]);
  // While a FIRST scan is running, poll quickly so the wait feels short — these
  // polls are answered from cache and never start another probe. Once there is
  // data, settle to the normal cadence.
  const scanning = !!d?.scanning && !d?.machine;
  usePoll(() => load(), scanning ? 2500 : POLL_MS, [id, scanning]);

  const m = d?.machine;
  const ident = m?.identity;
  // The server seeds every list, but a nil Go slice serialising to `null` is
  // exactly what crashed this page once (`warnings.length` on null). Normalise
  // here too so a stale server or an old cached payload can never do it again.
  const gpus = m?.gpus ?? [];
  const disks = m?.disks ?? [];
  const nets = m?.nets ?? [];
  const sensors = m?.sensors ?? [];
  const warnings = m?.warnings ?? [];

  // The machine's display name, in order of what a person would recognise: the
  // DMI product, then the Docker hostname, then the node's own name.
  const title = [ident?.vendor, ident?.product].filter(Boolean).join(" ")
    || d?.host?.name || d?.node_name || "Machine";

  return (
    <div>
      <div className="mb-5 flex flex-wrap items-start gap-4">
        <div className="min-w-0">
          <button
            onClick={() => navigate(`/servers/${id}`)}
            className="mb-1.5 flex items-center gap-1.5 text-xs text-on-surface-variant transition-colors hover:text-on-surface"
          >
            <ArrowLeft size={13} /> {d?.node_name || "Node"}
          </button>
          <h1 className="truncate text-[23px] font-bold tracking-tight">{title}</h1>
          <p className="mt-1 text-sm text-on-surface-variant">
            {[
              ident?.chassis_type,
              ident?.bios_date && `BIOS ${ident.bios_date}`,
              m?.platform.os_name || d?.host?.os,
              d?.host?.arch,
            ].filter(Boolean).join(" · ") || "Hardware and live utilisation"}
          </p>
        </div>
        <div className="ml-auto flex items-center gap-2.5">
          <span className="text-[11px] text-outline">
            {scanning
              ? "Scanning this machine…"
              : d?.refreshing
              ? "Refreshing…"
              : d && d.age_seconds > 3
                ? `As of ${d.age_seconds < 60 ? `${d.age_seconds} s` : `${Math.round(d.age_seconds / 60)} min`} ago`
                : `Live · every ${Math.round(POLL_MS / 1000)} s`}
          </span>
          <Button variant="secondary" onClick={() => load(true)} disabled={busy}>
            {busy ? <Loader2 size={14} className="animate-spin" /> : <RefreshCw size={14} />} Re-probe
          </Button>
        </div>
      </div>

      {err && (
        <Card className="mb-5 border-error/30 bg-error/[0.06] p-4 text-sm text-error">
          Couldn't load this machine: {err}
        </Card>
      )}

      {/* A first scan is running. This is NOT a failure and must not look like
          one — leading with red before anything has gone wrong is alarming and,
          as it turned out, usually wrong: the scan then succeeds. */}
      {d && !m && d.scanning && (
        <Card className="mb-5 p-4">
          <div className="mb-1 flex items-center gap-2 text-sm font-semibold text-primary">
            <Loader2 size={15} className="animate-spin" /> Scanning this machine…
          </div>
          <p className="text-sm text-on-surface-variant">
            Reading the hardware from the host. This takes a few seconds the first time — the results
            appear here automatically, and are cached afterwards so it stays instant.
          </p>
        </Card>
      )}

      {/* A scan actually ran and actually failed. Only now is a warning honest. */}
      {d && !m && !d.scanning && d.error && (
        <Card className="mb-5 border-warning/30 bg-warning/[0.07] p-4">
          <div className="mb-1 flex items-center gap-2 text-sm font-semibold text-warning">
            <AlertTriangle size={15} /> This machine's specifications couldn't be read
          </div>
          <p className="text-sm text-on-surface-variant">
            DockBack reads hardware by running a short, read-only container on the host with its
            <span className="font-mono"> /proc</span> and <span className="font-mono">/sys</span> mounted read-only.
            That didn't complete here, so only what the Docker API reports is shown below.
            Use <b>Re-probe</b> to try again.
          </p>
          <p className="mt-2 font-mono text-xs text-on-surface-variant">{d.error}</p>
        </Card>
      )}

      {m && m.complete === false && (
        <Card className="mb-5 p-4">
          <div className="mb-1 flex items-center gap-2 text-sm font-semibold text-warning">
            <Info size={15} /> Partial reading
          </div>
          <p className="text-sm text-on-surface-variant">
            This host was slow to answer, so the probe was cut short. Everything it did report is shown
            below; anything missing simply didn't respond in time. This is common on a NAS with disks in
            standby — waking them can block for longer than the probe waits.
          </p>
        </Card>
      )}

      {d && m && d.error && (
        <Card className="mb-5 border-warning/30 bg-warning/[0.07] p-4">
          <div className="mb-1 flex items-center gap-2 text-sm font-semibold text-warning">
            <AlertTriangle size={15} /> Showing the last successful reading
          </div>
          <p className="text-sm text-on-surface-variant">
            The most recent probe didn't complete, so the hardware below is from
            {" "}{d.age_seconds < 60 ? `${d.age_seconds} seconds` : `${Math.round(d.age_seconds / 60)} minutes`} ago.
            Specifications are still accurate; the live figures are not current.
          </p>
          <p className="mt-2 font-mono text-xs text-on-surface-variant">{d.error}</p>
        </Card>
      )}

      {!d ? (
        <div className="text-on-surface-variant">Probing machine…</div>
      ) : (
        <>
          <div className="grid items-start gap-4 [grid-template-columns:repeat(auto-fill,minmax(285px,1fr))]">
            {/* CPU — always present: even a bare Docker API knows the core count. */}
            <Tile
              name="Processor" color={ACCENT.cpu} icon={Cpu}
              value={m ? String(m.cpu.usage_pct) : "—"} unit={m ? "%" : undefined}
              fill={m ? m.cpu.usage_pct : 0}
            >
              {m?.cpu.model || d.host?.arch || "Not reported"}<br />
              {m
                ? <>{m.cpu.cores} cores / {m.cpu.threads} threads{m.cpu.cache_kb ? ` · ${Math.round(m.cpu.cache_kb / 1024)} MiB cache` : ""}</>
                : <>{d.host?.ncpu ?? 0} cores</>}
              {m && m.cpu.mhz_now > 0 && (
                <><br />{m.cpu.mhz_now.toFixed(0)} MHz now{m.cpu.mhz_max > 0 ? ` · ${m.cpu.mhz_max.toFixed(0)} MHz max` : ""}</>
              )}
              {m && <><br />load {m.cpu.load1.toFixed(2)} {m.cpu.load5.toFixed(2)} {m.cpu.load15.toFixed(2)}</>}
            </Tile>

            {/* Memory — host figures, which legitimately differ from the dashboard's
                container-sum. */}
            <Tile
              name="Memory" color={ACCENT.mem} icon={MemoryStick}
              value={m && m.memory.total_bytes > 0
                ? pct(m.memory.used_bytes, m.memory.total_bytes).toFixed(1)
                : "—"}
              unit={m && m.memory.total_bytes > 0 ? "%" : undefined}
              fill={m ? pct(m.memory.used_bytes, m.memory.total_bytes) : 0}
            >
              {m && m.memory.total_bytes > 0 ? (
                <>
                  {fmtBytes(m.memory.used_bytes)} used of {fmtBytes(m.memory.total_bytes)}<br />
                  {fmtBytes(m.memory.available_bytes)} available · {fmtBytes(m.memory.cached_bytes)} cached
                  {m.memory.swap_total_bytes > 0 && (
                    <><br />swap {fmtBytes(m.memory.swap_used_bytes)} of {fmtBytes(m.memory.swap_total_bytes)}</>
                  )}
                </>
              ) : (
                <>{fmtBytes(d.host?.mem_total || 0)} total<br />Usage not reported</>
              )}
            </Tile>

            {/* Graphics — omitted entirely when the host exposes none. */}
            {/* Graphics — one tile PER CARD when the driver publishes telemetry,
                so a GPU's temperature sits with its model rather than being
                buried in the sensors list. Falls back to a single summary tile
                when nothing but the PCI id is known. */}
            {gpus.map((g, i) => {
              const live = g.temp_c || g.usage_pct || g.fan_rpm || g.fan_pct || g.power_w;
              return (
                <Tile
                  key={g.card || `gpu-${i}`}
                  name={gpus.length > 1 ? `Graphics · ${g.driver || g.card || i}` : "Graphics"}
                  color={ACCENT.gpu} icon={MonitorCog}
                  value={g.usage_pct ? String(g.usage_pct) : g.temp_c ? String(g.temp_c) : "—"}
                  unit={g.usage_pct ? "%" : g.temp_c ? "°C" : undefined}
                  fill={g.usage_pct ? g.usage_pct : g.temp_c ? Math.min(100, g.temp_c) : 0}
                >
                  {g.name || `${g.vendor || "Unknown vendor"} ${g.device_id}`}
                  {g.driver && <><br />driver {g.driver}</>}
                  {g.temp_c ? <><br />{g.temp_c} °C</> : null}
                  {g.fan_rpm ? <> · fan {g.fan_rpm} rpm</> : null}
                  {g.fan_pct ? <> · fan {g.fan_pct} %</> : null}
                  {g.power_w ? <> · {g.power_w} W</> : null}
                  {g.vram_total_bytes ? (
                    <><br />VRAM {fmtBytes(g.vram_used_bytes || 0)} of {fmtBytes(g.vram_total_bytes)}</>
                  ) : null}
                  {!g.name && (
                    <div className="mt-1 text-outline">
                      Install <span className="font-mono">hwdata</span> or <span className="font-mono">pciutils</span> on
                      this host for the full model name.
                    </div>
                  )}
                  {/* Be specific about WHY a card shows identity only — the two
                      causes need different actions from the operator. */}
                  {!live && (
                    <div className="mt-1 text-outline">
                      {g.driver === "nvidia" ? (
                        <>Live figures need the <b className="text-on-surface-variant">NVIDIA Container Toolkit</b> on this
                        host. The driver alone publishes none to the kernel — <span className="font-mono">nvidia-smi</span> is
                        the only source, and it has to be reachable from a container.</>
                      ) : (
                        <>This driver publishes no temperature or utilisation to the kernel.</>
                      )}
                    </div>
                  )}
                </Tile>
              );
            })}

            {/* Storage — one tile per physical device, so an 8-disk server reads
                as eight tiles rather than one truncated list. */}
            {disks.map((disk) => {
              // Free space is a FILESYSTEM property: a disk may carry several,
              // or none at all. When it carries some, the headline becomes what
              // is left — the number actually being asked for — and the bar shows
              // how full it is rather than how busy.
              const hasFS = !!disk.fs_total_bytes;
              const usedPct = hasFS ? (disk.fs_used_bytes || 0) / (disk.fs_total_bytes || 1) * 100 : 0;
              const io = disk.read_bps + disk.write_bps;
              return (
                <Tile
                  key={disk.name}
                  name={`Disk · ${disk.name}`} color={ACCENT.disk} icon={HardDrive}
                  value={hasFS ? fmtBytes(disk.fs_free_bytes || 0) : fmtBytes(io)}
                  unit={hasFS ? "free" : "/s"}
                  fill={hasFS ? usedPct : Math.min(100, (io / (200 * 1024 * 1024)) * 100)}
                >
                  {disk.model || "Model not reported"}<br />
                  {fmtBytes(disk.size_bytes)} · {disk.rotational ? "HDD" : "SSD"}<br />
                  {hasFS ? (
                    <>
                      {fmtBytes(disk.fs_used_bytes || 0)} used of {fmtBytes(disk.fs_total_bytes || 0)} ({usedPct.toFixed(0)} %)<br />
                      {(disk.filesystems || []).map((fs) => (
                        <span key={fs.mount} className="block truncate">
                          {fs.mount} · {fmtBytes(fs.free_bytes)} free
                        </span>
                      ))}
                    </>
                  ) : (
                    <span className="text-outline">Nothing mounted from this disk</span>
                  )}
                  <br />read {fmtRate(disk.read_bps)} · write {fmtRate(disk.write_bps)}
                </Tile>
              );
            })}

            {/* Network — physical/virtual interfaces the host owns. */}
            {nets.map((n) => (
              <Tile
                key={n.name}
                name={`Network · ${n.name}`} color={ACCENT.net} icon={Network}
                value={fmtBytes(n.rx_bps + n.tx_bps)} unit="/s"
                fill={n.speed_mbps > 0
                  ? Math.min(100, (((n.rx_bps + n.tx_bps) * 8) / (n.speed_mbps * 1e6)) * 100)
                  : 0}
              >
                {n.speed_mbps > 0 ? `${n.speed_mbps} Mb/s link` : "Link speed not reported"} · {n.state || "unknown"}<br />
                down {fmtRate(n.rx_bps)}<br />
                up {fmtRate(n.tx_bps)}
              </Tile>
            ))}

            {/* Platform — always present; the one tile that can be built from the
                Docker API alone. */}
            <Tile
              name="Platform" color={ACCENT.platform} icon={Server}
              value={m?.platform.uptime_seconds ? fmtUptime(m.platform.uptime_seconds).split(" ")[0] : "—"}
              unit={m?.platform.uptime_seconds ? "d up" : undefined}
              fill={100}
            >
              {m?.platform.os_name || d.host?.os || "OS not reported"}<br />
              {m?.platform.kernel || d.host?.kernel || ""}{d.host?.arch ? ` · ${d.host.arch}` : ""}<br />
              Docker {d.docker.version || "—"} · {d.docker.running}/{d.docker.containers} running<br />
              {d.docker.images} images · {d.docker.volumes} volumes
              {m?.platform.battery_pct ? (
                <><br />battery {m.platform.battery_pct} %{m.platform.on_ac ? " · on AC" : ""}</>
              ) : null}
              {ident?.board && <><br />board {ident.board}</>}
            </Tile>

            {/* Sensors — a single tile listing every plausible reading, headlined
                by the hottest, because that is the one that matters. */}
            {m && sensors.length > 0 && (
              <Tile
                name="Temperature" color={ACCENT.platform} icon={AlertTriangle}
                value={String(Math.max(...sensors.map((s) => s.celsius)))} unit="°C"
                fill={Math.min(100, (Math.max(...sensors.map((s) => s.celsius)) / 100) * 100)}
              >
                {sensors.map((s) => (
                  <div key={s.label}>{s.label} · {s.celsius} °C</div>
                ))}
              </Tile>
            )}
          </div>

          {/* Explain a sparse result rather than leaving the operator to wonder
              whether the probe half-failed. */}
          {warnings.length > 0 && (
            <Card className="mt-5 p-4">
              <div className="mb-1.5 flex items-center gap-2 text-sm font-semibold">
                <Info size={15} className="text-primary" /> What this host doesn't report
              </div>
              <ul className="list-inside list-disc space-y-1 text-[13px] text-on-surface-variant">
                {warnings.map((wn, i) => <li key={i}>{wn}</li>)}
              </ul>
            </Card>
          )}

          <p className="mt-4 text-[11px] leading-relaxed text-outline">
            Hardware is read by a short-lived container on this host with <span className="font-mono">/proc</span> and
            <span className="font-mono"> /sys</span> mounted read-only — unprivileged, writes nothing, removed immediately.
            Serial numbers are deliberately never read. CPU and memory here are the <b>whole machine</b>, so they will read
            higher than the container totals on the dashboard.
          </p>
        </>
      )}
    </div>
  );
}
