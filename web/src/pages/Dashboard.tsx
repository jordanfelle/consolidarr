import { useQuery } from "@tanstack/react-query";
import { api } from "../api";

function Stat({ label, value, hint }: { label: string; value: string | number; hint?: string }) {
  return (
    <div className="card p-4">
      <div className="text-xs uppercase tracking-wide text-muted">{label}</div>
      <div className="mt-1 text-2xl font-semibold">{value}</div>
      {hint && <div className="mt-1 text-xs text-muted">{hint}</div>}
    </div>
  );
}

export default function Dashboard() {
  const st = useQuery({ queryKey: ["status"], queryFn: api.status, refetchInterval: 10000 });
  const insts = useQuery({ queryKey: ["instances"], queryFn: api.instances, refetchInterval: 10000 });
  const jobs = useQuery({ queryKey: ["jobs", true], queryFn: () => api.jobs(true), refetchInterval: 10000 });
  const s = st.data?.settings;
  return (
    <div className="space-y-6">
      <h1 className="text-xl font-semibold">Dashboard</h1>
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Stat label="TV seasons to consolidate" value={st.data?.flaggedTV ?? "–"} />
        <Stat label="Anime seasons" value={st.data?.flaggedAnime ?? "–"} hint={s?.includeAnime ? "included" : "hidden"} />
        <Stat label="In flight" value={st.data?.active ?? "–"} hint={s ? `max ${s.maxInflight}` : undefined} />
        <Stat label="Scheduler" value={s ? (s.schedulerEnabled ? (s.dryRun ? "Dry run" : "Live") : "Off") : "–"} />
      </div>
      {st.data?.once?.active && (
        <div className="card border-accent/40 p-3 text-sm">
          One-off run in progress: {st.data.once.started} of {st.data.once.limit} seasons started{s?.dryRun ? " (dry run: only logging)" : ""}.
        </div>
      )}
      {(s?.schedulerEnabled || st.data?.once?.active) && s?.dryRun && (
        <div className="card border-warn/40 p-3 text-sm text-warn">
          Dry run is on: the scheduler only logs what it would grab. Turn it off in Settings when you are happy with the picks.
        </div>
      )}
      <div className="card">
        <div className="border-b border-line px-4 py-3 text-sm font-medium">Instances</div>
        <table className="w-full">
          <thead><tr><th>Name</th><th>URL</th><th>Status</th><th>Last scan</th></tr></thead>
          <tbody>
            {insts.data?.map((i) => (
              <tr key={i.id}>
                <td>{i.name}</td><td className="text-muted">{i.url}</td>
                <td>{!i.enabled ? <span className="pill text-muted">disabled</span> : i.health ? <span className="pill text-bad" title={i.health}>unhealthy</span> : i.scanning ? <span className="pill text-warn">scanning</span> : <span className="pill text-good">ok</span>}</td>
                <td className="text-muted">{i.lastScan ? new Date(i.lastScan * 1000).toLocaleString() : "never"}</td>
              </tr>
            ))}
            {insts.data?.length === 0 && <tr><td colSpan={4} className="text-muted">No Sonarr instances yet. Add one in Settings.</td></tr>}
          </tbody>
        </table>
      </div>
      <div className="card">
        <div className="border-b border-line px-4 py-3 text-sm font-medium">Active replacements</div>
        <table className="w-full">
          <thead><tr><th>Season</th><th>Release</th><th>Status</th></tr></thead>
          <tbody>
            {jobs.data?.map((j) => (
              <tr key={j.id}><td>{j.title} S{String(j.season).padStart(2, "0")}</td><td className="max-w-md truncate text-muted">{j.release}</td>
                <td><span className="pill">{j.status}{j.status === "downloading" ? ` ${j.progress}%` : ""}</span></td></tr>
            ))}
            {jobs.data?.length === 0 && <tr><td colSpan={3} className="text-muted">Nothing in flight.</td></tr>}
          </tbody>
        </table>
      </div>
    </div>
  );
}
