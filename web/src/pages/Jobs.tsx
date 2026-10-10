import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";

const tone: Record<string, string> = { done: "text-good", failed: "text-bad", lost: "text-warn", holding: "text-warn", ready: "text-accent" };

export default function Jobs() {
  const qc = useQueryClient();
  const jobs = useQuery({ queryKey: ["jobs", false], queryFn: () => api.jobs(false), refetchInterval: 10000 });
  const refresh = () => qc.invalidateQueries({ queryKey: ["jobs"] });
  const approve = useMutation({ mutationFn: api.approve, onSuccess: refresh });
  const del = useMutation({ mutationFn: api.deleteJob, onSuccess: refresh });
  return (
    <div className="space-y-4">
      <h1 className="text-xl font-semibold">Replacements</h1>
      <p className="text-sm text-muted">
        A pack is downloaded, probed and checked against what you have today before any old file is deleted.
        <b> Ready</b> means verified and waiting for you; <b>holding</b> means a check failed and nothing was touched.
      </p>
      {approve.error && <div className="text-sm text-bad">{(approve.error as Error).message}</div>}
      <div className="card overflow-x-auto">
        <table className="w-full">
          <thead><tr><th>Season</th><th>Release</th><th>Size</th><th>Status</th><th>Detail</th><th /></tr></thead>
          <tbody>
            {jobs.data?.map((j) => (
              <tr key={j.id}>
                <td className="whitespace-nowrap">{j.title} S{String(j.season).padStart(2, "0")}</td>
                <td className="max-w-xs truncate text-muted" title={j.release}>{j.release}</td>
                <td className="whitespace-nowrap">{j.sizeGB.toFixed(1)} GB</td>
                <td><span className={`pill ${tone[j.status] ?? ""}`}>{j.status}{j.status === "downloading" ? ` ${j.progress}%` : ""}</span></td>
                <td className="max-w-sm text-xs text-muted">{j.message}</td>
                <td className="space-x-2 whitespace-nowrap text-right">
                  {j.status === "ready" && <button className="btn btn-primary" onClick={() => approve.mutate(j.id)}>Approve</button>}
                  <button className="btn btn-danger" onClick={() => del.mutate(j.id)} title="Stop tracking (does not cancel the download or restore files)">Remove</button>
                </td>
              </tr>
            ))}
            {jobs.data?.length === 0 && <tr><td colSpan={6} className="text-muted">No replacements yet.</td></tr>}
          </tbody>
        </table>
      </div>
    </div>
  );
}
