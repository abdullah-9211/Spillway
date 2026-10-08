import type { components } from "./api-types";

export type UsageSummary = components["schemas"]["UsageSummary"];
export type UsageGroup = components["schemas"]["UsageGroup"];
export type ProviderHealth = components["schemas"]["ProviderHealth"];
export type CacheSplit = components["schemas"]["CacheSplit"];

// --- the date range ---

export type Preset = "7d" | "14d" | "month";
export const PRESETS: { value: Preset; label: string }[] = [
  { value: "7d", label: "7 days" },
  { value: "14d", label: "14 days" },
  { value: "month", label: "This month" },
];

export function parsePreset(v: string | undefined): Preset {
  return v === "7d" || v === "month" ? v : "14d";
}

const iso = (d: Date) => d.toISOString().slice(0, 10);

/** The inclusive UTC dates a preset covers, ending today. Days are UTC because that is how Spillway buckets them. */
export function rangeFor(preset: Preset, now: Date = new Date()): { from: string; to: string } {
  const today = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate()));
  const back = (n: number) => iso(new Date(today.getTime() - n * 86_400_000));
  switch (preset) {
    case "7d":
      return { from: back(6), to: iso(today) };
    case "month":
      return { from: iso(new Date(Date.UTC(today.getUTCFullYear(), today.getUTCMonth(), 1))), to: iso(today) };
    default:
      return { from: back(13), to: iso(today) };
  }
}

// --- exact dollars ---

/** Micro-dollars in a six-place dollar string, as a BigInt, so sums are exact. */
export function microsOf(amount: string): bigint {
  const neg = amount.startsWith("-");
  const [whole, frac = ""] = amount.replace("-", "").split(".");
  const v = BigInt(whole || "0") * 1_000_000n + BigInt((frac + "000000").slice(0, 6));
  return neg ? -v : v;
}

export function dollarsOf(m: bigint): string {
  const neg = m < 0n;
  const a = neg ? -m : m;
  return `${neg ? "-" : ""}${a / 1_000_000n}.${(a % 1_000_000n).toString().padStart(6, "0")}`;
}

export function sumUsd(amounts: string[]): string {
  return dollarsOf(amounts.reduce((t, a) => t + microsOf(a), 0n));
}

// --- numbers for reading ---

export function count(n: number): string {
  return n.toLocaleString("en-US");
}

/** 61,300,000 -> "61.3M". Counts below a thousand stay exact. */
export function compact(n: number): string {
  const abs = Math.abs(n);
  const unit = (v: number, s: string) => `${v >= 100 ? Math.round(v) : Math.round(v * 10) / 10}${s}`;
  if (abs >= 1e9) return unit(n / 1e9, "B");
  if (abs >= 1e6) return unit(n / 1e6, "M");
  if (abs >= 1e4) return unit(n / 1e3, "K");
  return count(n);
}

export function percent(part: number, whole: number, places = 0): string {
  if (whole <= 0) return "0%";
  return `${((part / whole) * 100).toFixed(places)}%`;
}

/** A latency in milliseconds, as a person would say it. */
export function ms(v: number | null | undefined): string {
  if (v == null) return "—";
  if (v < 1) return "<1 ms";
  if (v < 1000) return `${Math.round(v)} ms`;
  return `${(v / 1000).toFixed(v < 10_000 ? 1 : 0)}s`;
}

/** Shares that add up to exactly 100, using the largest-remainder method so rounding never leaves 99 or 101. */
export function shares(values: number[]): number[] {
  const total = values.reduce((a, b) => a + b, 0);
  if (total <= 0) return values.map(() => 0);
  const raw = values.map((v) => (v / total) * 100);
  const floor = raw.map(Math.floor);
  let left = 100 - floor.reduce((a, b) => a + b, 0);
  const order = raw.map((r, i) => ({ i, rem: r - Math.floor(r) })).sort((a, b) => b.rem - a.rem || a.i - b.i);
  for (const { i } of order) {
    if (left-- <= 0) break;
    floor[i]++;
  }
  return floor;
}

// --- the spend-per-day chart ---

export type Series = { model: string; color: string; label: string };

const COLORS = ["var(--s1)", "var(--s2)", "var(--s3)"];
export const OTHER = "other";

/**
 * Chooses what the chart draws: the two models that cost most get their own colours, everything else paid is
 * grouped as "other", and models that cost nothing are named in the legend but have no bar.
 */
export function chartSeries(days: UsageGroup[], byModel: UsageGroup[]): { series: Series[]; free: string[]; colorOf: (m: string) => string } {
  const paid = byModel.filter((m) => microsOf(m.cost_usd) > 0n).sort((a, b) => (microsOf(b.cost_usd) > microsOf(a.cost_usd) ? 1 : -1));
  const free = byModel.filter((m) => microsOf(m.cost_usd) === 0n && m.requests > 0 && m.key !== "").map((m) => m.label);
  const named = paid.slice(0, 2);
  const series: Series[] = named.map((m, i) => ({ model: m.key, color: COLORS[i], label: m.label }));
  if (paid.length > 2) series.push({ model: OTHER, color: COLORS[2], label: "other models" });
  // A model that shows up on a day but not in the by-model list still has to be drawn, so it joins "other".
  const named_ = new Set(named.map((m) => m.key));
  const stray = days.some((d) => Object.entries(d.models ?? {}).some(([m, c]) => !named_.has(m) && microsOf(c) > 0n));
  if (stray && !series.some((x) => x.model === OTHER)) series.push({ model: OTHER, color: COLORS[2], label: "other models" });
  const colorOf = (m: string) => series.find((s) => s.model === m)?.color ?? COLORS[2];
  return { series, free, colorOf };
}

export type Bar = { day: string; label: string; total: string; segments: { model: string; color: string; cost: string; height: number }[]; tip: string; height: number };

const STEPS = [1, 2, 2.5, 5, 10];

/** The smallest "round" ceiling (1, 2, 2.5, 5 times a power of ten) at or above v, for a tidy y axis. */
export function niceCeil(v: number): number {
  if (v <= 0) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  return (STEPS.find((s) => s * p >= v - 1e-12) ?? 10) * p;
}

export function tickLabel(v: number, top: number): string {
  if (v === 0) return "$0";
  if (top >= 4) return `$${Math.round(v)}`;
  if (top >= 0.4) return `$${v.toFixed(2)}`;
  return `$${v.toFixed(4)}`.replace(/0+$/, "").replace(/\.$/, ".0");
}

export function buildBars(days: UsageGroup[], series: Series[]): { bars: Bar[]; top: number; ticks: string[] } {
  const totals = days.map((d) => Number(d.cost_usd));
  const top = niceCeil(Math.max(0, ...totals));
  const bars = days.map((d) => {
    const parts = new Map<string, bigint>();
    for (const [m, c] of Object.entries(d.models ?? {})) {
      const key = series.some((s) => s.model === m) ? m : OTHER;
      parts.set(key, (parts.get(key) ?? 0n) + microsOf(c));
    }
    const segments = series
      .map((s) => ({ model: s.model, color: s.color, cost: dollarsOf(parts.get(s.model) ?? 0n) }))
      .filter((s) => microsOf(s.cost) > 0n)
      .map((s) => ({ ...s, height: (Number(s.cost) / top) * 100 }));
    const dayNum = Number(d.key.slice(8, 10));
    const monthName = new Date(`${d.key}T00:00:00Z`).toLocaleDateString("en-US", { month: "short", timeZone: "UTC" });
    const detail = series.map((s) => `${s.label} ${fmtUsd(parts.get(s.model) ?? 0n)}`).join(", ");
    return {
      day: d.key,
      label: dayNum === 1 ? `${monthName} 1` : String(dayNum),
      total: d.cost_usd,
      segments,
      height: (Number(d.cost_usd) / top) * 100,
      tip: `${monthName} ${dayNum}: ${fmtUsd(microsOf(d.cost_usd))} spent${series.length ? ` (${detail})` : ""}, ${count(d.requests)} requests`,
    };
  });
  const ticks = [4, 3, 2, 1, 0].map((i) => tickLabel((top * i) / 4, top));
  return { bars, top, ticks };
}

function fmtUsd(m: bigint): string {
  const n = Number(m) / 1e6;
  const places = n === 0 || n >= 0.01 ? 2 : n >= 0.00005 ? 4 : 6;
  return "$" + n.toLocaleString("en-US", { minimumFractionDigits: places, maximumFractionDigits: places });
}

// --- provider health pills ---

export type Pill = { label: string; tone: "ok" | "wait" | "fail" | "mute"; icon: string };

export function breakerPill(p: Pick<ProviderHealth, "state" | "open_remaining_seconds" | "reason">): Pill {
  switch (p.state) {
    case "closed":
      return { label: "Breaker closed", tone: "ok", icon: "M2.5 6.5l2.2 2.2L9.5 3.5" };
    case "half_open":
      return { label: "Half open, probing", tone: "wait", icon: "M6 2.5v4M6 9v.01" };
    case "open":
      return { label: p.open_remaining_seconds != null ? `Breaker open for ${p.open_remaining_seconds}s` : "Breaker open", tone: "fail", icon: "M3 3l6 6M9 3l-6 6" };
    default:
      return { label: "Not configured", tone: "mute", icon: "M2.5 6h7" };
  }
}
