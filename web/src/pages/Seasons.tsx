import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Search, X } from "lucide-react";
import { useEffect, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { Candidate, Season, api } from "../api";
import { commas } from "../components/charts";

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
                    {c.dual && <span className="pill text-accent">dual audio</span>}
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

type Sort = "score" | "title" | "files" | "size";
const PAGE = 50;

function Chips({ items, max = 3 }: { items: string[]; max?: number }) {
  return (
    <span className="flex flex-wrap gap-1" title={items.join(", ")}>
      {items.slice(0, max).map((i) => <span key={i} className="pill text-slate-300">{i}</span>)}
      {items.length > max && <span className="pill text-muted">+{items.length - max}</span>}
    </span>
  );
}

function SortTh({ id, sort, onSort, children, right = false }: { id: Sort; sort: Sort; onSort: (s: Sort) => void; children: string; right?: boolean }) {
  return (
    <th className={right ? "text-right" : ""} aria-sort={sort === id ? (id === "title" ? "ascending" : "descending") : "none"}>
      <button className={`uppercase tracking-wide hover:text-white ${sort === id ? "text-white" : ""}`} onClick={() => onSort(id)}>
        {children}{sort === id ? (id === "title" ? " ▴" : " ▾") : ""}
      </button>
    </th>
  );
}

export default function SeasonsPage() {
  const [sp, setSp] = useSearchParams();
  const q = sp.get("q") ?? "";
  const [inst, setInst] = useState("");
  const [anime, setAnime] = useState(sp.get("anime") === "all" ? "all" : "hide");
  const [dub, setDub] = useState(sp.get("dub") === "1");
  const [sort, setSort] = useState<Sort>("score");
  const [page, setPage] = useState(0);
  const [sel, setSel] = useState<Season | null>(null);
  const insts = useQuery({ queryKey: ["instances"], queryFn: api.instances });
  const params = new URLSearchParams({ anime, limit: String(PAGE), offset: String(page * PAGE), sort });
  if (q) params.set("q", q);
  if (inst) params.set("inst", inst);
  if (dub) params.set("dub", "1");
  const seasons = useQuery({ queryKey: ["seasons", q, inst, anime, dub, sort, page], queryFn: () => api.seasons(params), placeholderData: keepPreviousData });
  const name = (id: number) => insts.data?.find((i) => i.id === id)?.name ?? id;
  const total = seasons.data?.total ?? 0;
  const rows = seasons.data?.seasons ?? [];
  const reset = () => setPage(0);
  const onSort = (n: Sort) => { setSort(n); reset(); };
  useEffect(() => { setPage((p) => Math.min(p, Math.max(0, Math.ceil(total / PAGE) - 1))); }, [total]);
  useEffect(() => { setPage(0); }, [q]);
  return (
    <div className="space-y-4">
      <h1 className="page-title">Seasons</h1>
      <p className="subtle">Seasons built entirely from single-episode grabs. The messiest (most release groups, qualities and sources) come first.</p>
      <div className="flex flex-wrap items-center gap-2">
        <div className="relative w-full sm:w-64">
          <Search size={14} className="pointer-events-none absolute left-3 top-2.5 text-muted" />
          <input className="input pl-8" placeholder="Search shows" aria-label="Search shows" value={q}
            onChange={(e) => { const n = new URLSearchParams(sp); if (e.target.value) n.set("q", e.target.value); else n.delete("q"); setSp(n, { replace: true }); reset(); }} />
        </div>
        <select className="input w-full sm:w-44" aria-label="Instance" value={inst} onChange={(e) => { setInst(e.target.value); reset(); }}>
          <option value="">All instances</option>{insts.data?.map((i) => <option key={i.id} value={i.id}>{i.name}</option>)}
        </select>
        <select className="input w-full sm:w-40" aria-label="Show type" value={anime} onChange={(e) => { setAnime(e.target.value); reset(); }}>
          <option value="hide">TV only</option><option value="all">TV + anime</option><option value="only">Anime only</option>
        </select>
        <label className="flex items-center gap-2 text-sm text-muted" title="Finished anime seasons with files that have no English audio">
          <input type="checkbox" checked={dub} onChange={(e) => { setDub(e.target.checked); reset(); }} /> Needs English dub
        </label>
        <span className="num text-sm text-muted">{total > 0 ? `${page * PAGE + 1}–${Math.min(total, page * PAGE + rows.length)} of ${commas(total)}` : seasons.isLoading ? "Loading…" : "0 seasons"}</span>
      </div>

      <div className="card hidden overflow-x-auto md:block">
        <table className="w-full">
          <thead><tr>
            <SortTh id="title" sort={sort} onSort={onSort}>Show</SortTh><th>Instance</th><th>Season</th><SortTh id="files" sort={sort} onSort={onSort} right>Files</SortTh>
            <SortTh id="score" sort={sort} onSort={onSort}>Release groups</SortTh><th>Qualities</th><SortTh id="size" sort={sort} onSort={onSort} right>Size</SortTh><th />
          </tr></thead>
          <tbody>
            {rows.map((s) => (
              <tr key={`${s.instId}-${s.seriesId}-${s.season}`}>
                <td className="max-w-[16rem] truncate font-medium" title={s.title}>{s.title}</td>
                <td className="text-muted">{name(s.instId)}</td>
                <td className="num">S{String(s.season).padStart(2, "0")}</td>
                <td className="num text-right">{s.files}</td>
                <td><span className="num mr-2 font-semibold">{s.groups.length}</span><span className="text-xs text-muted" title={s.groups.join(", ")}>{s.groups.slice(0, 3).join(", ")}{s.groups.length > 3 ? ` +${s.groups.length - 3}` : ""}</span></td>
                <td><Chips items={s.qualities} /></td>
                <td className="num whitespace-nowrap text-right">{s.gb.toFixed(1)} GB</td>
                <td className="text-right"><button className="btn btn-sm" onClick={() => setSel(s)}>Find pack</button></td>
              </tr>
            ))}
            {rows.length === 0 && !seasons.isLoading && <tr><td colSpan={8} className="text-muted">Nothing matches. Add an instance and run a scan in Settings, or loosen the filters.</td></tr>}
          </tbody>
        </table>
      </div>

      <ul className="space-y-2 md:hidden">
        {rows.map((s) => (
          <li key={`${s.instId}-${s.seriesId}-${s.season}`} className="card space-y-2 p-3">
            <div className="flex items-start justify-between gap-2">
              <div className="min-w-0">
                <div className="truncate font-medium">{s.title} <span className="num text-muted">S{String(s.season).padStart(2, "0")}</span></div>
                <div className="num text-xs text-muted">{s.files} files · {s.groups.length} groups · {s.gb.toFixed(1)} GB · {name(s.instId)}</div>
              </div>
              <button className="btn btn-sm shrink-0" onClick={() => setSel(s)}>Find pack</button>
            </div>
            <Chips items={s.qualities} />
          </li>
        ))}
      </ul>

      {total > PAGE && (
        <div className="flex items-center justify-between">
          <button className="btn" disabled={page === 0} onClick={() => setPage(page - 1)}>← Previous</button>
          <span className="num text-xs text-muted">Page {page + 1} of {Math.ceil(total / PAGE)}</span>
          <button className="btn" disabled={(page + 1) * PAGE >= total} onClick={() => setPage(page + 1)}>Next →</button>
        </div>
      )}
      {sel && <Drawer s={sel} onClose={() => setSel(null)} />}
    </div>
  );
}
