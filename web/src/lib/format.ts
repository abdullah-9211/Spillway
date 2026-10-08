import type { components } from "./api-types";

export type ApiKey = components["schemas"]["ApiKey"];

/**
 * Dollars for display. The API sends exact six-place strings; this only shortens them for reading. Amounts under a
 * cent keep four places so a $0.0045 request does not read as $0.00.
 */
export function usd(amount: string | null | undefined): string {
  if (amount == null) return "";
  const n = Number(amount);
  if (!Number.isFinite(n)) return amount;
  // Under a cent, keep enough places that a real amount never reads as $0.00 (down to a millionth of a dollar).
  const abs = Math.abs(n);
  const places = abs === 0 || abs >= 0.01 ? 2 : abs >= 0.00005 ? 4 : 6;
  return "$" + n.toLocaleString("en-US", { minimumFractionDigits: places, maximumFractionDigits: places });
}

export function rpmLabel(rpm: number | null): string {
  return rpm == null ? "No limit" : `${rpm.toLocaleString("en-US")} a min`;
}

export type BudgetState = "none" | "normal" | "close" | "over";

/** A key is close to its limit from 80% of the budget, and over once the budget is spent (spending exactly it counts). */
export const CLOSE_TO_LIMIT = 0.8;

export function budgetState(spend: string, budget: string | null): { state: BudgetState; ratio: number } {
  if (budget == null) return { state: "none", ratio: 0 };
  const s = Number(spend);
  const b = Number(budget);
  if (b <= 0) return { state: "over", ratio: 1 };
  const ratio = s / b;
  return { state: ratio >= 1 ? "over" : ratio >= CLOSE_TO_LIMIT ? "close" : "normal", ratio };
}

export function relativeTime(iso: string | null, now: Date = new Date()): string {
  if (!iso) return "Never";
  const t = new Date(iso);
  const secs = Math.round((now.getTime() - t.getTime()) / 1000);
  if (secs < 45) return "just now";
  const mins = Math.round(secs / 60);
  if (mins < 60) return `${mins} min ago`;
  const hours = Math.round(mins / 60);
  if (hours < 24) return `${hours} h ago`;
  return dateLabel(iso);
}

export function dateLabel(iso: string): string {
  return new Date(iso).toLocaleDateString("en-US", { month: "short", day: "numeric", timeZone: "UTC" });
}

export type KeyFormValues = {
  name: string;
  rpm: string;
  budget: string;
  semanticCache: boolean;
  cacheNonzeroTemp: boolean;
};

export type KeyFormErrors = Partial<Record<"name" | "rpm" | "budget", string>>;

export const MAX_NAME = 64;
export const MAX_RPM = 1_000_000;
export const MAX_BUDGET = 1_000_000;

export function validateKeyForm(v: KeyFormValues): KeyFormErrors {
  const e: KeyFormErrors = {};
  const name = v.name.trim();
  if (!name) e.name = "Give the key a name.";
  else if (name.length > MAX_NAME) e.name = `Use at most ${MAX_NAME} characters.`;
  else if (name.toLowerCase() === "playground") e.name = "That name is reserved for the dashboard's playground.";

  const rpm = v.rpm.trim();
  if (rpm) {
    if (!/^\d+$/.test(rpm) || Number(rpm) < 1 || Number(rpm) > MAX_RPM) e.rpm = `Enter a whole number from 1 to ${MAX_RPM.toLocaleString("en-US")}, or leave it empty for no limit.`;
  }

  const budget = v.budget.trim();
  if (budget) {
    if (!/^\d+(\.\d{1,6})?$/.test(budget)) e.budget = "Enter dollars such as 30 or 30.50, or leave it empty for no limit.";
    else if (Number(budget) > MAX_BUDGET) e.budget = "That is more than $1,000,000 a month.";
  }
  return e;
}

/** The body for POST /api/keys and PATCH /api/keys/{id}. Empty limits become null, which means "no limit". */
export function keyPayload(v: KeyFormValues) {
  return {
    name: v.name.trim(),
    rate_limit_rpm: v.rpm.trim() ? Number(v.rpm.trim()) : null,
    monthly_budget_usd: v.budget.trim() ? v.budget.trim() : null,
    semantic_cache: v.semanticCache,
    cache_nonzero_temp: v.cacheNonzeroTemp,
  };
}

export function formFromKey(k: ApiKey): KeyFormValues {
  return {
    name: k.name,
    rpm: k.rate_limit_rpm == null ? "" : String(k.rate_limit_rpm),
    // Trim trailing zeros so "30.000000" shows as 30 in the field.
    budget: k.monthly_budget_usd == null ? "" : k.monthly_budget_usd.replace(/\.?0+$/, ""),
    semanticCache: k.semantic_cache,
    cacheNonzeroTemp: k.cache_nonzero_temp,
  };
}

export const emptyForm: KeyFormValues = { name: "", rpm: "", budget: "", semanticCache: false, cacheNonzeroTemp: false };

// --- finding keys in a long list ---

export type StatusFilter = "active" | "revoked" | "all";
export type BudgetFilter = "any" | "attention" | "over" | "none";
export type SortKey = "newest" | "name" | "spend" | "recent";

export type KeyFilters = { query: string; status: StatusFilter; budget: BudgetFilter; sort: SortKey };

export const defaultFilters: KeyFilters = { query: "", status: "active", budget: "any", sort: "newest" };

export function isFiltered(f: KeyFilters): boolean {
  return f.query.trim() !== "" || f.status !== defaultFilters.status || f.budget !== defaultFilters.budget;
}

export const PAGE_SIZE = 10;

function matchesBudget(k: ApiKey, f: BudgetFilter): boolean {
  const { state } = budgetState(k.spend_usd, k.monthly_budget_usd);
  switch (f) {
    case "any": return true;
    case "attention": return state === "close" || state === "over";
    case "over": return state === "over";
    case "none": return state === "none";
  }
}

const time = (iso: string | null) => (iso ? new Date(iso).getTime() : 0);

/** Applies the search, the status and budget filters, then the sort. Every word typed must appear in the name or the key prefix. */
export function filterKeys(keys: ApiKey[], f: KeyFilters): ApiKey[] {
  const words = f.query.toLowerCase().split(/\s+/).filter(Boolean);
  const out = keys.filter((k) => {
    if (f.status === "active" && k.revoked_at) return false;
    if (f.status === "revoked" && !k.revoked_at) return false;
    if (!matchesBudget(k, f.budget)) return false;
    const hay = `${k.name} ${k.prefix}`.toLowerCase();
    return words.every((w) => hay.includes(w));
  });
  const byName = (a: ApiKey, b: ApiKey) => a.name.localeCompare(b.name, "en", { sensitivity: "base" });
  const sorters: Record<SortKey, (a: ApiKey, b: ApiKey) => number> = {
    newest: (a, b) => time(b.created_at) - time(a.created_at),
    name: byName,
    spend: (a, b) => Number(b.spend_usd) - Number(a.spend_usd),
    recent: (a, b) => time(b.last_used_at) - time(a.last_used_at),
  };
  return out.sort((a, b) => sorters[f.sort](a, b) || byName(a, b));
}

export function countByStatus(keys: ApiKey[]): Record<StatusFilter, number> {
  const revoked = keys.filter((k) => k.revoked_at).length;
  return { active: keys.length - revoked, revoked, all: keys.length };
}

export function paginate<T>(items: T[], page: number, size = PAGE_SIZE) {
  const pages = Math.max(1, Math.ceil(items.length / size));
  const p = Math.min(Math.max(1, page), pages);
  const start = (p - 1) * size;
  return { items: items.slice(start, start + size), page: p, pages, from: items.length ? start + 1 : 0, to: Math.min(start + size, items.length), total: items.length };
}
