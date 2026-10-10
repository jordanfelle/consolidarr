import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Search, X } from "lucide-react";
import { useState } from "react";
import { Candidate, Season, api } from "../api";

function Drawer({ s, onClose }: { s: Season; onClose: () => void }) {
  const qc = useQueryClient();
  const cands = useQuery({ queryKey: ["cands", s.instId, s.seriesId, s.season], queryFn: () => api.candidates(s) });
  const grab = useMutation({
    mutationFn: (c: Candidate) => api.grab(s, c),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["jobs"] }); onClose(); },
  });
  return (
    <div className="fixed inset-0 z-20 flex justify-end bg-black/60" onClick={onClose}>
      <div className="h-full w-full max-w-3xl overflow-y-auto border-l border-line bg-bg p-5" onClick={(e) => e.stopPropagation()}>
        <div className="mb-4 flex items-start justify-between">
          <div>
            <div className="text-lg font-semibold">{s.title} · Season {s.season}</div>
            <div className="text-sm text-muted">{s.files} files from {s.groups.length} groups ({s.groups.join(", ")}) · {s.qualities.join(", ")}</div>
          </div>
          <button className="btn" onClick={onClose}><X size={14} /></button>
        </div>
        <div className="mb-3 rounded-md border border-warn/40 p-3 text-sm text-warn">
          Grabbing a pack tracks it as a replacement. Your current {s.files} files are only deleted after the download finishes, probes clean and covers every episode you have.
        </div>
        {cands.isLoading && <div className="text-muted">Searching indexers…</div>}
        {cands.error && <div className="text-bad">{(cands.error as Error).message}</div>}
        {grab.error && <div className="text-bad">{(grab.error as Error).message}</div>}
        <div className="space-y-2">
          {cands.data?.map((c) => (
            <div key={c.guid} className={`card p-3 ${c.picked ? "border-accent" : ""}`}>
              <div className="flex items-center justify-between gap-3">
                <div className="min-w-0">
                  <div className="truncate text-sm" title={c.title}>{c.title}</div>
                  <div className="mt-1 flex flex-wrap gap-2 text-xs text-muted">
                    <span className="pill">{c.quality.quality.name}</span>
                    <span>{(c.size / 1e9).toFixed(1)} GB</span><span>{c.seeders ?? 0} seeders</span><span>{c.indexer}</span>
                    {c.picked && <span className="pill text-accent">auto-pick</span>}
                  </div>
                  {c.blocking.length > 0 && <div className="mt-1 text-xs text-bad">{c.blocking[0]}</div>}
                </div>
                <button className="btn btn-primary shrink-0" disabled={grab.isPending} onClick={() => grab.mutate(c)}>Grab</button>
              </div>
            </div>
          ))}
          {cands.data?.length === 0 && <div className="text-muted">No full-season releases found.</div>}
        </div>
      </div>
    </div>
  );
}

export default function SeasonsPage() {
  const [q, setQ] = useState("");
  const [inst, setInst] = useState("");
  const [anime, setAnime] = useState("hide");
  const [sel, setSel] = useState<Season | null>(null);
  const insts = useQuery({ queryKey: ["instances"], queryFn: api.instances });
  const params = new URLSearchParams({ anime, limit: "300" });
  if (q) params.set("q", q);
  if (inst) params.set("inst", inst);
  const seasons = useQuery({ queryKey: ["seasons", q, inst, anime], queryFn: () => api.seasons(params) });
  const name = (id: number) => insts.data?.find((i) => i.id === id)?.name ?? id;
  return (
    <div className="space-y-4">
      <h1 className="text-xl font-semibold">Seasons</h1>
      <p className="text-sm text-muted">Seasons built entirely from single-episode grabs, messiest first (more release groups, qualities and sources rank higher).</p>
      <div className="flex flex-wrap gap-2">
        <div className="relative"><Search size={14} className="absolute left-3 top-2.5 text-muted" />
          <input className="input w-64 pl-8" placeholder="Search shows" value={q} onChange={(e) => setQ(e.target.value)} /></div>
        <select className="input w-44" value={inst} onChange={(e) => setInst(e.target.value)}>
          <option value="">All instances</option>{insts.data?.map((i) => <option key={i.id} value={i.id}>{i.name}</option>)}
        </select>
        <select className="input w-40" value={anime} onChange={(e) => setAnime(e.target.value)}>
          <option value="hide">TV only</option><option value="">TV + anime</option><option value="only">Anime only</option>
        </select>
        <span className="self-center text-sm text-muted">{seasons.data ? `${seasons.data.seasons.length} of ${seasons.data.total}` : ""}</span>
      </div>
      <div className="card overflow-x-auto">
        <table className="w-full">
          <thead><tr><th>Show</th><th>Instance</th><th>Season</th><th>Files</th><th>Groups</th><th>Qualities</th><th>Size</th><th /></tr></thead>
          <tbody>
            {seasons.data?.seasons.map((s) => (
              <tr key={`${s.instId}-${s.seriesId}-${s.season}`}>
                <td>{s.title}</td><td className="text-muted">{name(s.instId)}</td><td>S{String(s.season).padStart(2, "0")}</td>
                <td>{s.files}</td>
                <td className="max-w-xs truncate text-muted" title={s.groups.join(", ")}>{s.groups.length} · {s.groups.join(", ")}</td>
                <td className="text-muted">{s.qualities.join(", ")}</td><td>{s.gb.toFixed(1)} GB</td>
                <td className="text-right"><button className="btn" onClick={() => setSel(s)}>Find pack</button></td>
              </tr>
            ))}
            {seasons.data?.seasons.length === 0 && <tr><td colSpan={8} className="text-muted">Nothing flagged. Add an instance and run a scan in Settings.</td></tr>}
          </tbody>
        </table>
      </div>
      {sel && <Drawer s={sel} onClose={() => setSel(null)} />}
    </div>
  );
}
