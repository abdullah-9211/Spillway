import type { components } from "./api-types";

export type UsageSummary = components["schemas"]["UsageSummary"];
export type UsageGroup = components["schemas"]["UsageGroup"];
export type ProviderHealth = components["schemas"]["ProviderHealth"];
export type CacheSplit = components["schemas"]["CacheSplit"];

// --- the date range ---

export type Preset = "7d" | "14d" | "30d" | "90d" | "month" | "last-month" | "180d" | "365d" | "custom";

/** The ranges shown as buttons. */
export const PRESETS: { value: Preset; label: string }[] = [
  { value: "7d", label: "7 days" },
  { value: "14d", label: "14 days" },
  { value: "30d", label: "30 days" },
  { value: "90d", label: "90 days" },
];

/** Ranges in the "more" menu. Custom opens a pair of date fields. */
export const MORE_PRESETS: { value: Preset; label: string }[] = [
  { value: "month", label: "This month" },
  { value: "last-month", label: "Last month" },
  { value: "180d", label: "Last 6 months" },
  { value: "365d", label: "Last 12 months" },
  { value: "custom", label: "Custom range…" },
];

const ALL: Preset[] = ["7d", "14d", "30d", "90d", "month", "last-month", "180d", "365d", "custom"];

export function parsePreset(v: string | undefined): Preset {
  return ALL.includes(v as Preset) ? (v as Preset) : "14d";
}

export const MAX_RANGE_DAYS = 366;
const DATE = /^\d{4}-\d{2}-\d{2}$/;

/** A custom range from the URL, or an explanation of what is wrong with it. */
export function parseCustom(from: string | undefined, to: string | undefined, now: Date = new Date()): { from: string; to: string } | { error: string } {
  // Date.parse accepts 2026-02-30 and rolls it into March, so check that the date survives a round trip.
  const real = (d: string) => DATE.test(d) && new Date(`${d}T00:00:00Z`).toISOString().slice(0, 10) === d;
  if (!from || !to || !real(from) || !real(to)) {
    return { error: "Choose a first and a last day." };
  }
  if (from > to) return { error: "The first day is after the last day." };
  const today = iso(new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate())));
  if (to > today) return { error: "The last day cannot be in the future." };
  const days = Math.round((Date.parse(to) - Date.parse(from)) / 86_400_000) + 1;
  if (days > MAX_RANGE_DAYS) return { error: `Choose at most ${MAX_RANGE_DAYS} days at a time.` };
  return { from, to };
}

const iso = (d: Date) => d.toISOString().slice(0, 10);

/** The inclusive UTC dates a preset covers, ending today. Days are UTC because that is how Spillway buckets them. */
export function rangeFor(preset: Preset, now: Date = new Date()): { from: string; to: string } {
  const today = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate()));
  const back = (n: number) => iso(new Date(today.getTime() - n * 86_400_000));
  switch (preset) {
    case "7d":
      return { from: back(6), to: iso(today) };
    case "30d":
      return { from: back(29), to: iso(today) };
    case "90d":
      return { from: back(89), to: iso(today) };
    case "180d":
      return { from: back(179), to: iso(today) };
    case "365d":
      return { from: back(364), to: iso(today) };
    case "month":
      return { from: iso(new Date(Date.UTC(today.getUTCFullYear(), today.getUTCMonth(), 1))), to: iso(today) };
    case "last-month": {
      const first = new Date(Date.UTC(today.getUTCFullYear(), today.getUTCMonth() - 1, 1));
      const last = new Date(Date.UTC(today.getUTCFullYear(), today.getUTCMonth(), 0));
      return { from: iso(first), to: iso(last) };
    }
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

// --- colours shared by the chart and the table ---

const COLORS = ["var(--s1)", "var(--s2)", "var(--s3)"];

/** The two models that cost most get their own colour; everything else is grey. Same rule the chart uses for spend. */
export function modelColors(byModel: UsageGroup[]): (model: string) => string {
  const paid = byModel.filter((m) => microsOf(m.cost_usd) > 0n).sort((a, b) => (microsOf(b.cost_usd) > microsOf(a.cost_usd) ? 1 : -1));
  const named = new Map(paid.slice(0, 2).map((m, i) => [m.key, COLORS[i]]));
  return (m) => named.get(m) ?? COLORS[2];
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

// --- the key filter's choices ---

export const MAX_KEY_CHOICES = 40;

/**
 * Keys worth offering in the filter: ones that have been used, most recently used first, capped so the menu stays
 * short. The key already selected is always included, so the current view can be shown and left.
 */
export function keyChoices(keys: { id: string; name: string; revoked_at: string | null; last_used_at: string | null; builtin: boolean }[], selected: string): { id: string; name: string }[] {
  const used = keys
    .filter((k) => k.last_used_at && !k.builtin)
    .sort((a, b) => (b.last_used_at as string).localeCompare(a.last_used_at as string))
    .slice(0, MAX_KEY_CHOICES);
  const picked = used.some((k) => k.id === selected) ? used : [...used, ...keys.filter((k) => k.id === selected)];
  return picked.map((k) => ({ id: k.id, name: k.revoked_at ? `${k.name} (revoked)` : k.name }));
}
