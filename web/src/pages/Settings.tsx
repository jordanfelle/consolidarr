import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { Instance, Settings, api } from "../api";

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
  const save = useMutation({ mutationFn: (x: Settings) => api.saveSettings(x), onSuccess: () => { qc.invalidateQueries({ queryKey: ["settings"] }); qc.invalidateQueries({ queryKey: ["status"] }); } });
  const scan = useMutation({ mutationFn: api.scan, onSuccess: () => qc.invalidateQueries({ queryKey: ["instances"] }) });
  const del = useMutation({ mutationFn: api.deleteInstance, onSuccess: () => qc.invalidateQueries({ queryKey: ["instances"] }) });
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
            <Toggle label="Include anime" v={s.includeAnime} on={(b) => setS({ ...s, includeAnime: b })} />
            <div className="grid gap-3 md:grid-cols-3">
              <div><label className="label">Max packs in flight</label><input className="input" type="number" value={s.maxInflight} onChange={num("maxInflight")} /></div>
              <div><label className="label">Max pack size (GB)</label><input className="input" type="number" value={s.maxPackGB} onChange={num("maxPackGB")} /></div>
              <div><label className="label">Min files in a season</label><input className="input" type="number" value={s.minFiles} onChange={num("minFiles")} /></div>
              <div><label className="label">Cycle (minutes)</label><input className="input" type="number" value={s.cycleMinutes} onChange={num("cycleMinutes")} /></div>
              <div><label className="label">Rescan every (hours)</label><input className="input" type="number" value={s.scanHours} onChange={num("scanHours")} /></div>
              <div><label className="label">Last-resort indexers</label>
                <input className="input" value={s.lastResortIndexers.join(", ")} onChange={(e) => setS({ ...s, lastResortIndexers: e.target.value.split(",").map((x) => x.trim()).filter(Boolean) })} />
                <span className="text-xs text-muted">Used only when no other indexer has a valid pack.</span></div>
            </div>
            <div className="flex gap-2"><button className="btn btn-primary" onClick={() => save.mutate(s)}>Save settings</button>
              <button className="btn" onClick={() => api.runNow()}>Run a cycle now</button></div>
          </div>
        </section>
      )}
    </div>
  );
}
