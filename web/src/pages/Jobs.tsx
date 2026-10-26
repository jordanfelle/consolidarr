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

// "BroadcastheNet (Prowlarr)" -> "BroadcastheNet": the indexer name Sonarr reports carries the proxy it came through.
const tracker = (indexer: string) => indexer.replace(/\s*\((?:Prowlarr|Jackett)\)\s*$/i, "") || "–";
// A pack that was rejected is gone from the download client and blocklisted, so "Retry now" can only end as "lost".
const wasRejected = (message: string) => /^(auto-)?rejected|pack rejected/i.test(message);

// ReviewDialog shows how a parked relabelled pack would be imported (changes nothing) and lets the human approve it.
function ReviewDialog({ id, onClose }: { id: number; onClose: () => void }) {
  const qc = useQueryClient();
  const plan = useQuery({ queryKey: ["review", id], queryFn: () => api.reviewPlan(id), retry: false });
  const approve = useMutation({
    mutationFn: () => api.reviewApprove(id, plan.data?.token ?? ""),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["jobs"] }); onClose(); },
  });
  const p = plan.data;
  return (
    <div className="fixed inset-0 z-20 flex items-center justify-center bg-black/60 p-4" onClick={onClose}>
      <div className="card max-h-[90vh] w-full max-w-3xl space-y-3 overflow-y-auto p-5" onClick={(e) => e.stopPropagation()}>
        <div className="text-lg font-semibold">Review this pack</div>
        {plan.isLoading && <div className="text-muted">Checking the pack…</div>}
        {plan.error && <div className="text-sm text-bad">{(plan.error as Error).message}</div>}
        {p && (
          <>
            <div className="break-all text-xs text-muted">{p.folder}</div>
            {p.ok ? (
              <div className="text-sm">
                Every episode of the season has exactly one file, each file passed the probe, and each carries an English track.
                Approving imports them onto these episodes by <b>copying</b> and keeps the torrent seeding in its seed category.
                <b> Sonarr then replaces each episode's current file with the new one, and with no recycle bin the replaced file is gone for good</b>: the table shows what each one replaces.
              </div>
            ) : (
              <div className="text-sm text-warn">Consolidarr cannot map this pack by itself: {p.reason}. Import it by hand in Sonarr (Manual Import), or reject it.</div>
            )}
            {p.files.length > 0 && (
              <table className="w-full text-sm">
                <thead><tr><th>Ep</th><th>Episode</th><th>New file</th><th>Quality / language</th><th>Identified by</th><th>Replaces</th></tr></thead>
                <tbody>{p.files.map((f) => (
                  <tr key={f.path}><td className="num">{f.episode}</td><td>{f.title}</td><td className="max-w-xs truncate text-muted" title={f.path}>{f.path.split("/").pop()}</td><td className="whitespace-nowrap">{f.packQuality}<div className="text-xs text-muted">{f.packLangs}</div></td><td className="text-muted">{f.why}</td><td className="max-w-xs truncate text-warn" title={f.replaces}>{f.replaces || "nothing (no file yet)"}</td></tr>
                ))}</tbody>
              </table>
            )}
            {p.skipped && p.skipped.length > 0 && <div className="text-xs text-muted">Not imported (not episodes): {p.skipped.join(", ")}</div>}
          </>
        )}
        {approve.error && <div className="text-sm text-bad">{(approve.error as Error).message}</div>}
        <div className="flex justify-end gap-2">
          <button className="btn" onClick={onClose}>Close</button>
          <button className="btn btn-primary" disabled={!p?.ok || approve.isPending} onClick={() => approve.mutate()}>Approve and import</button>
        </div>
      </div>
    </div>
  );
}

export default function Jobs() {
  const qc = useQueryClient();
  const [filter, setFilter] = useState("all");
  const [reviewing, setReviewing] = useState<number | null>(null);
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
          <thead><tr><th>Season</th><th>Release</th><th>Tracker</th><th>Size</th><th>Status</th><th>Detail</th><th /></tr></thead>
          <tbody>
            {jobs.data?.filter((j) => FILTERS.find((f) => f.id === filter)!.match(j.status)).map((j) => (
              <tr key={j.id}>
                <td className="whitespace-nowrap">{j.title} S{String(j.season).padStart(2, "0")}{j.kind === "dub" && <span className="pill ml-2 text-accent" title="Replacing this season with an English dub">dub</span>}{j.kind === "dub-review" && <span className="pill ml-2 text-accent" title="A dub pack the indexer numbers S01: downloaded, then left for you to check">dub, review</span>}</td>
                <td className="max-w-xs truncate text-muted" title={j.release}>{j.release}</td>
                <td className="whitespace-nowrap" title={j.indexer}>{tracker(j.indexer)}</td>
                <td className="whitespace-nowrap">{j.sizeGB.toFixed(1)} GB</td>
                <td>
                  <span className={`pill ${tone[j.status] ?? ""}`}><span aria-hidden>{glyph[j.status] ?? "•"}</span>{j.status}{j.status === "downloading" ? ` ${j.progress}%` : ""}</span>
                  {j.status === "downloading" && <div className="mt-1.5 h-1 w-24 overflow-hidden rounded-full bg-accent/15"><div className="h-full rounded-full bg-accent" style={{ width: `${j.progress}%` }} /></div>}
                </td>
                <td className="max-w-sm text-xs text-muted">{j.message}</td>
                <td className="space-x-2 whitespace-nowrap text-right">
                  {j.status === "ready" && <button className="btn btn-primary" onClick={() => approve.mutate(j.id)}>Approve</button>}
                  {j.kind === "dub-review" && j.status === "holding" && <button className="btn btn-primary" onClick={() => setReviewing(j.id)} title="See how this pack would be imported, then approve it">Review</button>}
                  {["holding", "lost", "failed", "ready"].includes(j.status) && !wasRejected(j.message) && (
                    <button className="btn" disabled={retry.isPending} onClick={() => retry.mutate(j.id)} title="Re-check this job now instead of waiting for the next cycle">Retry now</button>
                  )}
                  {["holding", "ready", "lost"].includes(j.status) && (
                    <button className="btn btn-danger" disabled={reject.isPending}
                      onClick={() => confirm(`Reject this pack for ${j.title} S${String(j.season).padStart(2, "0")}? The download is kept (moved to its seed category, never deleted), the release is blocklisted when that is safe, and your current files are not touched.`) && reject.mutate(j.id)}
                      title="Bad pack: take it out of the queue and set the download aside (never deleted)">Reject pack</button>
                  )}
                  <button className="btn btn-danger" onClick={() => del.mutate(j.id)} title="Stop tracking (does not cancel the download or restore files)">Remove</button>
                </td>
              </tr>
            ))}
            {jobs.data && jobs.data.filter((j) => FILTERS.find((f) => f.id === filter)!.match(j.status)).length === 0 && <tr><td colSpan={7} className="text-muted">{jobs.data.length === 0 ? "No replacements yet." : "Nothing in this state."}</td></tr>}
          </tbody>
        </table>
      </div>
      {reviewing !== null && <ReviewDialog id={reviewing} onClose={() => setReviewing(null)} />}
    </div>
  );
}
