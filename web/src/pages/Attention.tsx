import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Attention as Item, api } from "../api";

const label: Record<string, string> = {
  not_upgrade: "Not an upgrade",
  already_imported: "Already imported",
  destination_exists: "File already exists",
  id_match: "Matched by id",
  not_in_release: "Episode not in release",
  sample: "Sample",
  unknown_series: "Unknown series",
  other: "Other",
};

function RemoveDialog({ a, onClose }: { a: Item; onClose: () => void }) {
  const qc = useQueryClient();
  const [blocklist, setBlocklist] = useState(false);
  const [research, setResearch] = useState(false);
  const rm = useMutation({
    mutationFn: () => api.removePack(a, { blocklist, deleteFiles: false, research }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["attention"] }); onClose(); },
  });
  return (
    <div className="fixed inset-0 z-20 flex items-center justify-center bg-black/60 p-4" onClick={onClose}>
      <div className="card w-full max-w-md space-y-3 p-5" onClick={(e) => e.stopPropagation()}>
        <div className="text-lg font-semibold">Remove this pack?</div>
        <div className="text-sm text-muted">
          {a.series} S{String(a.season).padStart(2, "0")}: <span className="break-all">{a.pack}</span>. Your library files are never touched.
        </div>
        <div className="text-xs text-muted">The download itself is never deleted: it is moved to its seed category and kept seeding.</div>
        <label className="flex items-start gap-2 text-sm"><input type="checkbox" className="mt-1" checked={blocklist} onChange={(e) => setBlocklist(e.target.checked)} />
          <span>Blocklist this release<span className="block text-xs text-muted">Sonarr will never grab it again. Use this for bad or encrypted releases.</span></span></label>
        <label className="flex items-start gap-2 text-sm"><input type="checkbox" className="mt-1" checked={research} onChange={(e) => setResearch(e.target.checked)} />
          <span>Search for this season again</span></label>
        {rm.error && <div className="text-sm text-bad">{(rm.error as Error).message}</div>}
        <div className="flex gap-2">
          <button className="btn btn-danger" disabled={rm.isPending} onClick={() => rm.mutate()}>{rm.isPending ? "Removing…" : "Remove pack"}</button>
          <button className="btn" onClick={onClose}>Cancel</button>
        </div>
      </div>
    </div>
  );
}

export default function AttentionPage() {
  const qc = useQueryClient();
  const list = useQuery({ queryKey: ["attention"], queryFn: api.attention, refetchInterval: 30000 });
  const [removing, setRemoving] = useState<Item | null>(null);
  const [note, setNote] = useState<{ ok: boolean; msg: string } | null>(null);
  const done = (msg: string) => { setNote({ ok: true, msg }); qc.invalidateQueries({ queryKey: ["attention"] }); qc.invalidateQueries({ queryKey: ["jobs"] }); };
  const fail = (e: unknown) => setNote({ ok: false, msg: (e as Error).message });
  const adopt = useMutation({
    mutationFn: api.adopt,
    onSuccess: (j) => done(`Adopted. ${j.title} S${String(j.season).padStart(2, "0")} is now "${j.status}": ${j.message} See Replacements.`),
    onError: fail,
  });
  const retry = useMutation({ mutationFn: api.retryImport, onSuccess: () => done("Import retry requested. Sonarr will work through it."), onError: fail });
  return (
    <div className="space-y-4">
      <h1 className="text-xl font-semibold">Needs attention</h1>
      <p className="text-sm text-muted">
        Downloaded packs that Sonarr will not import by itself, and that no replacement is handling. Usually the pack is no better than what
        you have, or it was only partly imported. <b>Adopt</b> runs the normal checks (probe, coverage, safe-to-delete) and replaces your files if they pass;
        <b> Retry import</b> asks Sonarr to try again; <b>Remove</b> clears the pack.
      </p>
      {note && <div className={`text-sm ${note.ok ? "text-good" : "text-bad"}`}>{note.msg}</div>}
      {list.data?.errors.length ? <div className="text-sm text-warn">{list.data.errors.join("; ")}</div> : null}
      {list.isLoading && <div className="text-muted">Checking Sonarr…</div>}
      <div className="space-y-3">
        {list.data?.items.map((a) => (
          <div key={`${a.instId}-${a.seriesId}-${a.season}`} className="card p-4">
            <div className="flex flex-wrap items-start justify-between gap-3">
              <div className="min-w-0">
                <div className="font-medium">{a.series} · Season {a.season} <span className="text-xs text-muted">({a.instance})</span></div>
                <div className="mt-1 break-all text-xs text-muted">{a.pack}</div>
                <div className="mt-1 text-xs text-muted">{a.items} files in the pack · {a.sizeGB.toFixed(1)} GB · you have {a.haveFiles} of {a.aired} episodes · {a.indexer}</div>
              </div>
              <div className="flex shrink-0 flex-wrap gap-2">
                <button className="btn btn-primary" disabled={adopt.isPending} onClick={() => { setNote(null); adopt.mutate(a); }}>Adopt</button>
                <button className="btn" disabled={retry.isPending} onClick={() => { setNote(null); retry.mutate(a); }}>Retry import</button>
                <button className="btn btn-danger" onClick={() => { setNote(null); setRemoving(a); }}>Remove…</button>
              </div>
            </div>
            <div className="mt-3 space-y-1">
              {a.reasons.length === 0 && <div className="text-sm text-muted">Sonarr reports no specific reason.</div>}
              {a.reasons.map((r) => (
                <div key={r.kind} className="text-sm">
                  <span className="pill mr-2 text-warn">{label[r.kind] ?? r.kind}{r.count > 1 ? ` ×${r.count}` : ""}</span>
                  <span className="text-muted">{r.hint}</span>
                </div>
              ))}
            </div>
          </div>
        ))}
        {list.data?.items.length === 0 && <div className="card p-4 text-sm text-muted">Nothing needs attention.</div>}
      </div>
      {removing && <RemoveDialog a={removing} onClose={() => setRemoving(null)} />}
    </div>
  );
}
