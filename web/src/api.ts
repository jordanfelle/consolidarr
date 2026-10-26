export type Instance = {
  id: number; name: string; url: string; apiKey?: string; pathFrom: string; pathTo: string;
  enabled: boolean; scanning?: boolean; lastScan?: number; health?: string;
};
export type Season = {
  instId: number; seriesId: number; season: number; title: string; seriesType: string;
  files: number; aired: number; pack: number; single: number; multi: number; unknown: number;
  groups: string[]; qualities: string[]; sources: string[]; gb: number; score: number;
  english: number; noEnglish: number; protected: number; complete: boolean;
};
export type Candidate = {
  guid: string; indexerId: number; indexer: string; title: string; size: number; seeders: number | null;
  quality: { quality: { name: string } }; blocking: string[]; picked: boolean; dual: boolean; kind: string;
};
export type ReviewPlan = {
  ok: boolean; reason?: string; folder: string; skipped?: string[]; token?: string;
  files: { path: string; episode: number; title: string; why: string; packQuality: string; replaces: string }[];
};
export type Job = {
  id: number; instId: number; seriesId: number; season: number; title: string; release: string;
  indexer: string; sizeGB: number; status: string; message: string; progress: number; oldFileIds: number[];
  created: number; updated: number; kind?: string;
};
export type Settings = {
  schedulerEnabled: boolean; dryRun: boolean; autoDelete: boolean; allowUnverified: boolean; maxInflight: number; maxPackGB: number;
  minFiles: number; includeAnime: boolean; cycleMinutes: number; scanHours: number; lastResortIndexers: string[];
  remapOrders: boolean; holdRejectHours: number; dualAudio: boolean; dubMaxFiles: number; dubSearchesPerCycle: number; relabelAnimeBytes: boolean; reviewCategory?: string;
};
export type ApiKey = { id: number; name: string; prefix: string; created: number; lastUsed: number };
export type Once = { active: boolean; limit: number; started: number; began: number; finished: number };
export type IndexerList = { indexers: { name: string; protocol: string; searchable: boolean; instances: number[] }[]; errors: string[] };
export type Attention = {
  instId: number; instance: string; seriesId: number; series: string; season: number; pack: string; indexer: string;
  sizeGB: number; items: number; state: string; haveFiles: number; aired: number;
  reasons: { kind: string; count: number; example: string; hint: string }[];
};
export type Snapshot = {
  ts: number; flaggedTV: number; flaggedAnime: number; seasons: number; files: number;
  packFiles: number; singleFiles: number; multiFiles: number; unknownFiles: number; gb: number;
};
export type Metrics = {
  range: string; since: number; current: Snapshot; snapshots: Snapshot[];
  perDay: { day: string; done: number; failed: number; gb: number }[];
  groupBuckets: { label: string; count: number }[];
  top: { instId: number; seriesId: number; title: string; season: number; files: number; groups: number; gb: number }[];
  totals: { done: number; failed: number; gb: number; inflight: number };
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

// The server refuses a lower-quality or Sonarr-rejected pack with "needs force: <why>". Ask the person once; only an
// explicit yes retries with force.
async function withForce<T>(go: (force: boolean) => Promise<T>): Promise<T> {
  try {
    return await go(false);
  } catch (e) {
    const m = (e as Error).message || "";
    if (m.startsWith("needs force:") && window.confirm(m.replace("needs force:", "Are you sure?") + "\n\nProceed anyway?")) return go(true);
    throw e;
  }
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
    withForce((force) => req("POST", `/seasons/${s.instId}/${s.seriesId}/${s.season}/grab`, { guid: c.guid, force })),
  jobs: (active = false) => req<Job[]>("GET", "/jobs" + (active ? "?active=1" : "")),
  approve: (id: number) => req("POST", `/jobs/${id}/approve`),
  deleteJob: (id: number) => withForce((force) => req("DELETE", `/jobs/${id}${force ? "?force=1" : ""}`)),
  settings: () => req<Settings>("GET", "/settings"),
  saveSettings: (s: Settings) => req<Settings>("PUT", "/settings", s),
  events: () => req<Event[]>("GET", "/events"),
  status: () => req<{ active: number; flaggedTV: number; flaggedAnime: number; settings: Settings; once: Once }>("GET", "/status"),
  runNow: () => req("POST", "/engine/run"),
  once: () => req<Once>("GET", "/engine/once"),
  startOnce: (limit: number) => req<Once>("POST", "/engine/once", { limit }),
  cancelOnce: () => req<Once>("POST", "/engine/once/cancel"),
  metrics: (range: string) => req<Metrics>("GET", `/metrics?range=${range}`),
  indexers: () => req<IndexerList>("GET", "/indexers"),
  attention: () => req<{ items: Attention[]; errors: string[] }>("GET", "/attention"),
  adopt: (a: Attention) =>
    withForce((force) => req<Job>("POST", `/attention/${a.instId}/${a.seriesId}/${a.season}/adopt${force ? "?force=1" : ""}`)),
  retryImport: (a: Attention) => req<{ commandId: number }>("POST", `/attention/${a.instId}/${a.seriesId}/${a.season}/retry`),
  removePack: (a: Attention, o: { blocklist: boolean; deleteFiles: boolean; research: boolean }) =>
    req<{ removed: number; searched: boolean }>("POST", `/attention/${a.instId}/${a.seriesId}/${a.season}/remove`, o),
  reviewPlan: (id: number) => req<ReviewPlan>("GET", `/jobs/${id}/review`),
  reviewApprove: (id: number, token: string) => req<{ ok: boolean }>("POST", `/jobs/${id}/review/approve?token=${encodeURIComponent(token)}`),
  retryJob: (id: number) => req<Job>("POST", `/jobs/${id}/retry`),
  rejectJob: (id: number, research = true) => req<Job>("POST", `/jobs/${id}/reject`, { research }),
  keys: () => req<ApiKey[]>("GET", "/keys"),
  createKey: (name: string) => req<{ id: number; name: string; prefix: string; key: string }>("POST", "/keys", { name }),
  deleteKey: (id: number) => req("DELETE", `/keys/${id}`),
};
