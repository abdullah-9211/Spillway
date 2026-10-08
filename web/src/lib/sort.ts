import type { UsageGroup } from "./usage";
import { microsOf } from "./usage";

export type ModelSort = "model" | "requests" | "input" | "output" | "cost" | "p50" | "p95" | "cacheHit" | "errors";
export type Dir = "asc" | "desc";

export const COLUMNS: { key: ModelSort; label: string; numeric: boolean; hint?: string }[] = [
  { key: "model", label: "Model", numeric: false },
  { key: "requests", label: "Requests", numeric: true },
  { key: "input", label: "Tokens in", numeric: true },
  { key: "output", label: "Tokens out", numeric: true },
  { key: "cost", label: "Cost", numeric: true },
  { key: "p50", label: "p50", numeric: true, hint: "Median time for successful requests answered by a provider, not by a cache" },
  { key: "p95", label: "p95", numeric: true, hint: "95th percentile of the same" },
  { key: "cacheHit", label: "Cache hit", numeric: true, hint: "Share of requests answered from a cache" },
  { key: "errors", label: "Errors", numeric: true, hint: "Share of requests every provider failed" },
];

/** The first click on a column sorts the way people expect: names A to Z, numbers highest first. */
export function defaultDir(key: ModelSort): Dir {
  return key === "model" ? "asc" : "desc";
}

const rate = (part: number, whole: number) => (whole > 0 ? part / whole : 0);

function value(m: UsageGroup, key: ModelSort): number | string | null {
  switch (key) {
    case "model": return m.label.toLowerCase();
    case "requests": return m.requests;
    case "input": return m.input_tokens;
    case "output": return m.output_tokens;
    case "cost": return Number(microsOf(m.cost_usd));
    case "p50": return m.timed_requests > 0 ? m.p50_ms : null;
    case "p95": return m.timed_requests > 0 ? m.p95_ms : null;
    case "cacheHit": return rate(m.cache_hits, m.requests);
    case "errors": return rate(m.errors, m.requests);
  }
}

/**
 * Sorts a copy of the models. A model with no figure for the column (no latency measured) always goes last, whichever
 * way the column is sorted, so a dash never outranks a real number. Ties fall back to name so the order is stable.
 */
export function sortModels(models: UsageGroup[], key: ModelSort, dir: Dir): UsageGroup[] {
  const sign = dir === "asc" ? 1 : -1;
  return [...models].sort((a, b) => {
    const va = value(a, key);
    const vb = value(b, key);
    if (va === null && vb === null) return a.label.localeCompare(b.label);
    if (va === null) return 1;
    if (vb === null) return -1;
    const c = typeof va === "string" ? va.localeCompare(vb as string) : (va as number) - (vb as number);
    return c * sign || a.label.localeCompare(b.label);
  });
}

/** Keeps models whose name contains every word typed. */
export function filterModels(models: UsageGroup[], query: string): UsageGroup[] {
  const words = query.toLowerCase().split(/\s+/).filter(Boolean);
  return words.length ? models.filter((m) => words.every((w) => m.label.toLowerCase().includes(w))) : models;
}
