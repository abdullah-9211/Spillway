import type { components } from "./api-types";
import { durationLabel } from "./runs";

export type RunGraph = components["schemas"]["RunGraph"];
export type GraphNode = components["schemas"]["GraphNode"];
export type GraphAttempt = components["schemas"]["GraphAttempt"];

/** The geometry in px, from docs/design/tokens.json ("graph"). A test checks the two have not drifted apart. */
export const GEO = {
  nodeW: 90,
  nodeH: 80,
  colStep: 100,
  leftGutter: 72,
  recoveryShift: 30,
  modelTop: 120,
  toolTop: 270,
  height: 400,
  chipTop: 62,
  chipH: 44,
  rightMargin: 60,
  minWidth: 720,
} as const;

export const ICONS = {
  ok: "M2.5 6.5l2.2 2.2L9.5 3.5",
  run: "M6 1.5a4.5 4.5 0 1 0 4.5 4.5",
  fail: "M3 3l6 6M9 3l-6 6",
  redo: "M10 6a4 4 0 1 1-1.2-2.8M10 2.5v2.3H7.7",
  none: "M3 6h6",
  stop: "M3 3l6 6M9 3l-6 6",
  model: "M2 3h8v5.5H6.5L4.5 10.5V8.5H2z",
  tool: "M3 9l3.3-3.3M7.2 2.5a2.3 2.3 0 1 0 2.3 2.8L8 5.5 6.5 4z",
  goal: "M4 2.5v7l5-3.5z",
  wait: "M4 2.5v7M8 2.5v7",
  sleep: "M9.5 7A4 4 0 0 1 5 2.5a4 4 0 1 0 4.5 4.5z",
  compact: "M2 3.5h8M3.5 6h5M5 8.5h2",
  approve: "M2.5 6.5l2.2 2.2L9.5 3.5",
};

export type Lane = "model" | "tool";
export type NodeState = "finished" | "failed" | "running" | "stopped" | "waiting" | "sleeping";

export type LayoutNode = {
  id: string;
  kind: "goal" | "model" | "tool";
  x: number;
  y: number;
  w: number;
  h: number;
  /** goal nodes have no step; the rest show it in a corner */
  step: number | null;
  state: NodeState | "goal";
  reissued: boolean;
  label: string;
  meta: string;
  icon: string;
  mark: string;
  worker: string;
  epoch: number;
  tip: string;
  node: GraphNode | null;
};

export type LayoutEdge = { id: string; kind: "done" | "run" | "redo" | "end"; x: number; y: number; w: number; h: number; dashed: boolean };
export type LayoutBand = { id: string; x: number; w: number; tint: "base" | "recovered"; label: string; labelX: number };
export type LayoutCut = { id: string; x: number; label: string; afterStep: number };
export type LayoutChip = { id: string; x: number; y: number; w: number; h: number; text: string; nodeId: string };

/** Where a run that did not succeed ended, and why. A failure with no failed step (a rejection, a limit) shows here. */
export type LayoutEnd = { x: number; y: number; w: number; h: number; state: "failed" | "cancelled"; label: string; meta: string; tip: string };

export type Layout = {
  end: LayoutEnd | null;
  width: number;
  height: number;
  nodes: LayoutNode[];
  edges: LayoutEdge[];
  bands: LayoutBand[];
  cuts: LayoutCut[];
  chips: LayoutChip[];
  /** the connector from a fallback chip down to its model call */
  stubs: { id: string; x: number; y: number; h: number }[];
};

const laneOf = (t: GraphNode["type"]): Lane => (t === "model_call" || t === "compaction" ? "model" : "tool");

function iconFor(t: GraphNode["type"]): string {
  return t === "model_call" ? ICONS.model : t === "tool_call" ? ICONS.tool : t === "wait_human" ? ICONS.wait : t === "sleep" ? ICONS.sleep : ICONS.compact;
}
const top = (l: Lane) => (l === "model" ? GEO.modelTop : GEO.toolTop);

const kindWord: Record<GraphNode["type"], string> = { model_call: "model call", tool_call: "tool call", wait_human: "approval", sleep: "sleep", compaction: "compaction" };
const stateWord: Record<NodeState, string> = { finished: "finished", failed: "failed", running: "running", stopped: "stopped", waiting: "waiting for approval", sleeping: "sleeping" };

/** The words under a node's name: how long it took, or why it has no duration. */
export function nodeMeta(n: GraphNode): string {
  if (n.state === "waiting") return "needs you";
  if (n.state === "sleeping") return n.seconds ? `sleeps ${durationLabel(n.seconds * 1000)}` : "sleeping";
  if (n.type === "wait_human" && n.state === "finished" && n.decision) return n.decision === "approve" ? "approved" : "rejected";
  if (n.state === "stopped") return "stopped";
  if (n.state === "failed") return "failed";
  if (n.state === "finished" && n.cache && n.cache.startsWith("hit")) return "cached";
  const ms = n.duration_ms ?? 0;
  if (ms < 1000) return `${Math.max(ms, 0)} ms`;
  return `${(ms / 1000).toFixed(1)}s`;
}

function markFor(n: GraphNode): string {
  if (n.state === "waiting") return ICONS.wait;
  if (n.state === "sleeping") return ICONS.sleep;
  if (n.type === "wait_human" && n.decision === "reject") return ICONS.fail;
  if (n.state === "stopped") return ICONS.stop;
  if (n.state === "failed") return ICONS.fail;
  if (n.state === "running") return ICONS.run;
  return n.reissued ? ICONS.redo : ICONS.ok;
}

function labelFor(n: GraphNode): string {
  if (n.type === "tool_call") return n.tool || "tool";
  if (n.type === "model_call") return n.model || "model call";
  if (n.type === "wait_human") return n.gate && n.tool ? n.tool : "approval";
  if (n.type === "sleep") return "sleep";
  if (n.type === "compaction") return "compact";
  return kindWord[n.type];
}

/** The first attempt of a model call that failed before another one answered, if any: it is drawn as a chip above. */
function failedFirst(n: GraphNode): GraphAttempt | null {
  if (n.type !== "model_call" || n.attempts.length < 2) return null;
  const a = n.attempts[0];
  return a.error_kind || a.status ? a : null;
}

/**
 * Places a run's attempts on the canvas. Columns are attempts in order, in two lanes (model above, tools below). A
 * change of lease epoch between two attempts is a recovery: it draws a dashed cut, starts a new worker band, and shifts
 * everything after it right so the cut has room. Everything is plain numbers: the view absolutely positions elements
 * from them, so the layout can be tested without a browser.
 */
export function layoutGraph(g: Pick<RunGraph, "run" | "nodes" | "workers" | "recoveries">): Layout {
  const { nodeW: NW, nodeH: NH, colStep: STEP, leftGutter: X0, recoveryShift: SHIFT } = GEO;
  const out: Layout = { end: null, width: 0, height: GEO.height, nodes: [], edges: [], bands: [], cuts: [], chips: [], stubs: [] };

  // Which attempts begin a new recovery segment.
  const shiftAt: number[] = [];
  const cutBefore: number[] = [];
  let shifts = 0;
  g.nodes.forEach((n, i) => {
    if (i > 0 && n.epoch !== g.nodes[i - 1].epoch) {
      shifts += 1;
      cutBefore.push(i);
    }
    shiftAt.push(shifts);
  });
  const colX = (col: number, shift: number) => X0 + col * STEP + shift * SHIFT;

  out.nodes.push({
    id: "goal", kind: "goal", x: colX(0, 0), y: GEO.modelTop, w: NW, h: NH, step: null, state: "goal", reissued: false, label: "Goal",
    meta: g.run.key, icon: ICONS.goal, mark: ICONS.none, worker: "", epoch: 0, tip: "Goal", node: null,
  });
  g.nodes.forEach((n, i) => {
    const lane = laneOf(n.type);
    const id = `${n.step_no}:${n.epoch}`;
    const redo = n.reissued ? ", re-issued" : "";
    out.nodes.push({
      id, kind: lane === "model" ? "model" : "tool", x: colX(i + 1, shiftAt[i]), y: top(lane), w: NW, h: NH, step: n.step_no, state: n.state, reissued: n.reissued,
      label: labelFor(n), meta: nodeMeta(n), icon: iconFor(n.type), mark: markFor(n), worker: n.worker, epoch: n.epoch,
      tip: `Step ${n.step_no}, ${kindWord[n.type]}, ${stateWord[n.state]}${redo}, on ${n.worker}`, node: n,
    });
    const f = failedFirst(n);
    if (f) {
      const x = colX(i + 1, shiftAt[i]);
      const text = `${f.model} ${f.status ?? f.error_kind ?? "failed"}${n.attempts.filter((a) => a.error_kind || a.status).length > 1 ? " +" + (n.attempts.filter((a) => a.error_kind || a.status).length - 1) : ""}`;
      out.chips.push({ id: `chip:${id}`, x, y: GEO.chipTop, w: NW, h: GEO.chipH, text, nodeId: id });
      out.stubs.push({ id: `stub:${id}`, x: x + NW / 2 - 1, y: GEO.chipTop + GEO.chipH, h: GEO.modelTop - (GEO.chipTop + GEO.chipH) });
    }
  });

  // Edges between neighbours: straight within a lane, an elbow between lanes, dashed when it is a re-issue.
  for (let i = 1; i < out.nodes.length; i++) {
    const a = out.nodes[i - 1];
    const b = out.nodes[i];
    const n = b.node;
    const redo = !!n && n.reissued && !!a.node && a.node.step_no === n.step_no;
    const live = !redo && i === out.nodes.length - 1 && b.state === "running";
    const kind: LayoutEdge["kind"] = redo ? "redo" : live ? "run" : "done";
    const yA = a.y + NH / 2;
    const yB = b.y + NH / 2;
    const rA = a.x + NW;
    const lB = b.x;
    if (redo || yA === yB) {
      out.edges.push({ id: `e${i}`, kind, x: rA, y: yA - 1, w: lB - rA, h: redo ? 0 : 2, dashed: redo });
      continue;
    }
    const xm = Math.round(rA + (lB - rA) / 2);
    out.edges.push({ id: `e${i}a`, kind, x: rA, y: yA - 1, w: xm - rA + 1, h: 2, dashed: false });
    out.edges.push({ id: `e${i}b`, kind, x: xm, y: Math.min(yA, yB) - 1, w: 2, h: Math.abs(yB - yA) + 2, dashed: false });
    out.edges.push({ id: `e${i}c`, kind, x: xm, y: yB - 1, w: lB - xm, h: 2, dashed: false });
  }

  const last = out.nodes[out.nodes.length - 1];
  const st = g.run.status;
  if (st === "failed" || st === "cancelled") {
    const reason = (g.run.failure_reason ?? "").replace(/_/g, " ");
    const x = last.x + STEP;
    const y = last.y;
    out.end = {
      x, y, w: NW, h: NH, state: st, label: st === "failed" ? "Run failed" : "Cancelled", meta: reason || (st === "failed" ? "failed" : "cancelled"),
      tip: st === "failed" ? `The run failed${reason ? `: ${reason}` : ""}` : "The run was cancelled",
    };
    out.edges.push({ id: "eend", kind: "end", x: last.x + NW, y: y + NH / 2 - 1, w: x - (last.x + NW), h: 2, dashed: false });
  }
  out.width = Math.max(GEO.minWidth, (out.end ? out.end.x : last.x) + NW + GEO.rightMargin);

  // Cuts sit in the gap before the first attempt after a recovery; bands run between them.
  const gap = (STEP - NW + SHIFT) / 2;
  const cuts = cutBefore.map((i, k) => {
    const x = out.nodes[i + 1].x - gap;
    const rec = g.recoveries[k];
    const to = g.nodes[i].worker;
    return { id: `cut${k}`, x, label: `Lease expired, ${to} took over`, afterStep: rec ? rec.after_step : g.nodes[i - 1].step_no };
  });
  out.cuts = cuts;
  const edges = [0, ...cuts.map((c) => c.x), out.width];
  const segs: { worker: string; epoch: number }[] = [];
  let startNode = 0;
  for (const i of [...cutBefore, g.nodes.length]) {
    const seg = g.nodes[startNode];
    segs.push({ worker: seg ? seg.worker : "", epoch: seg ? seg.epoch : 0 });
    startNode = i;
  }
  segs.forEach((s, k) => {
    out.bands.push({
      id: `band${k}`, x: edges[k], w: edges[k + 1] - edges[k], tint: k === 0 ? "base" : "recovered",
      label: k === 0 ? `Worker ${s.worker}` : `Worker ${s.worker}, lease epoch ${s.epoch}`, labelX: edges[k] + 12,
    });
  });
  if (g.nodes.length === 0) out.bands = [{ id: "band0", x: 0, w: out.width, tint: "base", label: g.run.lease_owner ? `Worker ${g.run.lease_owner}` : "No worker yet", labelX: 12 }];
  return out;
}

// --- the numbers around the graph ---

export type Meter = { label: string; value: string; of: string; share: number };

const money = (s: string) => `$${Number(s).toFixed(s.length <= 8 && Number(s) < 10 ? 3 : 2)}`;

export function clock(sec: number): string {
  const s = Math.max(0, Math.floor(sec));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
}

/** Steps, cost and time against the run's limits. Time is measured to the end of the run, or to now. */
export function meters(run: RunGraph["run"], now: Date = new Date()): Meter[] {
  const end = run.finished_at ? new Date(run.finished_at).getTime() : now.getTime();
  const elapsed = Math.max(0, (end - new Date(run.created_at).getTime()) / 1000);
  const share = (n: number, d: number) => (d > 0 ? Math.min(1, Math.max(0, n / d)) : 0);
  return [
    { label: "Steps", value: String(run.step_count), of: `of ${run.max_steps}`, share: share(run.step_count, run.max_steps) },
    { label: "Cost", value: money(run.cost_usd), of: `of ${money(run.max_cost_usd)}`, share: share(Number(run.cost_usd), Number(run.max_cost_usd)) },
    { label: "Elapsed", value: clock(elapsed), of: `of ${clock(run.deadline_seconds)}`, share: share(elapsed, run.deadline_seconds) },
  ];
}

export function recoveredLabel(n: number): string {
  return n === 0 ? "no recoveries" : n === 1 ? "recovered once" : `recovered ${n} times`;
}

export { durationLabel };
