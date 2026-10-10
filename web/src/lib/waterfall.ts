import type { GraphNode, RunGraph } from "./graph";

export type Bar = {
  id: string;
  step: number;
  label: string;
  worker: string;
  epoch: number;
  state: GraphNode["state"];
  reissued: boolean;
  /** 0 to 100, position and width along the run's time */
  left: number;
  width: number;
  startMs: number;
  endMs: number;
  /** a stopped attempt's bar runs until the next attempt of the same step began, or the run ended */
  open: boolean;
};

export type Waterfall = { bars: Bar[]; totalMs: number; ticks: { at: number; label: string }[]; gaps: { left: number; width: number; label: string }[] };

const MIN_WIDTH = 0.8; // a very short step is still a visible sliver

const nodeLabel = (n: GraphNode) => (n.type === "tool_call" ? n.tool || "tool" : n.type === "model_call" ? n.model || "model" : n.type.replace("_", " "));

function tickStep(totalMs: number): number {
  const steps = [100, 250, 500, 1000, 2000, 5000, 10_000, 15_000, 30_000, 60_000, 120_000, 300_000, 600_000];
  return steps.find((s) => totalMs / s <= 8) ?? 600_000;
}

const tickLabel = (ms: number) => (ms === 0 ? "0" : ms < 1000 ? `${ms} ms` : ms % 60_000 === 0 ? `${ms / 60_000}m` : ms < 60_000 ? `${ms / 1000}s` : `${Math.floor(ms / 60_000)}m ${Math.round((ms % 60_000) / 1000)}s`);

/**
 * The run laid out along time: one bar per attempt, where it began and how long it took. Idle stretches (a worker lost
 * and the lease running out before another took over) show as gaps, which is where the time of a recovery goes.
 */
export function waterfall(g: Pick<RunGraph, "run" | "nodes">, now: Date = new Date()): Waterfall {
  if (g.nodes.length === 0) return { bars: [], totalMs: 0, ticks: [], gaps: [] };
  const t0 = new Date(g.run.created_at).getTime();
  const runEnd = g.run.finished_at ? new Date(g.run.finished_at).getTime() : now.getTime();
  const raw = g.nodes.map((n, i) => {
    const start = new Date(n.started_at).getTime();
    let end = n.duration_ms !== null ? start + n.duration_ms : runEnd;
    if (n.state === "stopped") {
      const next = g.nodes.slice(i + 1).find((o) => o.step_no === n.step_no);
      end = next ? new Date(next.started_at).getTime() : runEnd;
    }
    return { n, start: Math.max(start, t0), end: Math.max(end, start) };
  });
  const lastEnd = Math.max(runEnd, ...raw.map((r) => r.end));
  const total = Math.max(lastEnd - t0, 1);
  const pct = (ms: number) => ((ms - t0) / total) * 100;
  const bars: Bar[] = raw.map(({ n, start, end, }) => ({
    id: `${n.step_no}:${n.epoch}`, step: n.step_no, label: nodeLabel(n), worker: n.worker, epoch: n.epoch, state: n.state, reissued: n.reissued,
    left: pct(start), width: Math.max(pct(end) - pct(start), MIN_WIDTH), startMs: start - t0, endMs: end - t0, open: n.state === "stopped",
  }));

  // Idle stretches: after a stopped attempt's worker was lost until the next attempt began (the lease running out).
  const gaps: Waterfall["gaps"] = [];
  const sorted = [...raw].sort((a, b) => a.start - b.start);
  let reach = sorted[0].end;
  for (const r of sorted.slice(1)) {
    if (r.start - reach > 400) gaps.push({ left: pct(reach), width: pct(r.start) - pct(reach), label: `${((r.start - reach) / 1000).toFixed(1)}s with no worker` });
    reach = Math.max(reach, r.end);
  }
  const s = tickStep(total);
  const ticks: Waterfall["ticks"] = [];
  for (let at = 0; at <= total; at += s) ticks.push({ at: (at / total) * 100, label: tickLabel(at) });
  return { bars, totalMs: total, ticks, gaps };
}

/** The ids of the attempts on either side of a step, for moving with the arrow keys. */
export function neighbours(ids: string[], current: string | null): { prev: string | null; next: string | null } {
  const i = current ? ids.indexOf(current) : -1;
  if (i === -1) return { prev: null, next: ids[0] ?? null };
  return { prev: ids[i - 1] ?? null, next: ids[i + 1] ?? null };
}

/** JSON text laid out for reading; anything else is returned as it is. */
export function pretty(text: string): { text: string; json: boolean } {
  const t = text.trim();
  if (!t || (t[0] !== "{" && t[0] !== "[")) return { text, json: false };
  try {
    return { text: JSON.stringify(JSON.parse(t), null, 2), json: true };
  } catch {
    return { text, json: false };
  }
}

/** Splits JSON text into tokens for light colouring (keys, strings, numbers, words, punctuation). */
export function tokens(json: string): { t: "key" | "str" | "num" | "lit" | "punct" | "ws"; v: string }[] {
  const out: { t: "key" | "str" | "num" | "lit" | "punct" | "ws"; v: string }[] = [];
  const re = /("(?:\\.|[^"\\])*")(\s*:)?|(-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)|\b(true|false|null)\b|([{}[\],:])|(\s+)|([^\s])/g;
  let m: RegExpExecArray | null;
  while ((m = re.exec(json)) !== null) {
    if (m[1] !== undefined) {
      out.push({ t: m[2] ? "key" : "str", v: m[1] });
      if (m[2]) out.push({ t: "punct", v: m[2] });
    } else if (m[3] !== undefined) out.push({ t: "num", v: m[3] });
    else if (m[4] !== undefined) out.push({ t: "lit", v: m[4] });
    else if (m[5] !== undefined) out.push({ t: "punct", v: m[5] });
    else if (m[6] !== undefined) out.push({ t: "ws", v: m[6] });
    else out.push({ t: "punct", v: m[7] });
  }
  return out;
}

export type StepFilter = "all" | "model" | "tool" | "problems";

/** Which attempts a timeline filter keeps. "problems" is anything that stopped, failed or was re-issued. */
export function keepNode(n: GraphNode, f: StepFilter): boolean {
  if (f === "all") return true;
  if (f === "model") return n.type === "model_call";
  if (f === "tool") return n.type === "tool_call";
  return n.state === "stopped" || n.state === "failed" || n.state === "waiting" || n.reissued || (n.type === "model_call" && n.attempts.length > 1 && !!(n.attempts[0].status || n.attempts[0].error_kind));
}
