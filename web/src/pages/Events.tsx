import { useQuery } from "@tanstack/react-query";
import { api } from "../api";

const color: Record<string, string> = { error: "text-bad", warn: "text-warn", info: "text-slate-300" };

export default function Events() {
  const ev = useQuery({ queryKey: ["events"], queryFn: api.events, refetchInterval: 10000 });
  return (
    <div className="space-y-4">
      <h1 className="text-xl font-semibold">Activity</h1>
      <div className="card divide-y divide-line font-mono text-xs">
        {ev.data?.map((e) => (
          <div key={e.id} className="flex gap-3 px-4 py-2">
            <span className="shrink-0 text-muted">{new Date(e.ts * 1000).toLocaleString()}</span>
            <span className={color[e.level] ?? ""}>{e.msg}</span>
          </div>
        ))}
        {ev.data?.length === 0 && <div className="px-4 py-3 text-muted">No activity yet.</div>}
      </div>
    </div>
  );
}
