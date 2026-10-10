import type { GraphNode, RunGraph } from "./graph";
import { shortSpan } from "./runs";

// --- the replay: the run's history played back as a short animation ---

export type PlanStep = { id: string; start: number; end: number; kind: "start" | "model" | "tool" | "other"; recoveryBefore: boolean };
export type Plan = { total: number; steps: PlanStep[] };

/** Each attempt takes a share of its real duration, kept between a floor and a ceiling so a long run is still watchable. */
export function stepMs(durationMs: number | null): number {
  const d = durationMs ?? 900;
  return Math.min(900, Math.max(350, d * 0.15));
}

export const RECOVERY_PAUSE_MS = 1100;
export const START_MS = 500;

/** The timeline of a replay. A recovery adds a pause where the new worker takes over. */
export function replayPlan(nodes: { id: string; node: GraphNode | null; kind: string; epoch: number }[]): Plan {
  const steps: PlanStep[] = [];
  let t = 0;
  nodes.forEach((n, i) => {
    const prev = nodes[i - 1];
    const recovery = !!prev && n.node !== null && prev.node !== null && prev.epoch !== n.epoch;
    if (recovery) t += RECOVERY_PAUSE_MS;
    const len = n.node ? stepMs(n.node.duration_ms) : START_MS;
    steps.push({ id: n.id, start: t, end: t + len, kind: n.kind === "goal" ? "start" : n.kind === "model" ? "model" : n.kind === "tool" ? "tool" : "other", recoveryBefore: recovery });
    t += len;
  });
  return { total: t, steps };
}

export type Reach = "ahead" | "active" | "done";

/** Where the replay is, for one step. */
export function reach(step: PlanStep, at: number): Reach {
  if (at < step.start) return "ahead";
  if (at < step.end) return "active";
  return "done";
}

/** The step the packet is travelling to, and how far along its way it is (0 to 1), at a moment of the replay. */
export function packetAt(plan: Plan, at: number): { index: number; u: number } {
  if (plan.steps.length === 0) return { index: 0, u: 0 };
  for (let i = 0; i < plan.steps.length; i++) {
    const s = plan.steps[i];
    if (at < s.start) return { index: Math.max(i - 1, 0), u: 1 }; // waiting at the previous node (a recovery pause)
    if (at < s.end) return { index: i, u: Math.min(1, (at - s.start) / Math.max(1, Math.min(420, s.end - s.start))) };
  }
  return { index: plan.steps.length - 1, u: 1 };
}

// --- the live feed ---

export type FeedItem = { key: string; at: number; tone: "run" | "ok" | "fail" | "wait"; text: string };

const kind = (n: GraphNode) => (n.type === "tool_call" ? n.tool || "tool call" : n.type === "model_call" ? n.model || "model call" : n.type.replace("_", " "));

/** A readable log of what happened, from the graph itself, newest first. It is the same whether the page watched it live or opened afterwards. */
export function feedFrom(g: RunGraph): FeedItem[] {
  const items: FeedItem[] = [];
  const t = (iso: string) => new Date(iso).getTime();
  g.nodes.forEach((n) => {
    const at = t(n.started_at);
    const name = kind(n);
    items.push({ key: `s${n.step_no}:${n.epoch}`, at, tone: n.reissued ? "run" : "wait", text: n.reissued ? `Step ${n.step_no} re-issued on ${n.worker} (${name})` : `Step ${n.step_no} started on ${n.worker} (${name})` });
    if (n.state === "finished" && n.duration_ms !== null) items.push({ key: `f${n.step_no}:${n.epoch}`, at: at + n.duration_ms, tone: "ok", text: `Step ${n.step_no} finished in ${shortSpan(n.duration_ms) === "0s" ? `${n.duration_ms} ms` : shortSpan(n.duration_ms)}${n.cache?.startsWith("hit") ? ", from cache" : ""}` });
    if (n.state === "failed") items.push({ key: `x${n.step_no}:${n.epoch}`, at: at + (n.duration_ms ?? 0), tone: "fail", text: `Step ${n.step_no} failed${n.error ? `: ${n.error.slice(0, 80)}` : ""}` });
    if (n.state === "stopped") items.push({ key: `p${n.step_no}:${n.epoch}`, at: at + 1, tone: "fail", text: `Step ${n.step_no} stopped on ${n.worker}: the worker was lost` });
    if (n.type === "model_call" && n.attempts.length > 1 && (n.attempts[0].status || n.attempts[0].error_kind)) {
      const a = n.attempts[0];
      items.push({ key: `b${n.step_no}:${n.epoch}`, at: at + a.latency_ms, tone: "fail", text: `${a.provider}/${a.model} answered ${a.status ?? a.error_kind}: fell back` });
    }
  });
  g.recoveries.forEach((r) => items.push({ key: `r${r.epoch}`, at: t(r.at), tone: "run", text: `${r.to_worker} took over from ${r.from_worker} at lease epoch ${r.epoch}` }));
  if (g.run.finished_at) {
    const why = g.run.failure_reason ? ` (${g.run.failure_reason.replace(/_/g, " ")})` : "";
    items.push({ key: "end", at: t(g.run.finished_at), tone: g.run.status === "succeeded" ? "ok" : "fail", text: `Run ${g.run.status}${why}` });
  }
  items.push({ key: "begin", at: t(g.run.created_at), tone: "wait", text: "Run created" });
  return items.sort((a, b) => b.at - a.at || b.key.localeCompare(a.key));
}

// --- the workers ---

export type WorkerCard = { id: string; epochs: number[]; state: "holding" | "parked" | "stopped" | "done"; note: string };

/** Which worker holds the run now, and what became of the ones before it. */
export function workerCards(g: RunGraph, now: Date): WorkerCard[] {
  const live = ["queued", "running", "waiting_tool", "waiting_human", "sleeping"].includes(g.run.status);
  return g.workers.map((w, i) => {
    const lastNodeOf = [...g.nodes].reverse().find((n) => n.worker === w.id);
    const isCurrent = i === g.workers.length - 1;
    // A run waiting for a person, or asleep, is held by nobody: the worker let go of it, by design.
    if (isCurrent && (g.run.status === "waiting_human" || g.run.status === "sleeping")) {
      return { id: w.id, epochs: w.epochs, state: "parked" as const, note: g.run.status === "sleeping" ? "let go of the run while it sleeps" : "let go of the run while it waits for a decision" };
    }
    if (isCurrent && live) {
      const left = g.run.lease_expires_at ? new Date(g.run.lease_expires_at).getTime() - now.getTime() : 0;
      return { id: w.id, epochs: w.epochs, state: "holding" as const, note: left > 0 ? `lease renews, expires in ${shortSpan(left)}` : "holds the run" };
    }
    if (lastNodeOf?.state === "stopped" || (!isCurrent && g.workers.length > 1)) return { id: w.id, epochs: w.epochs, state: "stopped" as const, note: "lost: its lease expired" };
    return { id: w.id, epochs: w.epochs, state: "done" as const, note: isCurrent ? "finished the run" : "released the run" };
  });
}

// --- numbers that settle rather than jump ---

/** The values an animated number passes through, for tests and for the tween. Eases out. */
export function tweenValue(from: number, to: number, t: number): number {
  const k = Math.min(1, Math.max(0, t));
  const e = 1 - Math.pow(1 - k, 3);
  return from + (to - from) * e;
}
