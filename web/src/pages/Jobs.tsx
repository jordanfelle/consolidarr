import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { api } from "../api";

const tone: Record<string, string> = { done: "text-good", failed: "text-bad", lost: "text-warn", holding: "text-warn", ready: "text-accent" };
// A status colour never carries meaning alone: each state also has a glyph and its name.
const glyph: Record<string, string> = { done: "✓", failed: "✕", lost: "?", holding: "⏸", ready: "●", downloading: "↓", importing: "⇄", reserving: "…" };
const FILTERS: { id: string; label: string; match: (s: string) => boolean }[] = [
  { id: "all", label: "All", match: () => true },
  { id: "active", label: "In progress", match: (s) => ["reserving", "downloading", "importing"].includes(s) },
  { id: "you", label: "Needs you", match: (s) => ["ready", "holding"].includes(s) },
  { id: "done", label: "Done", match: (s) => s === "done" },
  { id: "bad", label: "Failed / lost", match: (s) => ["failed", "lost"].includes(s) },
];

export default function Jobs() {
  const qc = useQueryClient();
  const [filter, setFilter] = useState("all");
  const jobs = useQuery({ queryKey: ["jobs", false], queryFn: () => api.jobs(false), refetchInterval: 10000 });
  const refresh = () => qc.invalidateQueries({ queryKey: ["jobs"] });
  const approve = useMutation({ mutationFn: api.approve, onSuccess: refresh });
  const del = useMutation({ mutationFn: api.deleteJob, onSuccess: refresh });
  const retry = useMutation({ mutationFn: api.retryJob, onSuccess: refresh });
  const reject = useMutation({ mutationFn: (id: number) => api.rejectJob(id, true), onSuccess: refresh });
  const err = approve.error || del.error || retry.error || reject.error;
  return (
    <div className="space-y-4">
      <h1 className="text-xl font-semibold">Replacements</h1>
      {err && <div className="text-bad">{(err as Error).message}</div>}
      <p className="text-sm text-muted">
        A pack is downloaded, probed and checked against what you have today before any old file is deleted.
        <b> Ready</b> means verified and waiting for you; <b>holding</b> means a check failed and nothing was touched.
      </p>
      {(approve.error || retry.error || reject.error) && (
        <div className="text-sm text-bad">{((approve.error || retry.error || reject.error) as Error).message}</div>
      )}
      <div role="group" aria-label="Filter by state" className="flex flex-wrap gap-1.5">
        {FILTERS.map((f) => {
          const n = (jobs.data ?? []).filter((j) => f.match(j.status)).length;
          return (
            <button key={f.id} aria-pressed={filter === f.id} onClick={() => setFilter(f.id)}
              className={`pill ${filter === f.id ? "border-accent bg-accent/15 text-white" : "text-muted hover:text-white"}`}>
              {f.label} <span className="num">{n}</span>
            </button>
          );
        })}
      </div>
      <div className="card overflow-x-auto">
        <table className="w-full">
          <thead><tr><th>Season</th><th>Release</th><th>Size</th><th>Status</th><th>Detail</th><th /></tr></thead>
          <tbody>
            {jobs.data?.filter((j) => FILTERS.find((f) => f.id === filter)!.match(j.status)).map((j) => (
              <tr key={j.id}>
                <td className="whitespace-nowrap">{j.title} S{String(j.season).padStart(2, "0")}</td>
                <td className="max-w-xs truncate text-muted" title={j.release}>{j.release}</td>
                <td className="whitespace-nowrap">{j.sizeGB.toFixed(1)} GB</td>
                <td>
                  <span className={`pill ${tone[j.status] ?? ""}`}><span aria-hidden>{glyph[j.status] ?? "•"}</span>{j.status}{j.status === "downloading" ? ` ${j.progress}%` : ""}</span>
                  {j.status === "downloading" && <div className="mt-1.5 h-1 w-24 overflow-hidden rounded-full bg-accent/15"><div className="h-full rounded-full bg-accent" style={{ width: `${j.progress}%` }} /></div>}
                </td>
                <td className="max-w-sm text-xs text-muted">{j.message}</td>
                <td className="space-x-2 whitespace-nowrap text-right">
                  {j.status === "ready" && <button className="btn btn-primary" onClick={() => approve.mutate(j.id)}>Approve</button>}
                  {["holding", "lost", "failed", "ready"].includes(j.status) && (
                    <button className="btn" disabled={retry.isPending} onClick={() => retry.mutate(j.id)} title="Re-check this job now instead of waiting for the next cycle">Retry now</button>
                  )}
                  {["holding", "ready", "lost"].includes(j.status) && (
                    <button className="btn btn-danger" disabled={reject.isPending}
                      onClick={() => confirm(`Reject this pack for ${j.title} S${String(j.season).padStart(2, "0")}? It is removed from the download client and blocklisted, a new search starts, and your current files are not touched.`) && reject.mutate(j.id)}
                      title="Bad or encrypted pack: remove it, blocklist the release and search again">Reject pack</button>
                  )}
                  <button className="btn btn-danger" onClick={() => del.mutate(j.id)} title="Stop tracking (does not cancel the download or restore files)">Remove</button>
                </td>
              </tr>
            ))}
            {jobs.data && jobs.data.filter((j) => FILTERS.find((f) => f.id === filter)!.match(j.status)).length === 0 && <tr><td colSpan={6} className="text-muted">{jobs.data.length === 0 ? "No replacements yet." : "Nothing in this state."}</td></tr>}
          </tbody>
        </table>
      </div>
    </div>
  );
}
