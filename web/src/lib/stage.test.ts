import { describe, expect, it } from "vitest";
import type { GraphNode, RunGraph } from "./graph";
import { RECOVERY_PAUSE_MS, feedFrom, packetAt, reach, replayPlan, stepMs, tweenValue, workerCards } from "./stage";

const n = (o: Partial<GraphNode> & Pick<GraphNode, "step_no" | "type">): GraphNode =>
  ({ state: "finished", worker: "w-1", epoch: 1, reissued: false, started_at: "2026-10-09T12:00:00Z", duration_ms: 1000, cost_usd: "0", attempts: [], ...o }) as GraphNode;

const lay = (nodes: GraphNode[]) => [{ id: "goal", node: null, kind: "goal", epoch: 0 }, ...nodes.map((x) => ({ id: `${x.step_no}:${x.epoch}`, node: x, kind: x.type === "model_call" ? "model" : "tool", epoch: x.epoch }))];

describe("replay plan", () => {
  it("gives each step a share of its real time, within bounds", () => {
    expect([100, 1000, 2000, 4000, 10_000, null].map(stepMs)).toEqual([350, 350, 350, 600, 900, 350]);
  });

  it("lays steps end to end and pauses where a worker took over", () => {
    const p = replayPlan(lay([n({ step_no: 1, type: "model_call" }), n({ step_no: 2, type: "tool_call", state: "stopped", duration_ms: null }), n({ step_no: 2, type: "tool_call", epoch: 2, worker: "w-2", reissued: true })]));
    expect(p.steps.map((s) => s.id)).toEqual(["goal", "1:1", "2:1", "2:2"]);
    expect(p.steps.map((s) => s.recoveryBefore)).toEqual([false, false, false, true]);
    expect(p.steps[1].start).toBe(p.steps[0].end);
    expect(p.steps[3].start).toBe(p.steps[2].end + RECOVERY_PAUSE_MS);
    expect(p.total).toBe(p.steps[3].end);
    for (let i = 1; i < p.steps.length; i++) expect(p.steps[i].start).toBeGreaterThanOrEqual(p.steps[i - 1].end);
  });

  it("knows where it is in a replay", () => {
    const p = replayPlan(lay([n({ step_no: 1, type: "model_call" }), n({ step_no: 2, type: "tool_call" })]));
    const s = p.steps[1];
    expect([reach(s, s.start - 1), reach(s, s.start), reach(s, s.end - 1), reach(s, s.end)]).toEqual(["ahead", "active", "active", "done"]);
    expect(packetAt(p, 0)).toEqual({ index: 0, u: 0 });
    expect(packetAt(p, s.start + 1).index).toBe(1);
    expect(packetAt(p, p.total + 100)).toEqual({ index: 2, u: 1 });
    expect(packetAt({ total: 0, steps: [] }, 5)).toEqual({ index: 0, u: 0 });
  });

  it("waits at the previous node during a recovery pause", () => {
    const p = replayPlan(lay([n({ step_no: 1, type: "tool_call", state: "stopped", duration_ms: null }), n({ step_no: 1, type: "tool_call", epoch: 2, reissued: true })]));
    const mid = p.steps[2].start - RECOVERY_PAUSE_MS / 2;
    expect(packetAt(p, mid)).toEqual({ index: 1, u: 1 });
  });
});

describe("the feed", () => {
  const g = {
    run: { id: "r", status: "succeeded", created_at: "2026-10-09T11:59:00Z", finished_at: "2026-10-09T12:00:09Z", failure_reason: null, lease_expires_at: null, lease_owner: null },
    nodes: [
      n({ step_no: 1, type: "model_call", model: "sonnet", started_at: "2026-10-09T12:00:00Z", duration_ms: 2900, attempts: [{ provider: "openai", model: "mini", kind: "primary", latency_ms: 340, status: 503 }, { provider: "anthropic", model: "sonnet", kind: "fallback", latency_ms: 2560 }] }),
      n({ step_no: 2, type: "tool_call", tool: "fetch_page", state: "stopped", duration_ms: null, started_at: "2026-10-09T12:00:03Z" }),
      n({ step_no: 2, type: "tool_call", tool: "fetch_page", epoch: 2, worker: "w-2", reissued: true, started_at: "2026-10-09T12:00:08Z", duration_ms: 200 }),
    ],
    workers: [{ id: "w-1", epochs: [1] }, { id: "w-2", epochs: [2] }],
    recoveries: [{ after_step: 2, from_worker: "w-1", to_worker: "w-2", epoch: 2, at: "2026-10-09T12:00:07Z" }],
  } as unknown as RunGraph;

  it("tells the story newest first, with recoveries, fallbacks and the end", () => {
    const f = feedFrom(g);
    expect(f[0]).toMatchObject({ tone: "ok", text: "Run succeeded" });
    expect(f.at(-1)?.text).toBe("Run created");
    const texts = f.map((x) => x.text);
    expect(texts).toContain("w-2 took over from w-1 at lease epoch 2");
    expect(texts).toContain("Step 2 stopped on w-1: the worker was lost");
    expect(texts).toContain("Step 2 re-issued on w-2 (fetch_page)");
    expect(texts).toContain("openai/mini answered 503: fell back");
    expect(texts).toContain("Step 1 finished in 3s");
    const times = f.map((x) => x.at);
    expect([...times].sort((a, b) => b - a)).toEqual(times);
    expect(new Set(f.map((x) => x.key)).size).toBe(f.length);
  });

  it("states a failure with its reason", () => {
    const failed = { ...g, run: { ...g.run, status: "failed", failure_reason: "max_steps" } } as RunGraph;
    expect(feedFrom(failed)[0]).toMatchObject({ tone: "fail", text: "Run failed (max steps)" });
  });
});

describe("workers", () => {
  const base = { run: { status: "running", lease_expires_at: "2026-10-09T12:00:30Z" }, nodes: [n({ step_no: 1, type: "tool_call", state: "stopped", duration_ms: null }), n({ step_no: 1, type: "tool_call", epoch: 2, worker: "w-2" })], workers: [{ id: "w-1", epochs: [1] }, { id: "w-2", epochs: [2] }] } as unknown as RunGraph;
  it("shows who holds the run, with its lease, and who was lost", () => {
    const c = workerCards(base, new Date("2026-10-09T12:00:06Z"));
    expect(c).toEqual([
      { id: "w-1", epochs: [1], state: "stopped", note: "lost: its lease expired" },
      { id: "w-2", epochs: [2], state: "holding", note: "lease renews, expires in 24s" },
    ]);
  });
  it("a finished run's last worker finished it", () => {
    const c = workerCards({ ...base, run: { ...base.run, status: "succeeded" } } as RunGraph, new Date());
    expect(c[1]).toMatchObject({ state: "done", note: "finished the run" });
  });
  it("a parked run is held by nobody: the worker let go of it", () => {
    const one = { run: { status: "waiting_human", lease_expires_at: null }, nodes: [n({ step_no: 1, type: "model_call" })], workers: [{ id: "w-1", epochs: [1] }] } as unknown as RunGraph;
    expect(workerCards(one, new Date())).toEqual([{ id: "w-1", epochs: [1], state: "parked", note: "let go of the run while it waits for a decision" }]);
    expect(workerCards({ ...one, run: { ...one.run, status: "sleeping" } } as RunGraph, new Date())[0].note).toBe("let go of the run while it sleeps");
  });
  it("a single worker run has nothing lost", () => {
    const one = { run: { status: "succeeded" }, nodes: [n({ step_no: 1, type: "model_call" })], workers: [{ id: "w-1", epochs: [1] }] } as unknown as RunGraph;
    expect(workerCards(one, new Date())).toEqual([{ id: "w-1", epochs: [1], state: "done", note: "finished the run" }]);
  });
});

describe("tween", () => {
  it("eases from the start to the end", () => {
    expect([0, 0.5, 1, 2, -1].map((t) => Math.round(tweenValue(10, 20, t) * 100) / 100)).toEqual([10, 18.75, 20, 20, 10]);
  });
});
