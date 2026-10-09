import { hopsFor, type Fault, type FaultKind, type PlaygroundResult, type Policy } from "./playground";

export type NodeState = "plan" | "ok" | "fail" | "skip" | "start";

export type SceneNode = {
  id: string;
  label: string;
  /** The provider behind the node, so a click can break it. Absent for the request node. */
  provider?: string;
  state: NodeState;
  /** A fault set on this node's provider, shown as a word beside the node. */
  fault?: FaultKind;
};

export const FAULT_WORD: Record<FaultKind, string> = { rate_limit: "429", server_error: "503", slow: "slow", cut_stream: "cut" };

/** The route a policy would take, before anything is sent: the request, then each model in the order it is tried. */
export function planNodes(policy: Policy | undefined, faults: Fault[]): SceneNode[] {
  const nodes: SceneNode[] = [{ id: "request", label: "Request", state: "start" }];
  if (!policy) return nodes;
  const models = policy.type === "fixed" || policy.type === "cheapest" ? policy.models.slice(0, 1) : policy.models;
  models.forEach((m, i) => {
    const f = faults.find((x) => x.provider === m.provider);
    nodes.push({ id: `m${i}`, label: m.id, provider: m.provider, state: "plan", fault: f?.kind });
  });
  return nodes;
}

/** The route a request actually took, one node per attempt. */
export function runNodes(r: Pick<PlaygroundResult, "policy" | "attempts" | "faults">): SceneNode[] {
  const hops = hopsFor(r);
  return hops.map((h, i) => {
    if (i === 0) return { id: "request", label: "Request", state: "start" as const };
    const a = r.attempts[i - 1];
    const f = r.faults.find((x) => x.provider === a.provider);
    return { id: h.key, label: h.name, provider: a.provider, state: h.tone === "ok" ? "ok" : h.tone === "fail" ? "fail" : "skip", fault: f?.kind } satisfies SceneNode;
  });
}

/** A signature that changes when the scene must be rebuilt, and not when only the clock ticks. */
export const signature = (nodes: SceneNode[]) => nodes.map((n) => `${n.id}:${n.label}:${n.state}:${n.fault ?? ""}`).join("|");

export const HOP_MS = 520;
/** How long the replay of a route takes, so the answer can wait for it. */
export const playMs = (nodes: SceneNode[]) => Math.max(0, nodes.length - 1) * HOP_MS + 350;

/** Clicking a provider cycles: working, 429, 503, slow, working. */
const CYCLE: (FaultKind | null)[] = [null, "rate_limit", "server_error", "slow"];
export function nextFault(current: FaultKind | undefined): FaultKind | null {
  const i = CYCLE.indexOf(current ?? null);
  return CYCLE[(i + 1) % CYCLE.length];
}
