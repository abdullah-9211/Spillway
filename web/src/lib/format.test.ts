import { describe, expect, it } from "vitest";
import {
  budgetState, countByStatus, defaultFilters, emptyForm, filterKeys, formFromKey, isFiltered, keyPayload, paginate,
  relativeTime, rpmLabel, usd, validateKeyForm, type ApiKey, type KeyFormValues,
} from "./format";

describe("usd", () => {
  it.each([
    ["42.100000", "$42.10"],
    ["0.000000", "$0.00"],
    ["1000000.000000", "$1,000,000.00"],
    ["0.004500", "$0.0045"],
    ["0.009999", "$0.0100"],
    ["0.010000", "$0.01"],
  ])("%s -> %s", (input, want) => expect(usd(input)).toBe(want));

  it("handles missing and unreadable values", () => {
    expect(usd(null)).toBe("");
    expect(usd("n/a")).toBe("n/a");
  });
});

describe("rpmLabel", () => {
  it("says no limit for null", () => {
    expect(rpmLabel(null)).toBe("No limit");
    expect(rpmLabel(120)).toBe("120 a min");
    expect(rpmLabel(1500)).toBe("1,500 a min");
  });
});

describe("budgetState", () => {
  it.each([
    ["10", null, "none"],
    ["0", "100", "normal"],
    ["79.99", "100", "normal"],
    ["80", "100", "close"],
    ["88.40", "100", "close"],
    ["99.999999", "100", "close"],
    ["100", "100", "over"], // spending exactly the budget is spent
    ["150", "100", "over"],
    ["0", "0", "over"], // a $0 budget refuses everything
  ])("spend %s of %s is %s", (spend, budget, want) => expect(budgetState(spend, budget).state).toBe(want));
});

describe("relativeTime", () => {
  const now = new Date("2026-10-08T12:00:00Z");
  it.each([
    [null, "Never"],
    ["2026-10-08T11:59:30Z", "just now"],
    ["2026-10-08T11:58:00Z", "2 min ago"],
    ["2026-10-08T11:19:00Z", "41 min ago"],
    ["2026-10-08T10:00:00Z", "2 h ago"],
    ["2026-09-27T10:00:00Z", "Sep 27"],
  ])("%s -> %s", (iso, want) => expect(relativeTime(iso, now)).toBe(want));
});

describe("validateKeyForm", () => {
  const ok: KeyFormValues = { name: "ci-pipeline", rpm: "60", budget: "30.50", semanticCache: false, cacheNonzeroTemp: false };

  it("accepts a good form, and one with no limits", () => {
    expect(validateKeyForm(ok)).toEqual({});
    expect(validateKeyForm({ ...ok, rpm: "", budget: "" })).toEqual({});
  });

  it.each([
    ["empty name", { name: "  " }, "name"],
    ["long name", { name: "x".repeat(65) }, "name"],
    ["reserved name", { name: "Playground" }, "name"],
    ["zero rpm", { rpm: "0" }, "rpm"],
    ["decimal rpm", { rpm: "1.5" }, "rpm"],
    ["text rpm", { rpm: "fast" }, "rpm"],
    ["huge rpm", { rpm: "1000001" }, "rpm"],
    ["negative budget", { budget: "-5" }, "budget"],
    ["text budget", { budget: "lots" }, "budget"],
    ["seven decimals", { budget: "1.1234567" }, "budget"],
    ["huge budget", { budget: "1000001" }, "budget"],
    ["exponent", { budget: "1e3" }, "budget"],
  ])("rejects %s", (_n, patch, field) => {
    expect(validateKeyForm({ ...ok, ...patch })).toHaveProperty(field);
  });

  it("allows a $0 budget, which blocks the key", () => {
    expect(validateKeyForm({ ...ok, budget: "0" })).toEqual({});
  });
});

describe("keyPayload and formFromKey", () => {
  it("turns empty limits into null and trims the name", () => {
    expect(keyPayload({ ...emptyForm, name: "  a  " })).toEqual({ name: "a", rate_limit_rpm: null, monthly_budget_usd: null, semantic_cache: false, cache_nonzero_temp: false });
    expect(keyPayload({ name: "a", rpm: "60", budget: "30.50", semanticCache: true, cacheNonzeroTemp: true })).toEqual({
      name: "a", rate_limit_rpm: 60, monthly_budget_usd: "30.50", semantic_cache: true, cache_nonzero_temp: true,
    });
  });

  it("round-trips a key through the form", () => {
    const k = { name: "n", rate_limit_rpm: 60, monthly_budget_usd: "30.000000", semantic_cache: true, cache_nonzero_temp: false } as ApiKey;
    expect(formFromKey(k)).toEqual({ name: "n", rpm: "60", budget: "30", semanticCache: true, cacheNonzeroTemp: false });
    expect(formFromKey({ ...k, rate_limit_rpm: null, monthly_budget_usd: null })).toMatchObject({ rpm: "", budget: "" });
    expect(formFromKey({ ...k, monthly_budget_usd: "12.340000" }).budget).toBe("12.34");
    expect(formFromKey({ ...k, monthly_budget_usd: "0.000000" }).budget).toBe("0");
  });
});

describe("filterKeys", () => {
  const k = (over: Partial<ApiKey>): ApiKey => ({
    id: over.name ?? "x", name: "n", prefix: "spw_aaaa", rate_limit_rpm: null, monthly_budget_usd: null, spend_usd: "0", semantic_cache: false,
    cache_nonzero_temp: false, created_at: "2026-10-01T00:00:00Z", revoked_at: null, last_used_at: null, builtin: false, ...over,
  });
  const list = [
    k({ name: "Support Agent", prefix: "spw_7Hq2", spend_usd: "42", monthly_budget_usd: "100", created_at: "2026-10-03T00:00:00Z", last_used_at: "2026-10-08T11:00:00Z" }),
    k({ name: "research-bot", prefix: "spw_Lm9x", spend_usd: "88", monthly_budget_usd: "100", created_at: "2026-10-05T00:00:00Z", last_used_at: "2026-10-08T12:00:00Z" }),
    k({ name: "data-sync", prefix: "spw_Zc41", spend_usd: "50", monthly_budget_usd: "50", created_at: "2026-10-02T00:00:00Z" }),
    k({ name: "old-demo", prefix: "spw_Hn6e", revoked_at: "2026-09-28T00:00:00Z", created_at: "2026-09-01T00:00:00Z" }),
  ];
  const names = (f: Partial<typeof defaultFilters>) => filterKeys(list, { ...defaultFilters, ...f }).map((x) => x.name);

  it("hides revoked keys by default, newest first", () => {
    expect(names({})).toEqual(["research-bot", "Support Agent", "data-sync"]);
  });

  it("matches the name or the prefix, any case, every word", () => {
    expect(names({ query: "SUPPORT" })).toEqual(["Support Agent"]);
    expect(names({ query: "lm9x" })).toEqual(["research-bot"]);
    expect(names({ query: "support agent" })).toEqual(["Support Agent"]);
    expect(names({ query: "support bot" })).toEqual([]);
    expect(names({ query: "  " })).toHaveLength(3);
  });

  it("filters by status", () => {
    expect(names({ status: "revoked" })).toEqual(["old-demo"]);
    expect(names({ status: "all", sort: "name" })).toEqual(["data-sync", "old-demo", "research-bot", "Support Agent"]);
  });

  it("filters by budget state", () => {
    expect(names({ budget: "attention", sort: "name" })).toEqual(["data-sync", "research-bot"]);
    expect(names({ budget: "over" })).toEqual(["data-sync"]);
    expect(names({ budget: "none", status: "all" })).toEqual(["old-demo"]);
  });

  it("sorts by spend and by last use, with never-used keys last", () => {
    expect(names({ sort: "spend" })).toEqual(["research-bot", "data-sync", "Support Agent"]);
    expect(names({ sort: "recent" })).toEqual(["research-bot", "Support Agent", "data-sync"]);
  });

  it("combines filters", () => {
    expect(names({ query: "a", budget: "attention", status: "all", sort: "name" })).toEqual(["data-sync", "research-bot"]);
  });

  it("knows when a filter is on, and counts by status", () => {
    expect(isFiltered(defaultFilters)).toBe(false);
    expect(isFiltered({ ...defaultFilters, query: "x" })).toBe(true);
    expect(isFiltered({ ...defaultFilters, status: "all" })).toBe(true);
    expect(isFiltered({ ...defaultFilters, sort: "name" })).toBe(false); // a sort order is not a filter
    expect(countByStatus(list)).toEqual({ active: 3, revoked: 1, all: 4 });
  });
});

describe("paginate", () => {
  const items = Array.from({ length: 23 }, (_, i) => i);
  it("slices pages and reports the range", () => {
    expect(paginate(items, 1)).toMatchObject({ page: 1, pages: 3, from: 1, to: 10, total: 23 });
    expect(paginate(items, 3)).toMatchObject({ page: 3, from: 21, to: 23, items: [20, 21, 22] });
  });
  it("clamps out-of-range pages and handles empty lists", () => {
    expect(paginate(items, 99).page).toBe(3);
    expect(paginate(items, 0).page).toBe(1);
    expect(paginate([], 1)).toMatchObject({ page: 1, pages: 1, from: 0, to: 0, total: 0, items: [] });
  });
});
