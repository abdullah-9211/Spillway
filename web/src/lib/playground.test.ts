import { describe, expect, it } from "vitest";
import {
  answerFor, chipsFor, hopsFor, keepFaults, recentMeta, requestBody, segmentsFor, timingLabel, toggleFault, validate,
  emptyOptions, surprise, type Attempt, type PlaygroundResult,
} from "./playground";

const ok = (over: Partial<Attempt> = {}): Attempt => ({ provider: "openai", model: "mini", kind: "fallback", latency_ms: 1060, ...over });
const bad = (over: Partial<Attempt> = {}): Attempt => ({ provider: "anthropic", model: "sonnet", kind: "primary", latency_ms: 212, status: 429, error_kind: "rate_limited", injected: true, ...over });

describe("hopsFor", () => {
  it("starts with the request and shows a straight success as one answered stop", () => {
    const hops = hopsFor({ policy: "default", attempts: [ok({ kind: "primary" })] });
    expect(hops.map((h) => h.tone)).toEqual(["neutral", "ok"]);
    expect(hops[0].meta).toBe("policy default");
    expect(hops[1].meta).toBe("answered, 1.1s");
  });

  it("shows one failure then the fallback that answered, each with a shape path and a word", () => {
    const hops = hopsFor({ policy: "default", attempts: [bad(), ok()] });
    expect(hops.map((h) => [h.name, h.tone, h.meta])).toEqual([
      ["Request", "neutral", "policy default"],
      ["sonnet", "fail", "429, 212 ms"],
      ["mini, fallback", "ok", "answered, 1.1s"],
    ]);
    expect(hops[1].label).toContain("failed");
    expect(hops[1].label).toContain("made to fail on purpose");
    expect(hops[2].label).toContain("answered");
    expect(new Set(hops.map((h) => h.icon)).size).toBe(3);
  });

  it("shows two failures and a late answer, in order", () => {
    const hops = hopsFor({ policy: "default", attempts: [bad(), bad({ model: "mini", status: 503, latency_ms: 340 }), ok({ kind: "retry" })] });
    expect(hops.map((h) => h.tone)).toEqual(["neutral", "fail", "fail", "ok"]);
    expect(hops[3].name).toBe("mini, retry");
  });

  it("marks a cut stream as a failed last stop and a skipped provider as neutral", () => {
    const cut = hopsFor({ policy: "default", attempts: [bad({ status: undefined, error_kind: "server", latency_ms: 90 })] });
    expect(cut[1].tone).toBe("fail");
    const skipped = hopsFor({ policy: "default", attempts: [{ provider: "p", model: "m", kind: "skipped", latency_ms: 0, error: "breaker open", error_kind: "breaker_open" }] });
    expect(skipped[1].tone).toBe("neutral");
    expect(skipped[1].meta).toBe("breaker open");
  });
});

describe("timing", () => {
  it("gives each attempt its share of the time, failed ones in the failure tone", () => {
    const segs = segmentsFor([bad({ latency_ms: 250 }), ok({ latency_ms: 750 })]);
    expect(segs.map((s) => [s.tone, Math.round(s.share)])).toEqual([["fail", 25], ["ok", 75]]);
  });

  it("is empty when nothing was timed (a cache hit)", () => {
    expect(segmentsFor([])).toEqual([]);
    expect(timingLabel({ attempts: [], latency_ms: 4, overhead_ms: 4 }).aria).toBe("No provider was called");
  });

  it("describes the bar in words", () => {
    const t = timingLabel({ attempts: [bad({ latency_ms: 212 }), ok({ latency_ms: 1060 })], latency_ms: 1620, overhead_ms: 4 });
    expect(t.aria).toBe("Time spent: 212 ms failed, 1.1s answered");
    expect(t.total).toBe("1.6s in total");
    expect(t.overhead).toBe("4 ms added by Spillway");
  });
});

describe("answerFor", () => {
  it("tells apart an answer, a partial answer and no answer", () => {
    expect(answerFor({ answer: "hi", error: null, outcome: "ok" })).toEqual({ text: "hi", partial: false, failed: false });
    expect(answerFor({ answer: "hal", error: { code: "stream_interrupted", message: "cut" }, outcome: "upstream_error" })).toEqual({ text: "hal", partial: true, failed: true });
    expect(answerFor({ answer: "", error: { code: "all_providers_failed", message: "none" }, outcome: "all_providers_failed" })).toEqual({ text: "none", partial: false, failed: true });
  });
});

describe("recentMeta", () => {
  const base = { model: "mini", cache: "miss" as const, attempts: [] as Attempt[], outcome: "ok" };
  it("names the model, failures and cache hits", () => {
    expect(recentMeta(base)).toBe("mini");
    expect(recentMeta({ ...base, attempts: [bad(), bad()] })).toBe("mini, 2 failed attempts");
    expect(recentMeta({ ...base, attempts: [bad()] })).toBe("mini, 1 failed attempt");
    expect(recentMeta({ ...base, cache: "hit_exact" })).toBe("mini, cache hit");
    expect(recentMeta({ ...base, model: "" })).toBe("no answer");
  });
});

describe("fault chips", () => {
  it("offers 429 and 503 per provider on the route, and slow and cut for the first", () => {
    const chips = chipsFor(["anthropic", "openai"]);
    expect(chips.map((c) => c.label)).toEqual(["Anthropic returns 429", "Anthropic returns 503", "OpenAI returns 429", "OpenAI returns 503", "Slow response", "Cut the stream halfway"]);
    expect(chipsFor([])).toEqual([]);
  });

  it("toggles a fault on and off, and one provider has one fault at a time", () => {
    const a = { provider: "anthropic", kind: "rate_limit" } as const;
    const b = { provider: "anthropic", kind: "slow" } as const;
    const o = { provider: "openai", kind: "server_error" } as const;
    expect(toggleFault([], a)).toEqual([a]);
    expect(toggleFault([a], a)).toEqual([]);
    expect(toggleFault([a, o], b)).toEqual([o, b]);
  });

  it("drops faults whose provider left the route", () => {
    expect(keepFaults([{ provider: "anthropic", kind: "slow" }, { provider: "openai", kind: "slow" }], ["openai"])).toEqual([{ provider: "openai", kind: "slow" }]);
  });
});

describe("validate and requestBody", () => {
  it("needs a prompt and sane options", () => {
    expect(validate("  ", emptyOptions).prompt).toBeTruthy();
    expect(validate("hi", { ...emptyOptions, temperature: "3" }).temperature).toBeTruthy();
    expect(validate("hi", { ...emptyOptions, temperature: "abc" }).temperature).toBeTruthy();
    expect(validate("hi", { ...emptyOptions, maxTokens: "1.5" }).maxTokens).toBeTruthy();
    expect(validate("hi", { ...emptyOptions, maxTokens: "0" }).maxTokens).toBeTruthy();
    expect(validate("hi", { system: "s", temperature: "0.7", maxTokens: "200" })).toEqual({});
    expect(validate("x".repeat(9000), emptyOptions).prompt).toBeTruthy();
  });

  it("sends only what was set, and streams when the stream is to be cut", () => {
    expect(requestBody("default", " hi ", emptyOptions, [])).toEqual({ policy: "default", prompt: "hi" });
    const full = requestBody("hedged", "hi", { system: " be brief ", temperature: "0", maxTokens: "50" }, [{ provider: "openai", kind: "cut_stream" }]);
    expect(full).toEqual({ policy: "hedged", prompt: "hi", system: "be brief", temperature: 0, max_tokens: 50, stream: true, faults: [{ provider: "openai", kind: "cut_stream" }] });
  });
});

describe("surprise", () => {
  const seq = (...v: number[]) => { let i = 0; return () => v[i++ % v.length]; };
  it("picks a prompt other than the current one and breaks at most all-but-one provider", () => {
    for (let i = 0; i < 40; i++) {
      const s = surprise(["a", "b", "c"], "Write a haiku about a failing API");
      expect(s.prompt).not.toBe("Write a haiku about a failing API");
      expect(s.faults.length).toBeGreaterThanOrEqual(1);
      expect(s.faults.length).toBeLessThanOrEqual(2);
      expect(new Set(s.faults.map((f) => f.provider)).size).toBe(s.faults.length);
    }
  });
  it("leaves one provider working on a two-provider route, and breaks the only one on a one-provider route", () => {
    for (let i = 0; i < 20; i++) expect(surprise(["a", "b"], "").faults).toHaveLength(1);
    expect(surprise(["a"], "", seq(0.1)).faults).toHaveLength(1);
    expect(surprise([], "").faults).toEqual([]);
  });
});

describe("result type", () => {
  it("is the API's", () => {
    const r: Pick<PlaygroundResult, "outcome"> = { outcome: "ok" };
    expect(r.outcome).toBe("ok");
  });
});
