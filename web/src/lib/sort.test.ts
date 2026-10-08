import { describe, expect, it } from "vitest";
import { COLUMNS, defaultDir, filterModels, sortModels, type ModelSort } from "./sort";
import type { UsageGroup } from "./usage";

const m = (label: string, o: Partial<UsageGroup> = {}): UsageGroup => ({
  key: label, label, requests: 100, input_tokens: 1000, output_tokens: 100, cost_usd: "1.000000", saved_usd: "0.000000", cache_hits: 0, errors: 0,
  timed_requests: 10, p50_ms: 100, p95_ms: 200, ...o,
});

const models = [
  m("sonnet", { requests: 300, input_tokens: 9000, output_tokens: 400, cost_usd: "42.500000", cache_hits: 30, errors: 3, p50_ms: 1900, p95_ms: 5200 }),
  m("mini", { requests: 500, input_tokens: 4000, output_tokens: 900, cost_usd: "3.250000", cache_hits: 250, errors: 25, p50_ms: 800, p95_ms: 2100 }),
  m("local", { requests: 100, input_tokens: 2000, output_tokens: 200, cost_usd: "0.000000", cache_hits: 10, errors: 0, p50_ms: 1400, p95_ms: 3600 }),
  m("flash", { requests: 200, input_tokens: 500, output_tokens: 50, cost_usd: "0.000600", cache_hits: 0, errors: 40, timed_requests: 0, p50_ms: 0, p95_ms: 0 }),
];
const order = (key: ModelSort, dir: "asc" | "desc") => sortModels(models, key, dir).map((x) => x.label);

describe("sortModels", () => {
  it.each<[ModelSort, "asc" | "desc", string[]]>([
    ["model", "asc", ["flash", "local", "mini", "sonnet"]],
    ["model", "desc", ["sonnet", "mini", "local", "flash"]],
    ["requests", "desc", ["mini", "sonnet", "flash", "local"]],
    ["requests", "asc", ["local", "flash", "sonnet", "mini"]],
    ["input", "desc", ["sonnet", "mini", "local", "flash"]],
    ["output", "desc", ["mini", "sonnet", "local", "flash"]],
    ["cost", "desc", ["sonnet", "mini", "flash", "local"]],
    ["cost", "asc", ["local", "flash", "mini", "sonnet"]],
    ["cacheHit", "desc", ["mini", "local", "sonnet", "flash"]], // 50%, 10%, 10%, 0%: ties by name
    ["errors", "desc", ["flash", "mini", "sonnet", "local"]], // 20%, 5%, 1%, 0%
  ])("%s %s", (key, dir, want) => expect(order(key, dir)).toEqual(want));

  it("puts a model with no latency last, whichever way the column goes", () => {
    expect(order("p50", "asc")).toEqual(["mini", "local", "sonnet", "flash"]);
    expect(order("p50", "desc")).toEqual(["sonnet", "local", "mini", "flash"]);
    expect(order("p95", "asc").at(-1)).toBe("flash");
    expect(order("p95", "desc").at(-1)).toBe("flash");
  });

  it("is stable: equal values keep a fixed order by name", () => {
    const same = [m("b"), m("a"), m("c")];
    expect(sortModels(same, "requests", "desc").map((x) => x.label)).toEqual(["a", "b", "c"]);
    expect(sortModels(same, "requests", "asc").map((x) => x.label)).toEqual(["a", "b", "c"]);
  });

  it("does not change the list it is given", () => {
    const before = models.map((x) => x.label);
    sortModels(models, "requests", "asc");
    expect(models.map((x) => x.label)).toEqual(before);
  });

  it("sorts names without regard to case", () => {
    expect(sortModels([m("Zed"), m("alpha"), m("Beta")], "model", "asc").map((x) => x.label)).toEqual(["alpha", "Beta", "Zed"]);
  });

  it("has a sort for every column and a sensible first direction", () => {
    expect(COLUMNS.map((c) => c.key)).toEqual(["model", "requests", "input", "output", "cost", "p50", "p95", "cacheHit", "errors"]);
    expect(defaultDir("model")).toBe("asc");
    for (const c of COLUMNS.filter((c) => c.key !== "model")) expect(defaultDir(c.key)).toBe("desc");
  });
});

describe("filterModels", () => {
  it("keeps models containing every word, any case", () => {
    expect(filterModels(models, "SON").map((x) => x.label)).toEqual(["sonnet"]);
    expect(filterModels(models, "l").map((x) => x.label)).toEqual(["local", "flash"].filter((n) => models.some((x) => x.label === n)).sort((a, b) => models.findIndex((x) => x.label === a) - models.findIndex((x) => x.label === b)));
    expect(filterModels(models, "mini sonnet")).toEqual([]);
    expect(filterModels(models, "  ")).toBe(models);
  });
});
