import { describe, expect, it } from "vitest";
import {
  breakerPill, buildBars, chartSeries, compact, count, dollarsOf, microsOf, ms, niceCeil, parsePreset, percent, rangeFor, shares, sumUsd, tickLabel,
  type UsageGroup,
} from "./usage";

const g = (over: Partial<UsageGroup>): UsageGroup => ({
  key: "x", label: "x", requests: 0, input_tokens: 0, output_tokens: 0, cost_usd: "0.000000", saved_usd: "0.000000", cache_hits: 0, errors: 0,
  timed_requests: 0, p50_ms: null, p95_ms: null, ...over,
});

describe("rangeFor", () => {
  const now = new Date("2026-10-08T23:30:00Z");
  it("covers inclusive UTC days ending today", () => {
    expect(rangeFor("7d", now)).toEqual({ from: "2026-10-02", to: "2026-10-08" });
    expect(rangeFor("14d", now)).toEqual({ from: "2026-09-25", to: "2026-10-08" });
    expect(rangeFor("month", now)).toEqual({ from: "2026-10-01", to: "2026-10-08" });
  });
  it("uses the UTC date, not the local one", () => {
    expect(rangeFor("7d", new Date("2026-10-08T23:30:00-04:00")).to).toBe("2026-10-09");
  });
  it("handles the first of a month and a year boundary", () => {
    expect(rangeFor("month", new Date("2026-03-01T00:00:00Z"))).toEqual({ from: "2026-03-01", to: "2026-03-01" });
    expect(rangeFor("7d", new Date("2026-01-03T10:00:00Z"))).toEqual({ from: "2025-12-28", to: "2026-01-03" });
  });
  it("falls back to 14 days for anything unknown", () => {
    expect(parsePreset(undefined)).toBe("14d");
    expect(parsePreset("year")).toBe("14d");
    expect(parsePreset("7d")).toBe("7d");
  });
});

describe("exact dollars", () => {
  it("round-trips and sums without float error", () => {
    expect(microsOf("42.100000")).toBe(42_100_000n);
    expect(microsOf("0.000001")).toBe(1n);
    expect(dollarsOf(1n)).toBe("0.000001");
    expect(dollarsOf(microsOf("1234.567890"))).toBe("1234.567890");
    // 0.1 + 0.2 is the classic float trap
    expect(sumUsd(["0.100000", "0.200000"])).toBe("0.300000");
    expect(sumUsd(Array(10).fill("0.100000"))).toBe("1.000000");
    expect(sumUsd([])).toBe("0.000000");
  });
});

describe("number formatting", () => {
  it.each([[0, "0"], [999, "999"], [3440, "3,440"], [48210, "48.2K"], [61_300_000, "61.3M"], [47_900_000, "47.9M"], [1_200_000_000, "1.2B"], [250_000_000, "250M"]])(
    "compact(%d) = %s", (n, want) => expect(compact(n)).toBe(want));
  it("count adds separators", () => expect(count(48210)).toBe("48,210"));
  it("percent guards division by zero", () => {
    expect(percent(1, 0)).toBe("0%");
    expect(percent(21, 100)).toBe("21%");
    expect(percent(4, 1000, 1)).toBe("0.4%");
  });
  it.each([[null, "—"], [0.4, "<1 ms"], [3, "3 ms"], [29.6, "30 ms"], [1900, "1.9s"], [5200, "5.2s"], [12_000, "12s"]])("ms(%s) = %s", (v, want) => expect(ms(v)).toBe(want));
});

describe("shares", () => {
  it("always adds up to exactly 100", () => {
    for (const v of [[62, 25, 13], [1, 1, 1], [1, 1], [33, 33, 34], [5, 3, 2, 1], [1, 0, 0]]) {
      expect(shares(v).reduce((a, b) => a + b, 0)).toBe(100);
    }
    expect(shares([62, 25, 13])).toEqual([62, 25, 13]);
    expect(shares([1, 1, 1])).toEqual([34, 33, 33]);
  });
  it("is all zeros for nothing", () => expect(shares([0, 0, 0])).toEqual([0, 0, 0]));
});

describe("niceCeil and ticks", () => {
  it.each([[0, 1], [0.0045, 0.005], [0.7, 1], [1.2, 2], [2.1, 2.5], [3.4, 5], [11.8, 20], [19.9, 20], [20, 20], [21, 25], [86, 100]])(
    "niceCeil(%s) = %s", (v, want) => expect(niceCeil(v)).toBeCloseTo(want, 9));
  it("labels axes at the right precision", () => {
    expect(tickLabel(20, 20)).toBe("$20");
    expect(tickLabel(1.25, 5)).toBe("$1");
    expect(tickLabel(0.5, 2)).toBe("$0.50");
    expect(tickLabel(0.0025, 0.01)).toBe("$0.0025");
    expect(tickLabel(0, 0.01)).toBe("$0");
  });
});

describe("chart", () => {
  const byModel = [
    g({ key: "sonnet", label: "sonnet", cost_usd: "142.180000", requests: 10 }),
    g({ key: "mini", label: "mini", cost_usd: "44.240000", requests: 10 }),
    g({ key: "local", label: "local", cost_usd: "0.000000", requests: 5 }),
  ];
  const days = [
    g({ key: "2026-09-30", requests: 3, cost_usd: "9.500000", models: { sonnet: "7.100000", mini: "2.100000" } }),
    g({ key: "2026-10-01", requests: 0, cost_usd: "0.000000", models: {} }),
  ];

  it("gives the two costliest models their own colours and names free models without a bar", () => {
    const { series, free, colorOf } = chartSeries(days, byModel);
    expect(series.map((s) => [s.model, s.color])).toEqual([["sonnet", "var(--s1)"], ["mini", "var(--s2)"]]);
    expect(free).toEqual(["local"]);
    expect(colorOf("sonnet")).toBe("var(--s1)");
  });

  it("groups further paid models as other", () => {
    const many = [...byModel, g({ key: "flash", label: "flash", cost_usd: "3.000000", requests: 2 }), g({ key: "haiku", label: "haiku", cost_usd: "1.000000", requests: 2 })];
    const { series } = chartSeries(days, many);
    expect(series.map((s) => s.label)).toEqual(["sonnet", "mini", "other models"]);
    const { bars } = buildBars([g({ key: "2026-10-01", cost_usd: "4.000000", models: { flash: "3.000000", haiku: "1.000000" } })], series);
    expect(bars[0].segments).toHaveLength(1);
    expect(bars[0].segments[0]).toMatchObject({ model: "other", cost: "4.000000" });
  });

  it("scales to a round axis and stacks segments to the day's total", () => {
    const { series } = chartSeries(days, byModel);
    const { bars, top, ticks } = buildBars(days, series);
    expect(top).toBe(10);
    expect(ticks).toHaveLength(5); // top first
    expect(ticks[0]).toBe("$10");
    expect(ticks[4]).toBe("$0");
    const sum = bars[0].segments.reduce((a, s) => a + s.height, 0);
    expect(sum).toBeLessThanOrEqual(bars[0].height + 1e-9);
    expect(bars[0].height).toBeCloseTo(95, 5);
    expect(bars[0].tip).toContain("Sep 30: $9.50 spent");
    expect(bars[0].tip).toContain("sonnet $7.10");
    expect(bars[1].segments).toEqual([]);
    expect(bars[1].label).toBe("Oct 1"); // the first of a month is labelled with its month
    expect(bars[0].label).toBe("30");
  });

  it("draws a model that appears on a day but not in the by-model list, as other", () => {
    const { series } = chartSeries([g({ key: "2026-10-01", cost_usd: "1.000000", models: { ghost: "1.000000" } })], byModel);
    expect(series.map((s) => s.model)).toContain("other");
  });

  it("table totals equal the bar totals", () => {
    const { series } = chartSeries(days, byModel);
    const { bars } = buildBars(days, series);
    const chartTotal = sumUsd(bars.map((b) => b.total));
    expect(chartTotal).toBe(sumUsd(days.map((d) => d.cost_usd)));
  });

  it("copes with a range that spent nothing", () => {
    const quiet = [g({ key: "2026-10-01" }), g({ key: "2026-10-02" })];
    const { bars, top, ticks } = buildBars(quiet, []);
    expect(top).toBe(1);
    expect(bars.every((b) => b.height === 0 && b.segments.length === 0)).toBe(true);
    expect(ticks[4]).toBe("$0");
  });
});

describe("breakerPill", () => {
  it("is a shape plus a word for every state", () => {
    expect(breakerPill({ state: "closed", open_remaining_seconds: null })).toMatchObject({ label: "Breaker closed", tone: "ok" });
    expect(breakerPill({ state: "half_open", open_remaining_seconds: null })).toMatchObject({ label: "Half open, probing", tone: "wait" });
    expect(breakerPill({ state: "open", open_remaining_seconds: 21 })).toMatchObject({ label: "Breaker open for 21s", tone: "fail" });
    expect(breakerPill({ state: "not_configured", open_remaining_seconds: null })).toMatchObject({ label: "Not configured", tone: "mute" });
    for (const s of ["closed", "half_open", "open", "not_configured"] as const) expect(breakerPill({ state: s, open_remaining_seconds: null }).icon).toMatch(/^M/);
  });
});
