import type { components } from "./api-types";
import { compact, count, microsOf, type UsageGroup } from "./usage";

type SeriesStat = components["schemas"]["SeriesStat"];

export type Metric = "spend" | "requests" | "tokens" | "saved";
export type Stack = "model" | "key";
export type Gran = "day" | "week";

export const METRICS: { value: Metric; label: string }[] = [
  { value: "spend", label: "Spend" },
  { value: "requests", label: "Requests" },
  { value: "tokens", label: "Tokens" },
  { value: "saved", label: "Saved by cache" },
];

export type Cell = { label: string; requests: number; input: number; output: number; cost: bigint; saved: bigint };

/** One bar: a day, or a week of days added together. */
export type Bucket = {
  start: string;
  end: string;
  days: number;
  label: string; // under the bar
  title: string; // in the tooltip and the detail panel
  requests: number;
  input: number;
  output: number;
  cost: bigint;
  saved: bigint;
  cacheHits: number;
  errors: number;
  cells: Map<string, Cell>;
};

const monthDay = (iso: string) => new Date(`${iso}T00:00:00Z`).toLocaleDateString("en-US", { month: "short", day: "numeric", timeZone: "UTC" });

/** The Monday on or before a date (UTC), as YYYY-MM-DD. Weeks start on Monday. */
export function weekStart(iso: string): string {
  const d = new Date(`${iso}T00:00:00Z`);
  d.setUTCDate(d.getUTCDate() - ((d.getUTCDay() + 6) % 7));
  return d.toISOString().slice(0, 10);
}

function emptyBucket(start: string): Bucket {
  return { start, end: start, days: 0, label: "", title: "", requests: 0, input: 0, output: 0, cost: 0n, saved: 0n, cacheHits: 0, errors: 0, cells: new Map() };
}

/** Turns the API's days into bars: one per day, or one per week (partial weeks at the ends keep only the days in range). */
export function toBuckets(days: UsageGroup[], gran: Gran): Bucket[] {
  const out: Bucket[] = [];
  let cur: Bucket | null = null;
  let curKey = "";
  for (const d of days) {
    const key = gran === "week" ? weekStart(d.key) : d.key;
    if (!cur || key !== curKey) {
      cur = emptyBucket(d.key);
      curKey = key;
      out.push(cur);
    }
    cur.end = d.key;
    cur.days++;
    cur.requests += d.requests;
    cur.input += d.input_tokens;
    cur.output += d.output_tokens;
    cur.cost += microsOf(d.cost_usd);
    cur.saved += microsOf(d.saved_usd);
    cur.cacheHits += d.cache_hits;
    cur.errors += d.errors;
    for (const [id, st] of Object.entries((d.series ?? {}) as Record<string, SeriesStat>)) {
      const c = cur.cells.get(id) ?? { label: st.label || id, requests: 0, input: 0, output: 0, cost: 0n, saved: 0n };
      c.requests += st.requests;
      c.input += st.input_tokens;
      c.output += st.output_tokens;
      c.cost += microsOf(st.cost_usd);
      c.saved += microsOf(st.saved_usd);
      cur.cells.set(id, c);
    }
  }
  for (const b of out) {
    if (gran === "day") {
      const day = Number(b.start.slice(8, 10));
      b.label = day === 1 ? monthDay(b.start) : String(day);
      b.title = monthDay(b.start);
    } else {
      b.label = monthDay(b.start);
      b.title = b.days === 7 ? `Week of ${monthDay(b.start)}` : b.days === 1 ? monthDay(b.start) : `${monthDay(b.start)} to ${monthDay(b.end)}`;
    }
  }
  return out;
}

// --- one number per metric ---

type Measured = { requests: number; input: number; output: number; cost: bigint; saved: bigint };

/** The figure a metric plots. Money is turned into a number of dollars only here, for drawing; sums stay exact. */
export function valueOf(m: Measured, metric: Metric): number {
  switch (metric) {
    case "spend":
      return Number(m.cost) / 1e6;
    case "saved":
      return Number(m.saved) / 1e6;
    case "requests":
      return m.requests;
    case "tokens":
      return m.input + m.output;
  }
}

function dollars(n: number): string {
  const abs = Math.abs(n);
  const places = abs === 0 || abs >= 0.01 ? 2 : abs >= 0.00005 ? 4 : 6;
  return "$" + n.toLocaleString("en-US", { minimumFractionDigits: places, maximumFractionDigits: places });
}

export function formatValue(v: number, metric: Metric): string {
  return metric === "spend" || metric === "saved" ? dollars(v) : metric === "requests" ? count(Math.round(v)) : compact(Math.round(v));
}

// --- which series get a colour ---

export type SeriesDef = { id: string; label: string; color: string; ids: string[] };
const COLORS = ["var(--s1)", "var(--s2)", "var(--s3)"];
export const OTHER = "other";

/**
 * Chooses what the chart draws for a metric: the two biggest series get their own colours and the rest are grouped as
 * "other". Series that did nothing the metric measures but were active (a free model, on the spend chart) are named in
 * `idle` so the legend can say why they have no bar.
 */
export function planSeries(buckets: Bucket[], metric: Metric, noun: string): { series: SeriesDef[]; idle: string[] } {
  const total = new Map<string, { label: string; v: number; requests: number }>();
  for (const b of buckets) {
    for (const [id, c] of b.cells) {
      const t = total.get(id) ?? { label: c.label, v: 0, requests: 0 };
      t.v += valueOf(c, metric);
      t.requests += c.requests;
      total.set(id, t);
    }
  }
  const all = [...total.entries()];
  const drawn = all.filter(([, t]) => t.v > 0).sort((a, b) => b[1].v - a[1].v || a[1].label.localeCompare(b[1].label));
  const series: SeriesDef[] = drawn.slice(0, 2).map(([id, t], i) => ({ id, label: t.label, color: COLORS[i], ids: [id] }));
  if (drawn.length > 2) series.push({ id: OTHER, label: `other ${noun}`, color: COLORS[2], ids: drawn.slice(2).map(([id]) => id) });
  const idle = all.filter(([, t]) => t.v === 0 && t.requests > 0).map(([, t]) => t.label).sort();
  return { series, idle };
}

// --- bar geometry ---

export type Seg = { id: string; label: string; color: string; value: number; height: number };
export type BarModel = { index: number; bucket: Bucket; segs: Seg[]; total: number; height: number; tip: string };

const STEPS = [1, 2, 2.5, 5, 10];

/** The smallest "round" ceiling (1, 2, 2.5, 5 times a power of ten) at or above v, for a tidy y axis. */
export function niceCeil(v: number): number {
  if (v <= 0) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  return (STEPS.find((s) => s * p >= v - 1e-12) ?? 10) * p;
}

export function tickLabel(v: number, top: number, metric: Metric = "spend"): string {
  if (v === 0) return metric === "spend" || metric === "saved" ? "$0" : "0";
  if (metric === "requests" || metric === "tokens") return compact(Math.round(v));
  if (top >= 4) return `$${Math.round(v)}`;
  if (top >= 0.4) return `$${v.toFixed(2)}`;
  return `$${v.toFixed(4)}`.replace(/0+$/, "").replace(/\.$/, ".0");
}

/**
 * Stacks the visible series for each bucket. Hidden series are left out and the axis rescales to what remains, so
 * switching a series off makes the others easier to read.
 */
export function buildBars(buckets: Bucket[], metric: Metric, series: SeriesDef[], hidden: ReadonlySet<string>): { bars: BarModel[]; top: number; ticks: string[] } {
  const visible = series.filter((s) => !hidden.has(s.id));
  const raw = buckets.map((b) => {
    const segs = visible
      .map((s) => {
        let sum: Measured = { requests: 0, input: 0, output: 0, cost: 0n, saved: 0n };
        for (const id of s.ids) {
          const c = b.cells.get(id);
          if (c) sum = { requests: sum.requests + c.requests, input: sum.input + c.input, output: sum.output + c.output, cost: sum.cost + c.cost, saved: sum.saved + c.saved };
        }
        return { s, value: valueOf(sum, metric) };
      })
      .filter((x) => x.value > 0);
    return { b, segs, total: segs.reduce((a, x) => a + x.value, 0) };
  });
  let top = niceCeil(Math.max(0, ...raw.map((r) => r.total)));
  if ((metric === "requests" || metric === "tokens") && top < 4) top = 4; // whole-number ticks
  const bars: BarModel[] = raw.map((r, index) => ({
    index,
    bucket: r.b,
    total: r.total,
    height: (r.total / top) * 100,
    segs: r.segs.map((x) => ({ id: x.s.id, label: x.s.label, color: x.s.color, value: x.value, height: (x.value / top) * 100 })),
    tip: tooltipText(r.b, metric, r.total, r.segs.map((x) => ({ label: x.s.label, value: x.value }))),
  }));
  return { bars, top, ticks: [4, 3, 2, 1, 0].map((i) => tickLabel((top * i) / 4, top, metric)) };
}

export function tooltipText(b: Bucket, metric: Metric, total: number, parts: { label: string; value: number }[]): string {
  const what = metric === "spend" ? "spent" : metric === "saved" ? "saved by cache" : metric === "requests" ? "requests" : "tokens";
  const head = metric === "requests" || metric === "tokens" ? `${b.title}: ${formatValue(total, metric)} ${what}` : `${b.title}: ${formatValue(total, metric)} ${what}`;
  const detail = parts.length > 1 ? ` (${parts.map((p) => `${p.label} ${formatValue(p.value, metric)}`).join(", ")})` : "";
  return `${head}${detail}`;
}

/** The days a bar covers, as the range to zoom to. */
export function zoomRange(b: Bucket): { from: string; to: string } {
  return { from: b.start, to: b.end };
}

/** Default bar size for a range: days up to about six weeks, weeks beyond that. */
export function defaultGran(dayCount: number): Gran {
  return dayCount > 45 ? "week" : "day";
}

// --- keeping long lists readable ---

export const DETAIL_ROWS = 8;
export const IDLE_NAMES = 3;

/**
 * The per-series list in a pinned bar: series with the same label are merged (many keys can share a name), the
 * biggest few are listed, and the rest are summarised in one line, so a hundred small keys never become a hundred rows.
 */
export function detailRows(b: Bucket, metric: Metric, limit = DETAIL_ROWS): { rows: { label: string; value: number; n: number }[]; more: { count: number; value: number } | null; idle: number } {
  const merged = new Map<string, { label: string; value: number; n: number }>();
  let idle = 0;
  for (const c of b.cells.values()) {
    const v = valueOf(c, metric);
    if (v <= 0) {
      idle++;
      continue;
    }
    const m = merged.get(c.label) ?? { label: c.label, value: 0, n: 0 };
    m.value += v;
    m.n++;
    merged.set(c.label, m);
  }
  const all = [...merged.values()].sort((a, b) => b.value - a.value || a.label.localeCompare(b.label));
  const rest = all.slice(limit);
  return { rows: all.slice(0, limit), more: rest.length ? { count: rest.reduce((a, r) => a + r.n, 0), value: rest.reduce((a, r) => a + r.value, 0) } : null, idle };
}

/** Distinct names, in order, for the legend's "no bar" entries. */
export function uniqueNames(names: string[]): string[] {
  return [...new Set(names)];
}
