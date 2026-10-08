import { describe, expect, it } from "vitest";
import { breakerPill, compact, keyChoices, count, dollarsOf, microsOf, modelColors, ms, parseCustom, parsePreset, percent, rangeFor, shares, sumUsd, type UsageGroup } from "./usage";

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
  it("covers the longer ranges", () => {
    expect(rangeFor("30d", now)).toEqual({ from: "2026-09-09", to: "2026-10-08" });
    expect(rangeFor("90d", now)).toEqual({ from: "2026-07-11", to: "2026-10-08" });
    expect(rangeFor("180d", now)).toEqual({ from: "2026-04-12", to: "2026-10-08" });
    expect(rangeFor("365d", now)).toEqual({ from: "2025-10-09", to: "2026-10-08" });
  });
  it("last month is the whole previous calendar month, whatever day it is", () => {
    expect(rangeFor("last-month", now)).toEqual({ from: "2026-09-01", to: "2026-09-30" });
    expect(rangeFor("last-month", new Date("2026-03-31T10:00:00Z"))).toEqual({ from: "2026-02-01", to: "2026-02-28" });
    expect(rangeFor("last-month", new Date("2028-03-05T10:00:00Z"))).toEqual({ from: "2028-02-01", to: "2028-02-29" });
    expect(rangeFor("last-month", new Date("2026-01-15T10:00:00Z"))).toEqual({ from: "2025-12-01", to: "2025-12-31" });
  });
  it("no preset is longer than the 366 days the API allows", () => {
    for (const p of ["7d", "14d", "30d", "90d", "month", "last-month", "180d", "365d"] as const) {
      const r = rangeFor(p, now);
      expect((Date.parse(r.to) - Date.parse(r.from)) / 86_400_000 + 1).toBeLessThanOrEqual(366);
    }
  });
  it("falls back to 14 days for anything unknown", () => {
    expect(parsePreset(undefined)).toBe("14d");
    expect(parsePreset("year")).toBe("14d");
    expect(parsePreset("7d")).toBe("7d");
    expect(parsePreset("custom")).toBe("custom");
    expect(parsePreset("last-month")).toBe("last-month");
  });
});

describe("parseCustom", () => {
  const now = new Date("2026-10-08T12:00:00Z");
  it("accepts a good range, including one day", () => {
    expect(parseCustom("2026-09-01", "2026-09-30", now)).toEqual({ from: "2026-09-01", to: "2026-09-30" });
    expect(parseCustom("2026-10-08", "2026-10-08", now)).toEqual({ from: "2026-10-08", to: "2026-10-08" });
  });
  it.each([
    ["missing", undefined, "2026-09-30", /first and a last/],
    ["not a date", "yesterday", "2026-09-30", /first and a last/],
    ["impossible date", "2026-02-30", "2026-03-05", /first and a last/],
    ["backwards", "2026-09-30", "2026-09-01", /after the last/],
    ["in the future", "2026-10-01", "2026-10-09", /future/],
    ["too long", "2025-09-01", "2026-09-30", /at most 366/],
  ])("rejects %s", (_n, from, to, msg) => {
    const r = parseCustom(from, to, now);
    expect("error" in r && r.error).toMatch(msg);
  });
  it("allows exactly 366 days", () => {
    expect(parseCustom("2025-10-08", "2026-10-08", now)).toEqual({ from: "2025-10-08", to: "2026-10-08" });
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

describe("modelColors", () => {
  it("gives the two costliest models their own colour and everything else grey", () => {
    const c = modelColors([g({ key: "a", cost_usd: "1.000000" }), g({ key: "b", cost_usd: "5.000000" }), g({ key: "c", cost_usd: "3.000000" }), g({ key: "free", cost_usd: "0.000000" })]);
    expect([c("b"), c("c"), c("a"), c("free"), c("unknown")]).toEqual(["var(--s1)", "var(--s2)", "var(--s3)", "var(--s3)", "var(--s3)"]);
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

describe("keyChoices", () => {
  const k = (id: string, name: string, used: string | null, o: Partial<{ revoked_at: string | null; builtin: boolean }> = {}) => ({ id, name, last_used_at: used, revoked_at: null, builtin: false, ...o });
  it("offers used keys, most recent first, and hides the rest", () => {
    const out = keyChoices([k("a", "old", "2026-10-01T00:00:00Z"), k("b", "never", null), k("c", "new", "2026-10-08T00:00:00Z"), k("d", "pg", "2026-10-09T00:00:00Z", { builtin: true })], "");
    expect(out.map((x) => x.id)).toEqual(["c", "a"]);
  });
  it("marks revoked keys, and caps the list", () => {
    const many = Array.from({ length: 100 }, (_, i) => k(`k${i}`, `key-${i}`, `2026-10-${String((i % 28) + 1).padStart(2, "0")}T00:00:00Z`));
    expect(keyChoices(many, "")).toHaveLength(40);
    expect(keyChoices([k("r", "gone", "2026-10-01T00:00:00Z", { revoked_at: "2026-10-02T00:00:00Z" })], "")[0].name).toBe("gone (revoked)");
  });
  it("always includes the selected key, even if it is unused or past the cap", () => {
    const many = Array.from({ length: 100 }, (_, i) => k(`k${i}`, `key-${i}`, "2026-10-08T00:00:00Z"));
    many.push(k("sel", "selected", null));
    expect(keyChoices(many, "sel").map((x) => x.id)).toContain("sel");
  });
});
