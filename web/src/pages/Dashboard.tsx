import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../api";
import { ChartCard, Columns, Delta, HBars, Legend, LineChart, Meter, Stacked, StatTile, commas, compact } from "../components/charts";

const RANGES = [
  { id: "24h", label: "24 hours" },
  { id: "7d", label: "7 days" },
  { id: "30d", label: "30 days" },
  { id: "90d", label: "90 days" },
  { id: "all", label: "All time" },
];

const pad = (n: number) => String(n).padStart(2, "0");
const ago = (ts: number) => {
  const s = Math.max(0, Math.floor(Date.now() / 1000 - ts));
  if (s < 90) return "just now";
  if (s < 5400) return `${Math.round(s / 60)} min ago`;
  if (s < 129600) return `${Math.round(s / 3600)} h ago`;
  return `${Math.round(s / 86400)} d ago`;
};

export default function Dashboard() {
  const [range, setRange] = useState("7d");
  const st = useQuery({ queryKey: ["status"], queryFn: api.status, refetchInterval: 10000 });
  const m = useQuery({ queryKey: ["metrics", range], queryFn: () => api.metrics(range), refetchInterval: 30000, placeholderData: keepPreviousData });
  const insts = useQuery({ queryKey: ["instances"], queryFn: api.instances, refetchInterval: 15000 });
  const jobs = useQuery({ queryKey: ["jobs", true], queryFn: () => api.jobs(true), refetchInterval: 10000 });
  const events = useQuery({ queryKey: ["events"], queryFn: api.events, refetchInterval: 15000 });
  const attn = useQuery({ queryKey: ["attention"], queryFn: api.attention, refetchInterval: 60000, staleTime: 60000 });
  const s = st.data?.settings;
  const d = m.data;
  const stale = m.isFetching && m.isPlaceholderData; // keep the frame while a new range loads

  const snaps = d?.snapshots ?? [];
  const first = snaps[0];
  const cur = d?.current;
  const packShare = (x?: { packFiles: number; files: number }) => (x && x.files > 0 ? (100 * x.packFiles) / x.files : 0);
  const left = cur ? cur.flaggedTV + (s?.includeAnime ? cur.flaggedAnime : 0) : undefined;
  const leftThen = first ? first.flaggedTV + (s?.includeAnime ? first.flaggedAnime : 0) : undefined;
  const sparkLeft = snaps.slice(-12).map((x) => x.flaggedTV + (s?.includeAnime ? x.flaggedAnime : 0));
  const sparkPack = snaps.slice(-12).map((x) => packShare(x));

  const lineLeft = [
    { name: "TV", color: "var(--series-1)", points: snaps.map((x) => ({ t: x.ts, v: x.flaggedTV })) },
    ...(s?.includeAnime ? [{ name: "Anime", color: "var(--series-2)", points: snaps.map((x) => ({ t: x.ts, v: x.flaggedAnime })) }] : []),
  ];
  const linePack = [{ name: "Share of files from season packs", color: "var(--series-1)", points: snaps.map((x) => ({ t: x.ts, v: packShare(x) })) }];
  const days = (d?.perDay ?? []).slice(-30).map((x) => ({
    label: new Date(`${x.day}T00:00:00`).toLocaleDateString([], { month: "short", day: "numeric" }),
    value: x.done,
    sub: `${x.failed} failed · ${x.gb.toFixed(1)} GB downloaded`,
  }));
  const comp = cur
    ? [
        { name: "Season pack", value: cur.packFiles, color: "var(--series-1)" },
        { name: "Single episode", value: cur.singleFiles, color: "var(--series-2)" },
        { name: "Multi-episode", value: cur.multiFiles, color: "var(--series-3)" },
        { name: "Unknown (older imports)", value: cur.unknownFiles, color: "var(--viz-quiet)" },
      ]
    : [];

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="page-title">Dashboard</h1>
        <div role="group" aria-label="Time range" className="flex rounded-md border border-line p-0.5">
          {RANGES.map((r) => (
            <button key={r.id} aria-pressed={range === r.id} onClick={() => setRange(r.id)}
              className={`rounded px-3 py-1 text-xs ${range === r.id ? "bg-accent/20 text-white" : "text-muted hover:text-white"}`}>{r.label}</button>
          ))}
        </div>
      </div>

      {st.data?.once?.active && (
        <div className="card border-accent/40 p-3 text-sm">
          One-off run in progress: {st.data.once.started} of {st.data.once.limit} seasons started{s?.dryRun ? " (dry run: only logging)" : ""}.
        </div>
      )}
      {(s?.schedulerEnabled || st.data?.once?.active) && s?.dryRun && (
        <div className="card border-warn/40 p-3 text-sm text-warn">
          Dry run is on: nothing is grabbed or deleted, it only logs what it would do. Turn it off in Settings when you are happy with the picks.
        </div>
      )}

      <div className="grid grid-cols-2 gap-3 lg:grid-cols-5">
        <div className="col-span-2 lg:col-span-1">
          <StatTile hero label="Seasons left" value={left === undefined ? "–" : commas(left)} spark={sparkLeft}
            hint="Seasons built entirely from single-episode grabs, still waiting for a season pack"
            footer={<Delta now={left ?? 0} before={leftThen} upIsGood={false} label={`in ${RANGES.find((r) => r.id === range)?.label.toLowerCase()}`} />} />
        </div>
        <StatTile label="Files from packs" value={cur ? `${packShare(cur).toFixed(1)}%` : "–"} spark={sparkPack}
          hint="Share of all episode files that came from a season pack"
          footer={<span className="text-xs text-muted">{cur ? `${commas(cur.packFiles)} of ${commas(cur.files)} files` : ""}</span>} />
        <StatTile label="Replaced" value={d ? commas(d.totals.done) : "–"}
          footer={<span className="text-xs text-muted">{d ? `${d.totals.gb.toFixed(0)} GB of packs${d.totals.failed ? ` · ${d.totals.failed} failed` : ""}` : ""}</span>} />
        <StatTile label="In flight" value={d ? d.totals.inflight : "–"}
          footer={s ? <div className="space-y-1"><Meter value={d?.totals.inflight ?? 0} max={s.maxInflight} label="Packs in flight" /><div className="text-xs text-muted">of {s.maxInflight} allowed</div></div> : undefined} />
        <Link to="/attention" className="block focus-visible:rounded-lg">
          <StatTile label="Needs attention" value={attn.data ? attn.data.items.length : "–"}
            hint="Downloaded packs Sonarr will not import by itself"
            footer={<span className={`text-xs ${attn.data?.items.length ? "text-warn" : "text-muted"}`}>{attn.data?.items.length ? "▲ Review these →" : "Nothing stuck"}</span>} />
        </Link>
      </div>

      <div className={`grid gap-4 lg:grid-cols-2 ${stale ? "opacity-60 transition-opacity" : ""}`}>
        <ChartCard title="Seasons still to consolidate" subtitle="Each point is a scan or a finished replacement; lower is better"
          legend={lineLeft.length > 1 ? <Legend items={lineLeft.map((l) => ({ name: l.name, color: l.color }))} /> : undefined}
          table={{ head: ["Time", ...lineLeft.map((l) => l.name)], rows: snaps.map((x) => [new Date(x.ts * 1000).toLocaleString(), x.flaggedTV, ...(s?.includeAnime ? [x.flaggedAnime] : [])]) }}>
          <LineChart series={lineLeft} />
        </ChartCard>

        <ChartCard title="Files that came from season packs" subtitle="Share of every episode file in the library; higher is better"
          table={{ head: ["Time", "Pack share %", "Pack files", "All files"], rows: snaps.map((x) => [new Date(x.ts * 1000).toLocaleString(), packShare(x).toFixed(2), x.packFiles, x.files]) }}>
          <LineChart series={linePack} percent />
        </ChartCard>

        <ChartCard title="Replacements per day" subtitle="Seasons finished each day (last 30 days with activity)"
          table={{ head: ["Day", "Done", "Failed", "GB"], rows: (d?.perDay ?? []).map((x) => [x.day, x.done, x.failed, x.gb.toFixed(1)]) }}>
          {days.length > 0 ? <Columns data={days} /> : <div className="flex min-h-32 items-center justify-center rounded-md border border-dashed border-line px-4 text-center text-sm text-muted">No replacements finished in this range yet.</div>}
        </ChartCard>

        <ChartCard title="How scattered the remaining seasons are" subtitle="Seasons by the number of different release groups their episodes came from"
          table={{ head: ["Release groups", "Seasons"], rows: (d?.groupBuckets ?? []).map((b) => [b.label, b.count]) }}>
          <HBars rows={(d?.groupBuckets ?? []).map((b) => ({ label: b.label, value: b.count }))} />
        </ChartCard>

        <ChartCard title="Where every episode file came from" subtitle="Whole library, by how Sonarr received it" className="lg:col-span-2"
          table={{ head: ["Source", "Files"], rows: comp.map((c) => [c.name, c.value]) }}>
          <Stacked parts={comp} />
        </ChartCard>
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        <div className="card overflow-hidden">
          <div className="flex items-center justify-between border-b border-line px-4 py-3 text-sm font-medium">
            <span>Messiest seasons</span><Link className="text-xs text-accent hover:underline" to="/seasons">All seasons →</Link>
          </div>
          <div className="overflow-x-auto">
            <table className="w-full">
              <thead><tr><th>Show</th><th>Season</th><th className="text-right">Files</th><th className="text-right">Groups</th><th className="text-right">Size</th></tr></thead>
              <tbody>
                {d?.top.map((t) => (
                  <tr key={`${t.instId}-${t.seriesId}-${t.season}`}>
                    <td className="max-w-[14rem] truncate"><Link className="hover:text-accent" to={`/seasons?q=${encodeURIComponent(t.title)}`}>{t.title}</Link></td>
                    <td className="num">S{pad(t.season)}</td><td className="num text-right">{t.files}</td><td className="num text-right">{t.groups}</td>
                    <td className="num whitespace-nowrap text-right text-muted">{t.gb.toFixed(1)} GB</td>
                  </tr>
                ))}
                {d?.top.length === 0 && <tr><td colSpan={5} className="text-muted">Nothing flagged. Run a scan in Settings.</td></tr>}
              </tbody>
            </table>
          </div>
        </div>

        <div className="card overflow-hidden">
          <div className="flex items-center justify-between border-b border-line px-4 py-3 text-sm font-medium">
            <span>Recent activity</span><Link className="text-xs text-accent hover:underline" to="/events">All activity →</Link>
          </div>
          <ul className="divide-y divide-line text-xs">
            {events.data?.slice(0, 8).map((e) => (
              <li key={e.id} className="flex gap-3 px-4 py-2">
                <span className="w-16 shrink-0 text-muted">{ago(e.ts)}</span>
                <span className={e.level === "error" ? "text-bad" : e.level === "warn" ? "text-warn" : "text-slate-300"}>{e.msg}</span>
              </li>
            ))}
            {events.data?.length === 0 && <li className="px-4 py-3 text-muted">No activity yet.</li>}
          </ul>
        </div>
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        <div className="card overflow-hidden">
          <div className="border-b border-line px-4 py-3 text-sm font-medium">Active replacements</div>
          <div className="overflow-x-auto">
            <table className="w-full">
              <thead><tr><th>Season</th><th>Release</th><th>Status</th></tr></thead>
              <tbody>
                {jobs.data?.map((j) => (
                  <tr key={j.id}>
                    <td className="whitespace-nowrap">{j.title} S{pad(j.season)}</td>
                    <td className="max-w-[16rem] truncate text-muted" title={j.release}>{j.release}</td>
                    <td><span className="pill">{j.status}{j.status === "downloading" ? ` ${j.progress}%` : ""}</span></td>
                  </tr>
                ))}
                {jobs.data?.length === 0 && <tr><td colSpan={3} className="text-muted">Nothing in flight.</td></tr>}
              </tbody>
            </table>
          </div>
        </div>

        <div className="card overflow-hidden">
          <div className="border-b border-line px-4 py-3 text-sm font-medium">Sonarr instances</div>
          <div className="overflow-x-auto">
            <table className="w-full">
              <thead><tr><th>Name</th><th>Status</th><th>Last scan</th></tr></thead>
              <tbody>
                {insts.data?.map((i) => (
                  <tr key={i.id}>
                    <td>{i.name}</td>
                    <td>{!i.enabled ? <span className="pill text-muted">disabled</span> : i.health ? <span className="pill text-bad" title={i.health}>⚠ unhealthy</span> : i.scanning ? <span className="pill text-warn">scanning</span> : <span className="pill text-good">✓ ok</span>}</td>
                    <td className="whitespace-nowrap text-muted">{i.lastScan ? ago(i.lastScan) : "never"}</td>
                  </tr>
                ))}
                {insts.data?.length === 0 && <tr><td colSpan={3} className="text-muted">No Sonarr instances yet. Add one in Settings.</td></tr>}
              </tbody>
            </table>
          </div>
        </div>
      </div>
      <div className="sr-only" aria-live="polite">{compact(left ?? 0)} seasons left</div>
    </div>
  );
}
