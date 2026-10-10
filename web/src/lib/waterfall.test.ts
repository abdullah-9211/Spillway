import { describe, expect, it } from "vitest";
import type { GraphNode, RunGraph } from "./graph";
import { keepNode, neighbours, pretty, tokens, waterfall } from "./waterfall";

const at = (s: number) => new Date(Date.UTC(2026, 9, 9, 12, 0, s)).toISOString();
const n = (o: Partial<GraphNode> & Pick<GraphNode, "step_no" | "type">): GraphNode =>
  ({ state: "finished", worker: "w-1", epoch: 1, reissued: false, started_at: at(0), duration_ms: 1000, cost_usd: "0", attempts: [], ...o }) as GraphNode;
const run = (nodes: GraphNode[], over: Partial<RunGraph["run"]> = {}) => ({ run: { created_at: at(0), finished_at: at(20), status: "succeeded", ...over } as RunGraph["run"], nodes });

describe("waterfall", () => {
  it("places each attempt along the run's time", () => {
    const w = waterfall(run([n({ step_no: 1, type: "model_call", started_at: at(0), duration_ms: 5000 }), n({ step_no: 2, type: "tool_call", tool: "fetch", started_at: at(5), duration_ms: 10_000 })]));
    expect(w.totalMs).toBe(20_000);
    expect(w.bars.map((b) => [b.label, Math.round(b.left), Math.round(b.width)])).toEqual([["model", 0, 25], ["fetch", 25, 50]]);
    expect(w.ticks[0]).toEqual({ at: 0, label: "0" });
    expect(w.ticks.length).toBeGreaterThan(3);
    expect(w.gaps).toEqual([]);
  });

  it("shows a stopped attempt until the next one began, and the idle time between as a gap", () => {
    const w = waterfall(run([
      n({ step_no: 1, type: "tool_call", started_at: at(0), duration_ms: 2000, worker: "w-1" }),
      n({ step_no: 2, type: "tool_call", state: "stopped", duration_ms: null, started_at: at(2), worker: "w-1" }),
      n({ step_no: 2, type: "tool_call", epoch: 2, reissued: true, worker: "w-2", started_at: at(10), duration_ms: 2000 }),
    ]));
    const stopped = w.bars[1];
    expect(stopped.open).toBe(true);
    expect(stopped.startMs).toBe(2000);
    expect(stopped.endMs).toBe(10_000);
    expect(w.bars[2].reissued).toBe(true);
  });

  it("an idle stretch with no attempt running is a gap with its length", () => {
    const w = waterfall(run([n({ step_no: 1, type: "model_call", started_at: at(0), duration_ms: 1000 }), n({ step_no: 2, type: "model_call", started_at: at(6), duration_ms: 1000 })], { finished_at: at(7) }));
    expect(w.gaps).toHaveLength(1);
    expect(w.gaps[0].label).toBe("5.0s with no worker");
  });

  it("a running attempt reaches to now, and a very short one is still a sliver", () => {
    const w = waterfall(run([n({ step_no: 1, type: "model_call", started_at: at(0), duration_ms: 3 }), n({ step_no: 2, type: "tool_call", state: "running", started_at: at(2), duration_ms: 4000 })], { finished_at: null, status: "running" }), new Date(Date.UTC(2026, 9, 9, 12, 0, 8)));
    expect(w.totalMs).toBe(8000);
    expect(w.bars[0].width).toBeGreaterThanOrEqual(0.8);
    expect(w.bars[1].state).toBe("running");
  });

  it("copes with a run with no steps", () => {
    expect(waterfall(run([]))).toEqual({ bars: [], totalMs: 0, ticks: [], gaps: [] });
  });
});

describe("moving between steps", () => {
  const ids = ["1:1", "2:1", "2:2", "3:2"];
  it("finds the neighbours, and starts at the first", () => {
    expect(neighbours(ids, "2:1")).toEqual({ prev: "1:1", next: "2:2" });
    expect(neighbours(ids, "1:1")).toEqual({ prev: null, next: "2:1" });
    expect(neighbours(ids, "3:2")).toEqual({ prev: "2:2", next: null });
    expect(neighbours(ids, null)).toEqual({ prev: null, next: "1:1" });
    expect(neighbours([], null)).toEqual({ prev: null, next: null });
  });
});

describe("reading payloads", () => {
  it("lays JSON out and leaves other text alone", () => {
    expect(pretty('{"a":1,"b":[true,null]}')).toEqual({ text: '{\n  "a": 1,\n  "b": [\n    true,\n    null\n  ]\n}', json: true });
    expect(pretty("5 results returned")).toEqual({ text: "5 results returned", json: false });
    expect(pretty("{not json")).toEqual({ text: "{not json", json: false });
    expect(pretty("   ")).toEqual({ text: "   ", json: false });
  });
  it("splits JSON into tokens that add back up to the text", () => {
    const src = '{\n  "url": "https://x/y",\n  "n": -2.5e3,\n  "ok": true,\n  "z": null\n}';
    const t = tokens(src);
    expect(t.map((x) => x.v).join("")).toBe(src);
    expect(t.filter((x) => x.t === "key").map((x) => x.v)).toEqual(['"url"', '"n"', '"ok"', '"z"']);
    expect(t.find((x) => x.t === "str")?.v).toBe('"https://x/y"');
    expect(t.find((x) => x.t === "num")?.v).toBe("-2.5e3");
    expect(t.filter((x) => x.t === "lit").map((x) => x.v)).toEqual(["true", "null"]);
    expect(tokens('"a \\" quote"').map((x) => x.v).join("")).toBe('"a \\" quote"');
  });
});

describe("timeline filters", () => {
  const fb = n({ step_no: 3, type: "model_call", attempts: [{ provider: "o", model: "m", kind: "primary", latency_ms: 1, status: 503 }, { provider: "a", model: "s", kind: "fallback", latency_ms: 2 }] });
  const nodes = [n({ step_no: 1, type: "model_call" }), n({ step_no: 2, type: "tool_call" }), n({ step_no: 2, type: "tool_call", state: "stopped", duration_ms: null }), n({ step_no: 2, type: "tool_call", epoch: 2, reissued: true }), fb];
  it("keeps what was asked for", () => {
    expect(nodes.map((x) => keepNode(x, "all"))).toEqual([true, true, true, true, true]);
    expect(nodes.map((x) => keepNode(x, "model"))).toEqual([true, false, false, false, true]);
    expect(nodes.map((x) => keepNode(x, "tool"))).toEqual([false, true, true, true, false]);
    expect(nodes.map((x) => keepNode(x, "problems"))).toEqual([false, false, true, true, true]);
  });
});
