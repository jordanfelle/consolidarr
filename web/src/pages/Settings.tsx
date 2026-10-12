import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { Instance, Settings, api } from "../api";

// Last-resort indexers: pick from the indexers your Sonarrs actually have. A saved value matches an indexer when
// it is a case-insensitive substring of its name (so an older "torrentleech" still shows TorrentLeech checked).
function IndexerPicker({ value, onChange }: { value: string[]; onChange: (v: string[]) => void }) {
  const list = useQuery({ queryKey: ["indexers"], queryFn: api.indexers });
  const names = list.data?.indexers.map((i) => i.name) ?? [];
  const matches = (v: string, n: string) => n.toLowerCase().includes(v.toLowerCase());
  const checked = (n: string) => value.some((v) => matches(v, n));
  const orphans = value.filter((v) => !names.some((n) => matches(v, n)));
  const toggle = (n: string) => onChange(checked(n) ? value.filter((v) => !matches(v, n)) : [...value, n]);
  return (
    <div>
      <label className="label">Last-resort indexers</label>
      <div className="card max-h-44 space-y-1 overflow-y-auto p-2">
        {list.isLoading && <div className="text-sm text-muted">Loading indexers from Sonarr…</div>}
        {list.error && <div className="text-sm text-bad">{(list.error as Error).message}</div>}
        {names.map((n) => (
          <label key={n} className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={checked(n)} onChange={() => toggle(n)} /> {n}
          </label>
        ))}
        {orphans.map((v) => (
          <label key={v} className="flex items-center gap-2 text-sm text-warn" title="Not an indexer in any enabled Sonarr; uncheck to remove">
            <input type="checkbox" checked onChange={() => onChange(value.filter((x) => x !== v))} /> {v} (not found in Sonarr)
          </label>
        ))}
        {!list.isLoading && names.length === 0 && orphans.length === 0 && <div className="text-sm text-muted">No indexers found. Add and enable a Sonarr instance.</div>}
      </div>
      {list.data?.errors.length ? <div className="mt-1 text-xs text-warn">{list.data.errors.join("; ")}</div> : null}
      <span className="text-xs text-muted">Used only when no other indexer has a valid pack.</span>
    </div>
  );
}

function OneOff({ dryRun, hasInstance }: { dryRun: boolean; hasInstance: boolean }) {
  const qc = useQueryClient();
  const [n, setN] = useState(5);
  const once = useQuery({ queryKey: ["once"], queryFn: api.once, refetchInterval: 5000 });
  const refresh = () => { qc.invalidateQueries({ queryKey: ["once"] }); qc.invalidateQueries({ queryKey: ["jobs"] }); qc.invalidateQueries({ queryKey: ["events"] }); };
  const start = useMutation({ mutationFn: () => api.startOnce(n), onSuccess: refresh });
  const stop = useMutation({ mutationFn: api.cancelOnce, onSuccess: refresh });
  const o = once.data;
  return (
    <div className="space-y-3 border-t border-line pt-4">
      <div>
        <div className="text-sm font-medium">One-off run</div>
        <div className="text-xs text-muted">
          Replace up to N seasons now, without turning on the scheduler. It searches, grabs, verifies and replaces them, then stops on its own.
          Uses the dry run and auto-delete settings above (saved ones). {dryRun ? "Dry run is ON, so this only logs what it would grab." : "Dry run is OFF: this will grab and replace for real."}
        </div>
      </div>
      {o?.active ? (
        <div className="flex flex-wrap items-center gap-3 text-sm">
          <span className="pill text-accent">running</span>
          <span>{o.started} of {o.limit} seasons started</span>
          <button className="btn" disabled={stop.isPending || o.started >= o.limit} onClick={() => stop.mutate()}>Stop starting new ones</button>
          <span className="text-xs text-muted">Watch progress on Replacements and Activity.</span>
        </div>
      ) : (
        <div className="flex flex-wrap items-center gap-2">
          <input className="input w-24" type="number" min={1} max={500} value={n} onChange={(e) => setN(Number(e.target.value))} />
          <span className="text-sm text-muted">seasons</span>
          <button className="btn btn-primary" disabled={!hasInstance || start.isPending || n < 1} onClick={() => start.mutate()}>Run once</button>
          {!hasInstance && <span className="text-sm text-muted">Add a Sonarr instance first.</span>}
          {o && o.finished > 0 && <span className="text-xs text-muted">Last run finished {new Date(o.finished * 1000).toLocaleString()}: {o.started} seasons started.</span>}
        </div>
      )}
      {start.error && <div className="text-sm text-bad">{(start.error as Error).message}</div>}
    </div>
  );
}

function Keys() {
  const qc = useQueryClient();
  const keys = useQuery({ queryKey: ["keys"], queryFn: api.keys });
  const [name, setName] = useState("");
  const [fresh, setFresh] = useState("");
  const create = useMutation({ mutationFn: () => api.createKey(name), onSuccess: (r) => { setFresh(r.key); setName(""); qc.invalidateQueries({ queryKey: ["keys"] }); } });
  const del = useMutation({ mutationFn: api.deleteKey, onSuccess: () => qc.invalidateQueries({ queryKey: ["keys"] }) });
  return (
    <section className="space-y-3">
      <div className="flex items-center justify-between"><h2 className="text-lg font-semibold">API keys</h2>
        <a className="text-sm text-accent hover:underline" href="/api/docs" target="_blank" rel="noreferrer">API reference</a></div>
      <p className="text-sm text-muted">Send a key as <code>X-API-Key</code> or <code>Authorization: Bearer</code>. Keys are shown once when created.</p>
      {fresh && (
        <div className="card border-good/40 p-3 text-sm">
          <div className="mb-1 text-good">Copy this key now, it will not be shown again:</div>
          <code className="break-all">{fresh}</code>
          <div className="mt-2"><button className="btn" onClick={() => setFresh("")}>Done</button></div>
        </div>
      )}
      <div className="flex gap-2">
        <input className="input max-w-xs" placeholder="Key name (e.g. home-assistant)" value={name} onChange={(e) => setName(e.target.value)} />
        <button className="btn btn-primary" disabled={!name.trim() || create.isPending} onClick={() => create.mutate()}>Create key</button>
      </div>
      {create.error && <div className="text-sm text-bad">{(create.error as Error).message}</div>}
      <div className="card">
        <table className="w-full"><thead><tr><th>Name</th><th>Key</th><th>Created</th><th>Last used</th><th /></tr></thead><tbody>
          {keys.data?.map((k) => (
            <tr key={k.id}><td>{k.name}</td><td className="font-mono text-muted">{k.prefix}…</td>
              <td className="text-muted">{new Date(k.created * 1000).toLocaleDateString()}</td>
              <td className="text-muted">{k.lastUsed ? new Date(k.lastUsed * 1000).toLocaleString() : "never"}</td>
              <td className="text-right"><button className="btn btn-danger" onClick={() => confirm(`Revoke ${k.name}?`) && del.mutate(k.id)}>Revoke</button></td></tr>
          ))}
          {keys.data?.length === 0 && <tr><td colSpan={5} className="text-muted">No API keys.</td></tr>}
        </tbody></table>
      </div>
    </section>
  );
}

const blank: Partial<Instance> = { name: "", url: "", apiKey: "", pathFrom: "", pathTo: "", enabled: true };

function InstanceForm({ initial, onDone }: { initial: Partial<Instance>; onDone: () => void }) {
  const [f, setF] = useState<Partial<Instance>>(initial);
  const [msg, setMsg] = useState("");
  const set = (k: keyof Instance, v: string | boolean) => setF({ ...f, [k]: v });
  const test = async () => { try { setMsg("Connected: Sonarr " + (await api.testInstance(f)).version); } catch (e) { setMsg((e as Error).message); } };
  const save = useMutation({ mutationFn: () => api.saveInstance(f), onSuccess: onDone });
  return (
    <div className="card space-y-3 p-4">
      <div className="grid gap-3 md:grid-cols-2">
        <div><label className="label">Name</label><input className="input" value={f.name} onChange={(e) => set("name", e.target.value)} placeholder="Main Sonarr" /></div>
        <div><label className="label">URL</label><input className="input" value={f.url} onChange={(e) => set("url", e.target.value)} placeholder="http://sonarr:8989/sonarr" /></div>
        <div className="md:col-span-2"><label className="label">API key {initial.id ? "(leave blank to keep)" : ""}</label>
          <input className="input" type="password" value={f.apiKey ?? ""} onChange={(e) => set("apiKey", e.target.value)} /></div>
        <div><label className="label">Download path in Sonarr (optional)</label><input className="input" value={f.pathFrom} onChange={(e) => set("pathFrom", e.target.value)} placeholder="/downloads" /></div>
        <div><label className="label">Same path in consolidarr (optional)</label><input className="input" value={f.pathTo} onChange={(e) => set("pathTo", e.target.value)} placeholder="/downloads" /></div>
      </div>
      <p className="text-xs text-muted">Mount your download directory into consolidarr and fill both paths so ffprobe can verify packs (DRM, undecodable video) before anything is deleted. Without it, only episode coverage is checked.</p>
      <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={!!f.enabled} onChange={(e) => set("enabled", e.target.checked)} /> Enabled</label>
      {(msg || save.error) && <div className="text-sm text-muted">{msg || (save.error as Error).message}</div>}
      <div className="flex gap-2"><button className="btn" onClick={test}>Test</button><button className="btn btn-primary" onClick={() => save.mutate()}>Save</button><button className="btn" onClick={onDone}>Cancel</button></div>
    </div>
  );
}

function Toggle({ label, hint, v, on }: { label: string; hint?: string; v: boolean; on: (b: boolean) => void }) {
  return (
    <label className="flex items-start gap-3 text-sm">
      <input type="checkbox" className="mt-1" checked={v} onChange={(e) => on(e.target.checked)} />
      <span>{label}{hint && <span className="block text-xs text-muted">{hint}</span>}</span>
    </label>
  );
}

export default function SettingsPage() {
  const qc = useQueryClient();
  const insts = useQuery({ queryKey: ["instances"], queryFn: api.instances, refetchInterval: 5000 });
  const remote = useQuery({ queryKey: ["settings"], queryFn: api.settings });
  const [s, setS] = useState<Settings | null>(null);
  const [edit, setEdit] = useState<Partial<Instance> | null>(null);
  useEffect(() => { if (remote.data) setS(remote.data); }, [remote.data]);
  const [note, setNote] = useState<{ ok: boolean; msg: string } | null>(null);
  const save = useMutation({
    mutationFn: (x: Settings) => api.saveSettings(x),
    onSuccess: () => { setNote({ ok: true, msg: "Settings saved." }); qc.invalidateQueries({ queryKey: ["settings"] }); qc.invalidateQueries({ queryKey: ["status"] }); },
    onError: (e) => setNote({ ok: false, msg: (e as Error).message }),
  });
  const run = useMutation({
    mutationFn: api.runNow,
    onSuccess: () => setNote({ ok: true, msg: "Cycle started. Progress shows on the Activity and Replacements pages." }),
    onError: (e) => setNote({ ok: false, msg: (e as Error).message }),
  });
  const dirty = !!s && !!remote.data && JSON.stringify(s) !== JSON.stringify(remote.data);
  const scan = useMutation({ mutationFn: api.scan, onSuccess: () => qc.invalidateQueries({ queryKey: ["instances"] }) });
  const del = useMutation({
    mutationFn: api.deleteInstance,
    onSuccess: () => qc.invalidateQueries({ queryKey: ["instances"] }),
    onError: (e) => setNote({ ok: false, msg: (e as Error).message }),
  });
  const num = (k: keyof Settings) => (e: React.ChangeEvent<HTMLInputElement>) => s && setS({ ...s, [k]: Number(e.target.value) });
  return (
    <div className="max-w-4xl space-y-8">
      <section className="space-y-3">
        <div className="flex items-center justify-between"><h1 className="text-xl font-semibold">Sonarr instances</h1>
          <button className="btn btn-primary" onClick={() => setEdit(blank)}>Add instance</button></div>
        {edit && <InstanceForm initial={edit} onDone={() => { setEdit(null); qc.invalidateQueries({ queryKey: ["instances"] }); }} />}
        <div className="card">
          <table className="w-full"><thead><tr><th>Name</th><th>URL</th><th /></tr></thead><tbody>
            {insts.data?.map((i) => (
              <tr key={i.id}><td>{i.name}</td><td className="text-muted">{i.url}</td>
                <td className="space-x-2 text-right">
                  <button className="btn" disabled={i.scanning} onClick={() => scan.mutate(i.id)}>{i.scanning ? "Scanning…" : "Scan now"}</button>
                  <button className="btn" onClick={() => setEdit(i)}>Edit</button>
                  <button className="btn btn-danger" onClick={() => confirm(`Remove ${i.name}?`) && del.mutate(i.id)}>Remove</button></td></tr>
            ))}
            {insts.data?.length === 0 && <tr><td colSpan={3} className="text-muted">Add one or more Sonarr instances.</td></tr>}
          </tbody></table>
        </div>
      </section>
      <Keys />
      {s && (
        <section className="space-y-4">
          <h2 className="text-lg font-semibold">Scheduler</h2>
          <div className="card space-y-4 p-4">
            <Toggle label="Run the scheduler" hint="Scans on a timer, finds season packs and replaces seasons across every enabled instance." v={s.schedulerEnabled} on={(b) => setS({ ...s, schedulerEnabled: b })} />
            <Toggle label="Dry run" hint="Log what would be grabbed instead of grabbing it. Start here." v={s.dryRun} on={(b) => setS({ ...s, dryRun: b })} />
            <Toggle label="Delete old files automatically once verified" hint="Off = verified jobs wait on the Replacements page for you to approve." v={s.autoDelete} on={(b) => setS({ ...s, autoDelete: b })} />
            <Toggle label="Allow deleting when files can't be verified" hint="Off (recommended): if ffprobe can't read the downloaded files, the job is held and nothing is deleted. Turn on only if you can't mount the download directory." v={s.allowUnverified} on={(b) => setS({ ...s, allowUnverified: b })} />
            <Toggle label="Import packs numbered in a different order" hint="Some packs number episodes differently from Sonarr (aired vs DVD order, a wrong scene mapping). When every file's title proves which episode it is, import it onto those episodes explicitly. Nothing is deleted first; your file for each episode is swapped as it imports." v={s.remapOrders} on={(b) => setS({ ...s, remapOrders: b })} />
            <Toggle label="Include anime" v={s.includeAnime} on={(b) => setS({ ...s, includeAnime: b })} />
            <div className="grid gap-3 md:grid-cols-3">
              <div><label className="label">Max packs in flight</label><input className="input" type="number" value={s.maxInflight} onChange={num("maxInflight")} /></div>
              <div><label className="label">Max pack size (GB)</label>
                <input className="input" type="number" min={1} disabled={s.maxPackGB === 0} value={s.maxPackGB === 0 ? "" : s.maxPackGB} placeholder="No limit" onChange={num("maxPackGB")} />
                <label className="mt-1 flex items-center gap-2 text-xs text-muted">
                  <input type="checkbox" checked={s.maxPackGB === 0} onChange={(e) => setS({ ...s, maxPackGB: e.target.checked ? 0 : 60 })} /> No size limit
                </label></div>
              <div><label className="label">Reject a stuck pack after (hours)</label><input className="input" type="number" min={0} value={s.holdRejectHours} onChange={num("holdRejectHours")} />
                <div className="mt-1 text-xs text-muted">A pack held because it lacks your episodes, has nothing importable, fails probing or uses another order is removed and blocklisted after this long, freeing its slot. 0 = never.</div></div>
              <div><label className="label">Min files in a season</label><input className="input" type="number" value={s.minFiles} onChange={num("minFiles")} /></div>
              <div><label className="label">Cycle (minutes)</label><input className="input" type="number" value={s.cycleMinutes} onChange={num("cycleMinutes")} /></div>
              <div><label className="label">Rescan every (hours)</label><input className="input" type="number" value={s.scanHours} onChange={num("scanHours")} /></div>
              <IndexerPicker value={s.lastResortIndexers} onChange={(v) => setS({ ...s, lastResortIndexers: v })} />
            </div>
            <div className="flex flex-wrap items-center gap-2">
              <button className="btn btn-primary" disabled={!dirty || save.isPending} onClick={() => { setNote(null); save.mutate(s); }}>
                {save.isPending ? "Saving…" : "Save settings"}
              </button>
              <button className="btn" disabled={!remote.data?.schedulerEnabled || dirty || run.isPending}
                title={!remote.data?.schedulerEnabled ? "Enable the scheduler and save first" : dirty ? "Save your changes first" : ""}
                onClick={() => { setNote(null); run.mutate(); }}>
                Run a cycle now
              </button>
              {dirty && <span className="text-sm text-warn">Unsaved changes</span>}
              {!remote.data?.schedulerEnabled && !dirty && <span className="text-sm text-muted">Scheduler is off. Use a one-off run below, or enable it above and save.</span>}
              {note && <span className={`text-sm ${note.ok ? "text-good" : "text-bad"}`}>{note.msg}</span>}
            </div>
            <OneOff dryRun={remote.data?.dryRun ?? true} hasInstance={(insts.data?.length ?? 0) > 0} />
          </div>
        </section>
      )}
    </div>
  );
}
