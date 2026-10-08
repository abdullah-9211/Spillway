import { describe, expect, it } from "vitest";
import { buildBars, defaultGran, formatValue, niceCeil, planSeries, tickLabel, toBuckets, valueOf, weekStart, zoomRange } from "./chart";
import { sumUsd, type UsageGroup } from "./usage";

type Series = NonNullable<UsageGroup["series"]>;
const st = (label: string, requests: number, cost: string, o: Partial<Series[string]> = {}): Series[string] => ({
  label, requests, input_tokens: requests * 100, output_tokens: requests * 10, cost_usd: cost, saved_usd: "0.000000", ...o,
});
const day = (key: string, series: Series, o: Partial<UsageGroup> = {}): UsageGroup => {
  const vals = Object.values(series);
  return {
    key, label: key, requests: vals.reduce((a, v) => a + v.requests, 0), input_tokens: vals.reduce((a, v) => a + v.input_tokens, 0),
    output_tokens: vals.reduce((a, v) => a + v.output_tokens, 0), cost_usd: sumUsd(vals.map((v) => v.cost_usd)), saved_usd: sumUsd(vals.map((v) => v.saved_usd)),
    cache_hits: 0, errors: 0, timed_requests: 0, p50_ms: null, p95_ms: null, series, ...o,
  };
};

// Wed 30 Sep to Sat 10 Oct 2026, with a quiet 2 Oct: it spans two Monday-based weeks, the first one partial.
const days: UsageGroup[] = [
  day("2026-09-30", { sonnet: st("sonnet", 10, "7.100000"), mini: st("mini", 20, "2.100000"), local: st("local", 5, "0.000000") }),
  day("2026-10-01", { sonnet: st("sonnet", 12, "8.400000"), mini: st("mini", 10, "1.000000"), flash: st("flash", 3, "0.500000") }),
  day("2026-10-02", {}),
  ...["03", "04", "05", "06", "07", "08", "09"].map((d) => day(`2026-10-${d}`, { sonnet: st("sonnet", 1, "1.000000") })),
  day("2026-10-10", { sonnet: st("sonnet", 2, "2.000000") }),
];

describe("weekStart", () => {
  it.each([["2026-10-05", "2026-10-05"], ["2026-10-04", "2026-09-28"], ["2026-10-08", "2026-10-05"], ["2026-01-01", "2025-12-29"], ["2024-03-01", "2024-02-26"]])(
    "%s is in the week starting %s", (d, want) => expect(weekStart(d)).toBe(want));
});

describe("toBuckets", () => {
  it("is one bucket per day, labelled with the day number and the month on the first", () => {
    const b = toBuckets(days, "day");
    expect(b).toHaveLength(days.length);
    expect(b[0].label).toBe("30");
    expect(b[1].label).toBe("Oct 1");
    expect(b[0].title).toBe("Sep 30");
  });

  it("groups into Monday weeks and names partial ones honestly", () => {
    const w = toBuckets(days, "week");
    expect(w.map((x) => [x.start, x.end, x.days])).toEqual([["2026-09-30", "2026-10-04", 5], ["2026-10-05", "2026-10-10", 6]]);
    expect(w[0].title).toBe("Sep 30 to Oct 4");
    expect(w[1].title).toBe("Oct 5 to Oct 10");
    const full = toBuckets([...Array(7)].map((_, i) => day(`2026-10-${String(5 + i).padStart(2, "0")}`, {})), "week");
    expect(full[0].title).toBe("Week of Oct 5");
  });

  it("adds up exactly: weekly totals equal daily totals, for every figure", () => {
    const d = toBuckets(days, "day");
    const w = toBuckets(days, "week");
    const sum = (bs: typeof d, f: (b: (typeof d)[number]) => number) => bs.reduce((a, b) => a + f(b), 0);
    expect(sum(w, (b) => b.requests)).toBe(sum(d, (b) => b.requests));
    expect(sum(w, (b) => b.input + b.output)).toBe(sum(d, (b) => b.input + b.output));
    expect(sumUsd(w.map((b) => (Number(b.cost) / 1e6).toFixed(6)))).toBe(sumUsd(d.map((b) => (Number(b.cost) / 1e6).toFixed(6))));
    const sonnetW = w.reduce((a, b) => a + (b.cells.get("sonnet")?.cost ?? 0n), 0n);
    const sonnetD = d.reduce((a, b) => a + (b.cells.get("sonnet")?.cost ?? 0n), 0n);
    expect(sonnetW).toBe(sonnetD);
  });

  it("handles a day with no series and an empty list", () => {
    expect(toBuckets([day("2026-10-02", {})], "day")[0].cells.size).toBe(0);
    expect(toBuckets([], "week")).toEqual([]);
  });
});

describe("metrics", () => {
  const b = toBuckets(days, "day")[0];
  it("reads each metric from a bucket", () => {
    expect(valueOf(b, "spend")).toBeCloseTo(9.2, 9);
    expect(valueOf(b, "requests")).toBe(35);
    expect(valueOf(b, "tokens")).toBe(3500 + 350);
    expect(valueOf(b, "saved")).toBe(0);
  });
  it("formats each the way it is read", () => {
    expect(formatValue(9.2, "spend")).toBe("$9.20");
    expect(formatValue(0.0045, "spend")).toBe("$0.0045");
    expect(formatValue(0.000002, "saved")).toBe("$0.000002");
    expect(formatValue(48210, "requests")).toBe("48,210");
    expect(formatValue(61_300_000, "tokens")).toBe("61.3M");
  });
});

describe("planSeries", () => {
  const buckets = toBuckets(days, "day");

  it("colours the two biggest and groups the rest, per metric", () => {
    const spend = planSeries(buckets, "spend", "models");
    expect(spend.series.map((s) => [s.label, s.color])).toEqual([["sonnet", "var(--s1)"], ["mini", "var(--s2)"], ["other models", "var(--s3)"]]);
    expect(spend.series[2].ids).toEqual(["flash"]);
    expect(spend.idle).toEqual(["local"]); // used, but free: named, no bar
  });

  it("ranks differently when the metric changes", () => {
    // Sonnet cost the most and also has the most requests (31 against mini's 30). The free model, which has no bar
    // on the spend chart, is real traffic here and is drawn, inside "other models" with flash.
    const req = planSeries(buckets, "requests", "models");
    expect(req.series.map((s) => s.label)).toEqual(["sonnet", "mini", "other models"]);
    expect(req.series[2].ids.sort()).toEqual(["flash", "local"]);
    expect(req.idle).toEqual([]);
    const sav = planSeries(buckets, "saved", "models");
    expect(sav.series).toEqual([]);
  });

  it("uses the noun for the group", () => {
    expect(planSeries(buckets, "spend", "keys").series.at(-1)?.label).toBe("other keys");
  });
});

describe("niceCeil and ticks", () => {
  it.each([[0, 1], [0.0045, 0.005], [0.7, 1], [1.2, 2], [2.1, 2.5], [3.4, 5], [11.8, 20], [20, 20], [21, 25], [86, 100]])("niceCeil(%s) = %s", (v, want) =>
    expect(niceCeil(v)).toBeCloseTo(want, 9));
  it("labels axes at the right precision, per metric", () => {
    expect(tickLabel(20, 20)).toBe("$20");
    expect(tickLabel(0.5, 2)).toBe("$0.50");
    expect(tickLabel(0.0025, 0.01)).toBe("$0.0025");
    expect(tickLabel(0, 0.01)).toBe("$0");
    expect(tickLabel(0, 5, "requests")).toBe("0");
    expect(tickLabel(2500, 10000, "requests")).toBe("2,500");
    expect(tickLabel(5_000_000, 20_000_000, "tokens")).toBe("5M");
  });
});

describe("buildBars", () => {
  const buckets = toBuckets(days, "day");
  const { series } = planSeries(buckets, "spend", "models");

  it("stacks the series to the bucket's total and scales to a round axis", () => {
    const { bars, top, ticks } = buildBars(buckets, "spend", series, new Set());
    expect(top).toBe(10);
    expect(ticks[0]).toBe("$10");
    expect(ticks[4]).toBe("$0");
    expect(bars[0].segs.map((s) => s.label)).toEqual(["sonnet", "mini"]);
    expect(bars[0].total).toBeCloseTo(9.2, 9);
    expect(bars[0].segs.reduce((a, s) => a + s.height, 0)).toBeCloseTo(bars[0].height, 9);
    expect(bars[0].tip).toBe("Sep 30: $9.20 spent (sonnet $7.10, mini $2.10)");
    expect(bars[1].segs.map((s) => s.label)).toEqual(["sonnet", "mini", "other models"]);
    expect(bars[2].segs).toEqual([]);
  });

  it("rescales the axis when a series is hidden", () => {
    const all = buildBars(buckets, "spend", series, new Set());
    const noSonnet = buildBars(buckets, "spend", series, new Set(["sonnet"]));
    expect(noSonnet.top).toBeLessThan(all.top);
    expect(noSonnet.bars[0].segs.map((s) => s.label)).toEqual(["mini"]);
    expect(noSonnet.bars[0].total).toBeCloseTo(2.1, 9);
    const none = buildBars(buckets, "spend", series, new Set(series.map((s) => s.id)));
    expect(none.bars.every((b) => b.total === 0)).toBe(true);
    expect(none.top).toBe(1);
  });

  it("keeps request and token axes in whole numbers, even for tiny counts", () => {
    const tiny = toBuckets([day("2026-10-01", { a: st("a", 1, "0.000000") })], "day");
    const { series: s } = planSeries(tiny, "requests", "models");
    const { top, ticks } = buildBars(tiny, "requests", s, new Set());
    expect(top).toBe(4);
    expect(ticks).toEqual(["4", "3", "2", "1", "0"]);
  });

  it("plots requests and tokens in their own units", () => {
    const { series: rs } = planSeries(buckets, "requests", "models");
    const req = buildBars(buckets, "requests", rs, new Set());
    expect(req.bars[0].total).toBe(35);
    expect(req.bars[0].tip).toMatch(/^Sep 30: 35 requests/);
    const { series: ts } = planSeries(buckets, "tokens", "models");
    expect(buildBars(buckets, "tokens", ts, new Set()).bars[0].total).toBe(3850);
  });

  it("weekly bars sum the days", () => {
    const wk = toBuckets(days, "week");
    const { series: s } = planSeries(wk, "spend", "models");
    const { bars } = buildBars(wk, "spend", s, new Set());
    expect(bars[0].total).toBeCloseTo(9.2 + 9.9 + 1 + 1, 9); // Sep 30, Oct 1 and the 3rd and 4th
  });
});

describe("helpers", () => {
  it("zooms to the days a bar covers", () => {
    const w = toBuckets(days, "week")[1];
    expect(zoomRange(w)).toEqual({ from: "2026-10-05", to: "2026-10-10" });
  });
  it("starts with weeks only for long ranges", () => {
    expect(defaultGran(14)).toBe("day");
    expect(defaultGran(45)).toBe("day");
    expect(defaultGran(46)).toBe("week");
    expect(defaultGran(365)).toBe("week");
  });
});
