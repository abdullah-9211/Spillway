import { describe, expect, it } from "vitest";
import type { Attempt, Policy } from "./playground";
import { HOP_MS, nextFault, planNodes, playMs, runNodes, signature } from "./scene";

const pol = (over: Partial<Policy>): Policy => ({
  name: "default", type: "fallback", description: "", models: [{ id: "sonnet", provider: "anthropic" }, { id: "mini", provider: "openai" }], providers: ["anthropic", "openai"], weights: [], tag: "", hedge_after_ms: 0, ...over,
});

describe("planNodes", () => {
  it("starts with the request and lists each model, marking the ones with a fault", () => {
    const n = planNodes(pol({}), [{ provider: "openai", kind: "server_error" }]);
    expect(n.map((x) => [x.label, x.fault])).toEqual([["Request", undefined], ["sonnet", undefined], ["mini", "server_error"]]);
  });
  it("shows one node for a fixed or cheapest policy, and only the request with no policy", () => {
    expect(planNodes(pol({ type: "cheapest" }), [])).toHaveLength(2);
    expect(planNodes(pol({ type: "fixed" }), [])).toHaveLength(2);
    expect(planNodes(undefined, [])).toHaveLength(1);
  });
});

describe("runNodes", () => {
  const bad: Attempt = { provider: "anthropic", model: "sonnet", kind: "primary", latency_ms: 10, status: 429, error_kind: "rate_limited", injected: true };
  const good: Attempt = { provider: "openai", model: "mini", kind: "fallback", latency_ms: 10 };
  it("turns attempts into failed and answered nodes in order", () => {
    const n = runNodes({ policy: "default", attempts: [bad, good], faults: [{ provider: "anthropic", kind: "rate_limit" }] });
    expect(n.map((x) => [x.state, x.fault])).toEqual([["start", undefined], ["fail", "rate_limit"], ["ok", undefined]]);
  });
});

describe("timing and signature", () => {
  it("scales with the number of hops", () => {
    expect(playMs([{ id: "a", label: "a", state: "start" }])).toBe(350);
    expect(playMs(planNodes(pol({}), []))).toBe(2 * HOP_MS + 350);
  });
  it("changes with state and fault but not with identical input", () => {
    const a = planNodes(pol({}), []);
    expect(signature(a)).toBe(signature(planNodes(pol({}), [])));
    expect(signature(a)).not.toBe(signature(planNodes(pol({}), [{ provider: "openai", kind: "slow" }])));
  });
});

describe("nextFault", () => {
  it("cycles working, 429, 503, slow, working", () => {
    expect([undefined, "rate_limit", "server_error", "slow"].map((k) => nextFault(k as never))).toEqual(["rate_limit", "server_error", "slow", null]);
  });
});
