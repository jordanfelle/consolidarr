export type Instance = {
  id: number; name: string; url: string; apiKey?: string; pathFrom: string; pathTo: string;
  enabled: boolean; scanning?: boolean; lastScan?: number; health?: string;
};
export type Season = {
  instId: number; seriesId: number; season: number; title: string; seriesType: string;
  files: number; aired: number; pack: number; single: number; multi: number; unknown: number;
  groups: string[]; qualities: string[]; sources: string[]; gb: number; score: number;
};
export type Candidate = {
  guid: string; indexerId: number; indexer: string; title: string; size: number; seeders: number | null;
  quality: { quality: { name: string } }; blocking: string[]; picked: boolean;
};
export type Job = {
  id: number; instId: number; seriesId: number; season: number; title: string; release: string;
  indexer: string; sizeGB: number; status: string; message: string; progress: number; oldFileIds: number[];
  created: number; updated: number;
};
export type Settings = {
  schedulerEnabled: boolean; dryRun: boolean; autoDelete: boolean; maxInflight: number; maxPackGB: number;
  minFiles: number; includeAnime: boolean; cycleMinutes: number; scanHours: number; lastResortIndexers: string[];
};
export type Event = { id: number; ts: number; level: string; msg: string };

async function req<T>(method: string, path: string, body?: unknown): Promise<T> {
  const r = await fetch("/api" + path, {
    method,
    headers: body ? { "Content-Type": "application/json" } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  });
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error((data as { error?: string }).error || r.statusText);
  return data as T;
}

export const api = {
  me: () => req<{ setupRequired: boolean; authenticated: boolean; version: string }>("GET", "/auth/me"),
  setup: (username: string, password: string) => req("POST", "/auth/setup", { username, password }),
  login: (username: string, password: string) => req("POST", "/auth/login", { username, password }),
  logout: () => req("POST", "/auth/logout"),
  instances: () => req<Instance[]>("GET", "/instances"),
  saveInstance: (i: Partial<Instance>) => i.id ? req("PUT", `/instances/${i.id}`, i) : req("POST", "/instances", i),
  deleteInstance: (id: number) => req("DELETE", `/instances/${id}`),
  testInstance: (i: Partial<Instance>) => req<{ version: string }>("POST", "/instances/test", i),
  scan: (id: number) => req("POST", `/instances/${id}/scan`),
  seasons: (q: URLSearchParams) => req<{ total: number; seasons: Season[] }>("GET", "/seasons?" + q.toString()),
  candidates: (s: Season) => req<Candidate[]>("GET", `/seasons/${s.instId}/${s.seriesId}/${s.season}/candidates`),
  grab: (s: Season, c: Candidate) =>
    req("POST", `/seasons/${s.instId}/${s.seriesId}/${s.season}/grab`, {
      guid: c.guid, indexerId: c.indexerId, title: c.title, sizeGB: c.size / 1e9, indexer: c.indexer,
    }),
  jobs: (active = false) => req<Job[]>("GET", "/jobs" + (active ? "?active=1" : "")),
  approve: (id: number) => req("POST", `/jobs/${id}/approve`),
  deleteJob: (id: number) => req("DELETE", `/jobs/${id}`),
  settings: () => req<Settings>("GET", "/settings"),
  saveSettings: (s: Settings) => req<Settings>("PUT", "/settings", s),
  events: () => req<Event[]>("GET", "/events"),
  status: () => req<{ active: number; flaggedTV: number; flaggedAnime: number; settings: Settings }>("GET", "/status"),
  runNow: () => req("POST", "/engine/run"),
};
