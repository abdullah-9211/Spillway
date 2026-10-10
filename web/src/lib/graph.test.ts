import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { GEO, clock, layoutGraph, meters, nodeMeta, recoveredLabel, type GraphNode, type RunGraph } from "./graph";

type NodeSpec = Partial<GraphNode> & Pick<GraphNode, "step_no" | "type">;

function node(s: NodeSpec): GraphNode {
  return {
    state: "finished", worker: "w-1", epoch: 1, reissued: false, started_at: "2026-10-09T12:00:00Z", duration_ms: 1400, cost_usd: "0.004800", attempts: [],
    ...s,
  } as GraphNode;
}

function graph(nodes: GraphNode[], over: Partial<RunGraph["run"]> = {}): RunGraph {
  const workers: RunGraph["workers"] = [];
  const recoveries: RunGraph["recoveries"] = [];
  nodes.forEach((n, i) => {
    let w = workers.find((x) => x.id === n.worker);
    if (!w) workers.push((w = { id: n.worker, epochs: [] }));
    if (!w.epochs.includes(n.epoch)) w.epochs.push(n.epoch);
    const p = nodes[i - 1];
    if (p && p.epoch !== n.epoch) recoveries.push({ after_step: p.step_no, from_worker: p.worker, to_worker: n.worker, epoch: n.epoch, at: "2026-10-09T12:01:00Z" });
  });
  return {
    run: {
      id: "r1", status: "running", goal: "Research pricing", key: "research-bot", model: "default", tools: [], step_count: nodes.length, cost_usd: "0.0624", max_steps: 50,
      max_cost_usd: "1.0", deadline_seconds: 900, created_at: "2026-10-09T11:56:00Z", finished_at: null, deadline_at: "2026-10-09T12:11:00Z", failure_reason: null,
      cancel_requested: false, lease_owner: null, lease_epoch: 1, lease_expires_at: null, ...over,
    } as RunGraph["run"],
    workers, nodes, recoveries, last_event_id: 42,
  };
}

const M = (n: number, over: Partial<GraphNode> = {}) => node({ step_no: n, type: "model_call", model: "sonnet", ...over });
const T = (n: number, over: Partial<GraphNode> = {}) => node({ step_no: n, type: "tool_call", tool: "web_search", duration_ms: 600, ...over });

describe("layoutGraph", () => {
  it("lays out a run with no recovery: a goal, then one column per attempt, two lanes, no cuts", () => {
    const l = layoutGraph(graph([M(1), T(2), M(3)]));
    expect(l.nodes.map((n) => [n.label, n.x, n.y])).toEqual([["Goal", 72, 120], ["sonnet", 172, 120], ["web_search", 272, 270], ["sonnet", 372, 120]]);
    expect(l.cuts).toEqual([]);
    expect(l.bands).toHaveLength(1);
    expect(l.bands[0]).toMatchObject({ x: 0, tint: "base", label: "Worker w-1" });
    expect(l.chips).toEqual([]);
    expect(l.height).toBe(400);
    expect(l.nodes.every((n) => n.w === 90 && n.h === 80)).toBe(true);
  });

  it("draws straight edges within a lane and elbows between lanes", () => {
    const l = layoutGraph(graph([M(1), T(2), M(3)]));
    // goal -> model: same lane, one straight edge. model -> tool: an elbow of three pieces.
    expect(l.edges.map((e) => e.id)).toEqual(["e1", "e2a", "e2b", "e2c", "e3a", "e3b", "e3c"]);
    expect(l.edges[0]).toMatchObject({ x: 162, y: 159, w: 10, h: 2, kind: "done", dashed: false });
    const [a, v, c] = l.edges.slice(1, 4);
    expect(a.y).toBe(159); // leaves the model's middle
    expect(v.w).toBe(2); // the vertical piece
    expect(v.h).toBe(150 + 2); // 120+40 to 270+40
    expect(c.y).toBe(309); // enters the tool's middle
  });

  it("matches the approved design: one recovery shifts the columns after it and puts a cut in the gap", () => {
    const l = layoutGraph(graph([
      M(1, { worker: "w-2" }), T(2, { worker: "w-2" }), M(3, { worker: "w-2", attempts: [{ provider: "openai", model: "mini", kind: "primary", latency_ms: 340, status: 503, error_kind: "server" }, { provider: "anthropic", model: "sonnet", kind: "fallback", latency_ms: 2560 }] }),
      T(4, { worker: "w-2" }), M(5, { worker: "w-2" }), T(6, { worker: "w-2", state: "stopped", duration_ms: null }),
      T(6, { worker: "w-4", epoch: 3, reissued: true, previous_worker: "w-2", previous_epoch: 1, duration_ms: 200 }),
      M(7, { worker: "w-4", epoch: 3, cache: "hit_exact" }), T(8, { worker: "w-4", epoch: 3, state: "running", duration_ms: 3200 }),
    ]));
    // The design's x positions: columns 0..6 at 72+100i, then the 30px shift after the cut.
    expect(l.nodes.map((n) => n.x)).toEqual([72, 172, 272, 372, 472, 572, 672, 802, 902, 1002]);
    expect(l.cuts).toEqual([{ id: "cut0", x: 782, label: "Lease expired, w-4 took over", afterStep: 6 }]);
    expect(l.bands.map((b) => [b.x, b.tint, b.label])).toEqual([[0, "base", "Worker w-2"], [782, "recovered", "Worker w-4, lease epoch 3"]]);
    expect(l.bands[0].w).toBe(782);
    // The fallback chip sits over step 3 with its connector.
    expect(l.chips).toEqual([{ id: "chip:3:1", x: 372, y: 62, w: 90, h: 44, text: "mini 503", nodeId: "3:1" }]);
    expect(l.stubs).toEqual([{ id: "stub:3:1", x: 416, y: 106, h: 14 }]);
    // The stopped attempt and its re-issue are joined by a dashed line; the live node's edge is the run colour.
    const redo = l.edges.find((e) => e.kind === "redo");
    expect(redo).toMatchObject({ dashed: true, x: 762, y: 309, h: 0 });
    expect(l.edges.filter((e) => e.kind === "run").length).toBeGreaterThan(0);
    // Node states carry through, with their words.
    expect(l.nodes[6]).toMatchObject({ state: "stopped", meta: "stopped" });
    expect(l.nodes[7]).toMatchObject({ reissued: true, meta: "0 ms".replace("0 ms", "200 ms") });
    expect(l.nodes[8].meta).toBe("cached");
    expect(l.nodes[9]).toMatchObject({ state: "running", meta: "3.2s" });
    expect(l.nodes[7].tip).toBe("Step 6, tool call, finished, re-issued, on w-4");
  });

  it("with two recoveries there are two cuts and three bands, each shifting the rest further", () => {
    const l = layoutGraph(graph([
      M(1, { worker: "w-1", epoch: 1 }), T(2, { worker: "w-1", epoch: 1, state: "stopped", duration_ms: null }),
      T(2, { worker: "w-2", epoch: 2, reissued: true, state: "stopped", duration_ms: null }),
      T(2, { worker: "w-3", epoch: 3, reissued: true }), M(3, { worker: "w-3", epoch: 3 }),
    ]));
    expect(l.nodes.map((n) => n.x)).toEqual([72, 172, 272, 402, 532, 632]);
    expect(l.cuts.map((c) => c.x)).toEqual([382, 512]);
    expect(l.bands.map((b) => b.label)).toEqual(["Worker w-1", "Worker w-2, lease epoch 2", "Worker w-3, lease epoch 3"]);
    expect(l.bands.map((b) => [b.x, b.w])).toEqual([[0, 382], [382, 130], [512, l.width - 512]]);
    expect(l.edges.filter((e) => e.kind === "redo")).toHaveLength(2);
    // Bands tile the canvas exactly.
    expect(l.bands.reduce((t, b) => t + b.w, 0)).toBe(l.width);
  });

  it("a chip counts further failed attempts, and a model call that answered first time has none", () => {
    const failed = (m: string, s: number) => ({ provider: "p", model: m, kind: "primary", latency_ms: 10, status: s, error_kind: "server" });
    const l = layoutGraph(graph([M(1, { attempts: [failed("a", 429), failed("b", 503), { provider: "p", model: "c", kind: "fallback", latency_ms: 5 }] }), M(2, { attempts: [{ provider: "p", model: "c", kind: "primary", latency_ms: 5 }] })]));
    expect(l.chips.map((c) => c.text)).toEqual(["a 429 +1"]);
  });

  it("copes with a run that has not started a step", () => {
    const l = layoutGraph(graph([], { lease_owner: "w-9" }));
    expect(l.nodes.map((n) => n.id)).toEqual(["goal"]);
    expect(l.edges).toEqual([]);
    expect(l.bands[0].label).toBe("Worker w-9");
    expect(l.width).toBeGreaterThanOrEqual(GEO.minWidth);
    expect(layoutGraph(graph([])).bands[0].label).toBe("No worker yet");
  });

  it("is a pure function: the same graph gives the same layout, and the input is not changed", () => {
    const g = graph([M(1), T(2)]);
    const before = JSON.stringify(g);
    expect(layoutGraph(g)).toEqual(layoutGraph(g));
    expect(JSON.stringify(g)).toBe(before);
  });

  it("uses the geometry in the design tokens", () => {
    const t = JSON.parse(readFileSync(resolve(__dirname, "../../../docs/design/tokens.json"), "utf8")).graph;
    expect({ nodeW: GEO.nodeW, nodeH: GEO.nodeH, colStep: GEO.colStep, leftGutter: GEO.leftGutter, recoveryShift: GEO.recoveryShift, modelTop: GEO.modelTop, toolTop: GEO.toolTop, height: GEO.height, chipTop: GEO.chipTop, chipH: GEO.chipH }).toEqual({
      nodeW: t.node.w, nodeH: t.node.h, colStep: t.colStep, leftGutter: t.leftGutter, recoveryShift: t.recoveryShift, modelTop: t.lane.modelTop, toolTop: t.lane.toolTop, height: t.height, chipTop: t.fallbackChip.top, chipH: t.fallbackChip.h,
    });
  });
});

describe("words and numbers around the graph", () => {
  it("says how long a node took, or why it did not", () => {
    const n = (o: Partial<GraphNode>) => node({ step_no: 1, type: "tool_call", ...o });
    expect([n({ duration_ms: 612 }), n({ duration_ms: 2900 }), n({ state: "stopped", duration_ms: null }), n({ state: "failed" }), n({ type: "model_call", cache: "hit_semantic" })].map(nodeMeta)).toEqual(["612 ms", "2.9s", "stopped", "failed", "cached"]);
  });
  it("measures steps, cost and time against the limits", () => {
    const g = graph([M(1)], { step_count: 8, cost_usd: "0.062400", max_cost_usd: "1.000000", created_at: "2026-10-09T11:56:00Z" });
    const [steps, cost, time] = meters(g.run, new Date("2026-10-09T12:00:02Z"));
    expect(steps).toMatchObject({ value: "8", of: "of 50", share: 0.16 });
    expect(cost).toMatchObject({ value: "$0.062", of: "of $1.000" });
    expect(cost.share).toBeCloseTo(0.0624);
    expect(time).toMatchObject({ value: "4:02", of: "of 15:00" });
    expect(time.share).toBeCloseTo(242 / 900);
    expect(meters({ ...g.run, finished_at: "2026-10-09T11:57:00Z" }, new Date())[2].value).toBe("1:00");
  });
  it("never lets a meter overflow", () => {
    const g = graph([], { step_count: 80, max_steps: 50 });
    expect(meters(g.run)[0].share).toBe(1);
  });
  it("formats clocks and recoveries", () => {
    expect([clock(0), clock(59), clock(62), clock(900), clock(-3)]).toEqual(["0:00", "0:59", "1:02", "15:00", "0:00"]);
    expect([0, 1, 3].map(recoveredLabel)).toEqual(["no recoveries", "recovered once", "recovered 3 times"]);
  });
});
