import { ReactNode, useEffect, useMemo, useRef, useState } from "react";

// ---- helpers ---------------------------------------------------------------------------------------------------

export const compact = (n: number): string => {
  const a = Math.abs(n);
  if (a >= 1e6) return `${(n / 1e6).toFixed(a >= 1e7 ? 0 : 1)}M`;
  if (a >= 1e3) return `${(n / 1e3).toFixed(a >= 1e4 ? 0 : 1)}K`;
  return `${Math.round(n)}`;
};
export const commas = (n: number) => Math.round(n).toLocaleString();

/** Round the top of the scale up to a clean number and return evenly spaced ticks from 0. */
export function niceTicks(max: number, count = 4): number[] {
  if (!isFinite(max) || max <= 0) return [0, 1];
  const raw = max / count;
  const mag = Math.pow(10, Math.floor(Math.log10(raw)));
  const step = [1, 2, 2.5, 5, 10].map((m) => m * mag).find((s) => s >= raw) ?? raw;
  const out: number[] = [];
  for (let v = 0; v <= max + step * 0.999; v += step) out.push(Math.round(v * 1e6) / 1e6);
  return out;
}

function useWidth(): [React.RefObject<HTMLDivElement | null>, number] {
  const ref = useRef<HTMLDivElement | null>(null);
  const [w, setW] = useState(600);
  useEffect(() => {
    if (!ref.current) return;
    const ro = new ResizeObserver(([e]) => setW(Math.max(240, Math.floor(e.contentRect.width))));
    ro.observe(ref.current);
    return () => ro.disconnect();
  }, []);
  return [ref, w];
}

const fmtTime = (t: number, span: number) => {
  const d = new Date(t * 1000);
  return span <= 2 * 86400
    ? d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" })
    : d.toLocaleDateString([], { month: "short", day: "numeric" });
};

// ---- container with the table twin ----------------------------------------------------------------------------

export function ChartCard({ title, subtitle, legend, table, children, className = "" }: {
  title: string; subtitle?: string; legend?: ReactNode;
  table?: { head: string[]; rows: (string | number)[][] };
  children: ReactNode; className?: string;
}) {
  const [asTable, setAsTable] = useState(false);
  return (
    <figure className={`card flex flex-col p-4 ${className}`}>
      <figcaption className="mb-3 flex items-start justify-between gap-3">
        <div>
          <div className="text-sm font-medium">{title}</div>
          {subtitle && <div className="mt-0.5 text-xs text-muted">{subtitle}</div>}
        </div>
        {table && (
          <button className="btn btn-sm shrink-0" onClick={() => setAsTable(!asTable)} aria-pressed={asTable}>
            {asTable ? "Chart" : "Table"}
          </button>
        )}
      </figcaption>
      {asTable && table ? (
        <div className="max-h-64 overflow-auto">
          <table className="w-full">
            <thead><tr>{table.head.map((h) => <th key={h}>{h}</th>)}</tr></thead>
            <tbody>{table.rows.map((r, i) => <tr key={i}>{r.map((c, j) => <td key={j} className="num">{c}</td>)}</tr>)}</tbody>
          </table>
        </div>
      ) : (
        <>
          {children}
          {legend && <div className="mt-3">{legend}</div>}
        </>
      )}
    </figure>
  );
}

export function Legend({ items }: { items: { name: string; color: string; kind?: "line" | "rect" }[] }) {
  return (
    <ul className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-slate-300">
      {items.map((i) => (
        <li key={i.name} className="flex items-center gap-1.5">
          <span aria-hidden className={i.kind === "rect" ? "h-2.5 w-2.5 rounded-[2px]" : "h-0.5 w-4 rounded"} style={{ background: i.color }} />
          {i.name}
        </li>
      ))}
    </ul>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="flex min-h-32 items-center justify-center rounded-md border border-dashed border-line px-4 text-center text-sm text-muted">{children}</div>;
}

type TipRow = { name?: string; value: string; color?: string };

function Tip({ x, y, width, title, rows }: { x: number; y: number; width: number; title: string; rows: TipRow[] }) {
  const flip = x > width * 0.6;
  return (
    <div role="tooltip" className="pointer-events-none absolute z-10 min-w-32 rounded-md border border-line bg-bg/95 px-3 py-2 text-xs shadow-lg"
      style={{ left: flip ? undefined : x + 14, right: flip ? width - x + 14 : undefined, top: Math.max(0, y) }}>
      <div className="mb-1 text-muted">{title}</div>
      {rows.map((r, i) => (
        <div key={i} className="flex items-center justify-between gap-4">
          <span className="flex items-center gap-1.5 text-muted">
            {r.color && <span aria-hidden className="h-0.5 w-3 rounded" style={{ background: r.color }} />}{r.name}
          </span>
          <span className="num font-semibold text-slate-100">{r.value}</span>
        </div>
      ))}
    </div>
  );
}

// ---- line chart ------------------------------------------------------------------------------------------------

export type Series = { name: string; color: string; points: { t: number; v: number }[] };

export function LineChart({ series, height = 220, format = commas, percent = false, empty }: {
  series: Series[]; height?: number; format?: (n: number) => string; percent?: boolean; empty?: ReactNode;
}) {
  const [ref, width] = useWidth();
  const [hover, setHover] = useState<number | null>(null);
  const ts = useMemo(() => [...new Set(series.flatMap((s) => s.points.map((p) => p.t)))].sort((a, b) => a - b), [series]);
  if (ts.length < 2) return <Empty>{empty ?? "Collecting history. Points appear after the next scan or replacement."}</Empty>;

  const m = { l: 40, r: 56, t: 12, b: 24 };
  const w = width - m.l - m.r;
  const h = height - m.t - m.b;
  const max = percent ? 100 : Math.max(1, ...series.flatMap((s) => s.points.map((p) => p.v)));
  const ticks = percent ? [0, 25, 50, 75, 100] : niceTicks(max);
  const top = ticks[ticks.length - 1];
  const t0 = ts[0], t1 = ts[ts.length - 1], span = t1 - t0;
  const X = (t: number) => m.l + ((t - t0) / span) * w;
  const Y = (v: number) => m.t + h - (v / top) * h;
  const at = (s: Series, t: number) => s.points.find((p) => p.t === t) ?? [...s.points].reverse().find((p) => p.t <= t);
  const xTicks = Array.from({ length: Math.min(5, ts.length) }, (_, i) => t0 + (span * i) / (Math.min(5, ts.length) - 1));
  const single = series.length === 1;

  const move = (e: React.PointerEvent<SVGRectElement>) => {
    const box = e.currentTarget.getBoundingClientRect();
    const x = ((e.clientX - box.left) / box.width) * w + m.l;
    let best = 0, d = Infinity;
    ts.forEach((t, i) => { const dd = Math.abs(X(t) - x); if (dd < d) { d = dd; best = i; } });
    setHover(best);
  };
  const key = (e: React.KeyboardEvent) => {
    if (e.key === "ArrowLeft") setHover((i) => Math.max(0, (i ?? ts.length) - 1));
    else if (e.key === "ArrowRight") setHover((i) => Math.min(ts.length - 1, (i ?? -1) + 1));
    else if (e.key === "Escape") setHover(null);
  };
  const ht = hover === null ? null : ts[hover];

  return (
    <div ref={ref} className="relative" style={{ height }}>
      <svg width={width} height={height} role="img" tabIndex={0} onKeyDown={key} onBlur={() => setHover(null)} onFocus={() => setHover((i) => i ?? ts.length - 1)}
        aria-label={`${series.map((s) => s.name).join(", ")} over time. Use the arrow keys to read values.`}>
        {ticks.map((v) => (
          <g key={v}>
            <line x1={m.l} x2={m.l + w} y1={Y(v)} y2={Y(v)} stroke={v === 0 ? "var(--viz-axis)" : "var(--viz-grid)"} strokeWidth={1} />
            <text x={m.l - 8} y={Y(v) + 4} textAnchor="end" fontSize={11} fill="var(--viz-muted)" className="num">{percent ? `${v}%` : compact(v)}</text>
          </g>
        ))}
        {xTicks.map((t, i) => (
          <text key={i} x={X(t)} y={height - 6} fontSize={11} fill="var(--viz-muted)" textAnchor={i === 0 ? "start" : i === xTicks.length - 1 ? "end" : "middle"}>{fmtTime(t, span)}</text>
        ))}
        {series.map((s) => {
          const pts = s.points.map((p) => `${X(p.t)},${Y(p.v)}`).join(" ");
          const last = s.points[s.points.length - 1];
          return (
            <g key={s.name}>
              {single && <polygon points={`${X(s.points[0].t)},${Y(0)} ${pts} ${X(last.t)},${Y(0)}`} fill={s.color} fillOpacity={0.1} />}
              <polyline points={pts} fill="none" stroke={s.color} strokeWidth={2} strokeLinejoin="round" strokeLinecap="round" />
              <circle cx={X(last.t)} cy={Y(last.v)} r={4} fill={s.color} stroke="var(--viz-surface)" strokeWidth={2} />
              <text x={X(last.t) + 10} y={Y(last.v) + 4} fontSize={12} fontWeight={600} fill="var(--viz-ink)" className="num">{percent ? `${Math.round(last.v)}%` : format(last.v)}</text>
            </g>
          );
        })}
        {ht !== null && (
          <g>
            <line x1={X(ht)} x2={X(ht)} y1={m.t} y2={m.t + h} stroke="var(--viz-muted)" strokeWidth={1} />
            {series.map((s) => { const p = at(s, ht); return p ? <circle key={s.name} cx={X(ht)} cy={Y(p.v)} r={4} fill={s.color} stroke="var(--viz-surface)" strokeWidth={2} /> : null; })}
          </g>
        )}
        <rect x={m.l} y={m.t} width={w} height={h} fill="transparent" onPointerMove={move} onPointerLeave={() => setHover(null)} />
      </svg>
      {ht !== null && (
        <Tip x={X(ht)} y={m.t} width={width} title={new Date(ht * 1000).toLocaleString([], { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" })}
          rows={series.map((s) => { const p = at(s, ht); return { name: s.name, color: s.color, value: p ? (percent ? `${p.v.toFixed(1)}%` : format(p.v)) : "–" }; })} />
      )}
    </div>
  );
}

// ---- columns ---------------------------------------------------------------------------------------------------

export function Columns({ data, color = "var(--series-1)", height = 200, format = commas, unit = "" }: {
  data: { label: string; value: number; sub?: string }[]; color?: string; height?: number; format?: (n: number) => string; unit?: string;
}) {
  const [ref, width] = useWidth();
  const [hover, setHover] = useState<number | null>(null);
  const m = { l: 36, r: 8, t: 14, b: 24 };
  const w = width - m.l - m.r, h = height - m.t - m.b;
  const ticks = niceTicks(Math.max(1, ...data.map((d) => d.value)), 3);
  const top = ticks[ticks.length - 1];
  const band = w / Math.max(1, data.length);
  const bw = Math.min(24, band * 0.6);
  const maxI = data.reduce((b, d, i) => (d.value > data[b].value ? i : b), 0);
  const every = Math.ceil(data.length / Math.max(1, Math.floor(w / 56)));
  return (
    <div ref={ref} className="relative" style={{ height }}>
      <svg width={width} height={height} role="img" aria-label={`Bar chart of ${data.length} values`}>
        {ticks.map((v) => (
          <g key={v}>
            <line x1={m.l} x2={m.l + w} y1={m.t + h - (v / top) * h} y2={m.t + h - (v / top) * h} stroke={v === 0 ? "var(--viz-axis)" : "var(--viz-grid)"} />
            <text x={m.l - 8} y={m.t + h - (v / top) * h + 4} textAnchor="end" fontSize={11} fill="var(--viz-muted)" className="num">{compact(v)}</text>
          </g>
        ))}
        {data.map((d, i) => {
          const cx = m.l + band * i + band / 2;
          const bh = (d.value / top) * h;
          const x = cx - bw / 2, y = m.t + h - bh, r = Math.min(4, bh);
          return (
            <g key={i} onPointerEnter={() => setHover(i)} onPointerLeave={() => setHover(null)}>
              <rect x={m.l + band * i} y={m.t} width={band} height={h} fill="transparent" />
              {d.value > 0 && (
                <path d={`M${x},${y + bh} V${y + r} Q${x},${y} ${x + r},${y} H${x + bw - r} Q${x + bw},${y} ${x + bw},${y + r} V${y + bh} Z`}
                  fill={color} opacity={hover === null || hover === i ? 1 : 0.55} />
              )}
              {(i === maxI || i === data.length - 1) && d.value > 0 && (
                <text x={cx} y={y - 5} textAnchor="middle" fontSize={11} fontWeight={600} fill="var(--viz-ink)" className="num">{format(d.value)}</text>
              )}
              {i % every === 0 && <text x={cx} y={height - 6} textAnchor="middle" fontSize={11} fill="var(--viz-muted)">{d.label}</text>}
            </g>
          );
        })}
      </svg>
      {hover !== null && (
        <Tip x={m.l + band * hover + band / 2} y={m.t} width={width} title={data[hover].label}
          rows={[{ value: `${format(data[hover].value)}${unit}` }, ...(data[hover].sub ? [{ name: data[hover].sub!, value: "" }] : [])]} />
      )}
    </div>
  );
}

// ---- horizontal bars ------------------------------------------------------------------------------------------

export function HBars({ rows, color = "var(--series-1)", format = commas }: {
  rows: { label: string; value: number; sub?: string }[]; color?: string; format?: (n: number) => string;
}) {
  const max = Math.max(1, ...rows.map((r) => r.value));
  const [hover, setHover] = useState<number | null>(null);
  return (
    <ul className="space-y-2">
      {rows.map((r, i) => (
        <li key={i} className="grid grid-cols-[7.5rem_1fr_3rem] items-center gap-3 text-xs" tabIndex={0}
          onPointerEnter={() => setHover(i)} onPointerLeave={() => setHover(null)} onFocus={() => setHover(i)} onBlur={() => setHover(null)}
          title={`${r.label}: ${format(r.value)}${r.sub ? ` (${r.sub})` : ""}`}>
          <span className="truncate text-slate-300">{r.label}</span>
          <span className="h-3.5">
            <span className="block h-full rounded-r-[4px]" style={{ width: `${(r.value / max) * 100}%`, minWidth: r.value > 0 ? 3 : 0, background: color, opacity: hover === null || hover === i ? 1 : 0.55 }} />
          </span>
          <span className="num text-right font-semibold text-slate-100">{format(r.value)}</span>
        </li>
      ))}
    </ul>
  );
}

// ---- part-to-whole: one stacked bar ---------------------------------------------------------------------------

export function Stacked({ parts, total }: { parts: { name: string; value: number; color: string }[]; total?: number }) {
  const sum = total ?? parts.reduce((a, p) => a + p.value, 0);
  const [hover, setHover] = useState<number | null>(null);
  if (sum <= 0) return <Empty>Nothing scanned yet.</Empty>;
  return (
    <div>
      <div className="flex h-8 w-full overflow-hidden rounded-[4px]" role="img" aria-label={parts.map((p) => `${p.name} ${commas(p.value)}`).join(", ")}>
        {parts.filter((p) => p.value > 0).map((p, i, arr) => (
          <div key={p.name} className="relative flex items-center justify-center text-[11px] font-semibold text-black/80"
            style={{ width: `${(p.value / sum) * 100}%`, background: p.color, marginRight: i < arr.length - 1 ? 2 : 0, opacity: hover === null || hover === i ? 1 : 0.55 }}
            onPointerEnter={() => setHover(i)} onPointerLeave={() => setHover(null)} title={`${p.name}: ${commas(p.value)} (${((p.value / sum) * 100).toFixed(1)}%)`}>
            {(p.value / sum) * 100 > 12 ? `${Math.round((p.value / sum) * 100)}%` : ""}
          </div>
        ))}
      </div>
      <ul className="mt-3 grid grid-cols-2 gap-x-4 gap-y-1 text-xs sm:grid-cols-4">
        {parts.map((p) => (
          <li key={p.name} className="flex items-center gap-1.5">
            <span aria-hidden className="h-2.5 w-2.5 shrink-0 rounded-[2px]" style={{ background: p.color }} />
            <span className="text-muted">{p.name}</span>
            <span className="num ml-auto font-semibold text-slate-100">{commas(p.value)}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}

// ---- figures ---------------------------------------------------------------------------------------------------

export function Sparkline({ values, width = 84, height = 24 }: { values: number[]; width?: number; height?: number }) {
  if (values.length < 2) return null;
  const min = Math.min(...values), max = Math.max(...values), r = max - min || 1;
  const X = (i: number) => 2 + (i / (values.length - 1)) * (width - 4);
  const Y = (v: number) => height - 3 - ((v - min) / r) * (height - 6);
  return (
    <svg width={width} height={height} aria-hidden>
      <polyline points={values.map((v, i) => `${X(i)},${Y(v)}`).join(" ")} fill="none" stroke="var(--viz-quiet)" strokeWidth={1.5} strokeLinejoin="round" strokeLinecap="round" />
      <circle cx={X(values.length - 1)} cy={Y(values[values.length - 1])} r={3} fill="var(--series-1)" stroke="var(--viz-surface)" strokeWidth={2} />
    </svg>
  );
}

/** Delta chip: colour is direction x whether up is good; always paired with an arrow and a sign. */
export function Delta({ now, before, upIsGood, label }: { now: number; before?: number; upIsGood: boolean; label: string }) {
  if (before === undefined || now === before) return <span className="text-xs text-muted">no change {label}</span>;
  const d = now - before, up = d > 0, good = up === upIsGood;
  return (
    <span className={`num text-xs ${good ? "text-good" : "text-warn"}`}>
      {up ? "▲" : "▼"} {up ? "+" : "−"}{commas(Math.abs(d))} <span className="text-muted">{label}</span>
    </span>
  );
}

export function StatTile({ label, value, hero = false, footer, spark, hint }: {
  label: string; value: string | number; hero?: boolean; footer?: ReactNode; spark?: number[]; hint?: string;
}) {
  return (
    <div className="card flex min-h-28 flex-col justify-between p-4" title={hint}>
      <div className="text-xs uppercase tracking-wide text-muted">{label}</div>
      <div className="flex items-end justify-between gap-2">
        <div className={`font-semibold leading-none ${hero ? "text-5xl" : "text-3xl"}`}>{value}</div>
        {spark && <Sparkline values={spark} />}
      </div>
      <div className="mt-2 min-h-4">{footer}</div>
    </div>
  );
}

export function Meter({ value, max, label }: { value: number; max: number; label?: string }) {
  const r = max > 0 ? Math.min(1, value / max) : 0;
  const fill = r >= 1 ? "var(--viz-warn)" : "var(--series-1)";
  return (
    <div role="meter" aria-valuenow={value} aria-valuemin={0} aria-valuemax={max} aria-label={label}>
      <div className="h-2 w-full overflow-hidden rounded-full" style={{ background: "color-mix(in srgb, var(--series-1) 18%, transparent)" }}>
        <div className="h-full rounded-full" style={{ width: `${r * 100}%`, background: fill }} />
      </div>
    </div>
  );
}
