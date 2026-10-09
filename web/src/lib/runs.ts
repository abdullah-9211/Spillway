import type { components } from "./api-types";

export type RunItem = components["schemas"]["RunItem"];
export type RunCounts = components["schemas"]["RunCounts"];
export type WaitingRun = components["schemas"]["WaitingRun"];
export type ActivityBucket = components["schemas"]["ActivityBucket"];
export type StripNode = components["schemas"]["StripNode"];
export type Strip = components["schemas"]["Strip"];

export type Snapshot = {
  hours: Hours;
  counts: RunCounts;
  waiting: WaitingRun[];
  bucketSeconds: number;
  buckets: ActivityBucket[];
  active: RunItem[];
  finished: RunItem[];
  nextCursor: string | null;
};

// --- the time range ---

export type Hours = 1 | 24 | 168;
export const RANGES: { hours: Hours; label: string; ago: string; finishedHeading: string }[] = [
  { hours: 1, label: "1 hour", ago: "1 hour ago", finishedHeading: "Earlier in the last hour" },
  { hours: 24, label: "24 hours", ago: "24h ago", finishedHeading: "Earlier today" },
  { hours: 168, label: "7 days", ago: "7 days ago", finishedHeading: "Earlier this week" },
];

export function parseHours(v: string | string[] | undefined | null): Hours {
  const s = Array.isArray(v) ? v[0] : v;
  return s === "1" ? 1 : s === "168" ? 168 : 24;
}

export const rangeOf = (h: Hours) => RANGES.find((r) => r.hours === h) ?? RANGES[1];

// --- words for a run's end ---

const REASONS: Record<string, string> = {
  max_steps: "Step limit",
  max_cost: "Cost limit",
  deadline: "Deadline passed",
  provider_failed: "Provider failed",
  tool_failed: "Tool failed",
  cancelled: "Cancelled",
  rejected: "Rejected",
  key_revoked: "Key revoked",
};

export function reasonLabel(reason: string | null | undefined): string {
  if (!reason) return "";
  return REASONS[reason] ?? reason.replace(/_/g, " ");
}

export type Tone = "ok" | "fail" | "cancel" | "run" | "wait" | "sleep";
export type StatusView = { word: string; tone: Tone; icon: string };

const ICON = {
  ok: "M2.5 6.5l2.2 2.2L9.5 3.5",
  fail: "M3 3l6 6M9 3l-6 6",
  cancel: "M2.5 6h7",
  run: "M6 1.5a4.5 4.5 0 1 0 4.5 4.5",
  wait: "M4 2.5v7M8 2.5v7",
  sleep: "M9.5 7A4 4 0 0 1 5 2.5a4 4 0 1 0 4.5 4.5z",
  queued: "M6 3v3l2 1.2",
};

/** A status as a shape and a word, never colour alone. The icon paths are the 12 by 12 ones in docs/design/tokens.json. */
export function statusView(r: Pick<RunItem, "status" | "wake_at">, now: Date = new Date()): StatusView {
  switch (r.status) {
    case "succeeded":
      return { word: "Succeeded", tone: "ok", icon: ICON.ok };
    case "failed":
      return { word: "Failed", tone: "fail", icon: ICON.fail };
    case "cancelled":
      return { word: "Cancelled", tone: "cancel", icon: ICON.cancel };
    case "waiting_human":
      return { word: "Waiting for approval", tone: "wait", icon: ICON.wait };
    case "sleeping":
      return { word: r.wake_at ? `Wakes in ${shortSpan(new Date(r.wake_at).getTime() - now.getTime())}` : "Sleeping", tone: "sleep", icon: ICON.sleep };
    case "queued":
      return { word: "Queued", tone: "sleep", icon: ICON.queued };
    default:
      return { word: "Running", tone: "run", icon: ICON.run };
  }
}

// --- durations, times, money ---

/** 6m 51s, 41m, 2s: the two biggest units that are not zero. */
export function durationLabel(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return s % 60 === 0 ? `${m}m` : `${m}m ${String(s % 60).padStart(2, "0")}s`;
  const h = Math.floor(m / 60);
  return m % 60 === 0 ? `${h}h` : `${h}h ${m % 60}m`;
}

/** One unit, for a short span: 9m, 2h, 3d. */
export function shortSpan(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.round(m / 60);
  return h < 48 ? `${h}h` : `${Math.round(h / 24)}d`;
}

export function agoLabel(iso: string | null | undefined, now: Date = new Date()): string {
  if (!iso) return "";
  const ms = now.getTime() - new Date(iso).getTime();
  if (ms < 45_000) return "just now";
  return `${shortSpan(ms)} ago`;
}

/** Dollars for a run, three places: $0.147. Numbers use Inter with tabular figures, so this is only the text. */
export function runCost(amount: string): string {
  const n = Number(amount);
  if (!Number.isFinite(n)) return amount;
  return "$" + n.toFixed(3);
}

/** How long a run has been going, or took. A live run's clock is the current time. */
export function runDuration(r: Pick<RunItem, "finished_at" | "created_at" | "duration_ms">, now: Date = new Date()): string {
  if (r.finished_at) return durationLabel(r.duration_ms);
  return durationLabel(now.getTime() - new Date(r.created_at).getTime());
}

// --- strips ---

export const stripClass = (n: StripNode): string => `nd ${n.kind === "model" ? "m" : "t"}${n.state === "done" ? "" : ` ${n.state === "current" ? "cur" : n.state === "failed" ? "fail" : "wait"}`}`;

/** The strip in words, for screen readers: "9 steps so far: 4 model and 4 tool steps done, 1 model step running". */
export function stripLabel(s: Strip): string {
  const total = s.steps.length + s.more;
  if (total === 0) return "No steps yet";
  const count = (kind: string, state: string) => s.steps.filter((n) => n.kind === kind && n.state === state).length;
  const parts: string[] = [];
  const done = (k: string) => count(k, "done");
  if (done("model") + done("tool") > 0) parts.push(`${done("model") + done("tool") + s.more} done`);
  const word = { current: "running", failed: "failed", waiting: "waiting for a person" } as const;
  for (const st of ["current", "waiting", "failed"] as const) {
    for (const k of ["model", "tool"]) {
      const n = count(k, st);
      if (n) parts.push(`${n} ${k} ${n === 1 ? "step" : "steps"} ${word[st]}`);
    }
  }
  return `${total} ${total === 1 ? "step" : "steps"}: ${parts.join(", ")}`;
}

// --- the activity chart ---

export type Bar = { key: string; ok: number; fail: number; cancel: number; run: number; tip: string; label: string };

/** Heights are shares of the tallest bar (never zero-height for a non-zero count: at least 6 percent). */
export function bars(buckets: ActivityBucket[], bucketSeconds: number, hours: Hours): { bars: Bar[]; max: number; summary: string } {
  const total = (b: ActivityBucket) => b.succeeded + b.failed + b.cancelled + b.in_progress;
  const max = Math.max(1, ...buckets.map(total));
  const pct = (n: number) => (n === 0 ? 0 : Math.max(6, (n / max) * 100));
  const n = buckets.length;
  const labelAt = new Map<number, string>([[0, rangeOf(hours).ago], [n - 1, "now"]]);
  if (n >= 12) {
    labelAt.set(Math.round((n - 1) / 3), "");
    labelAt.set(Math.round(((n - 1) * 2) / 3), "");
  }
  const out = buckets.map((b, i) => {
    const age = (n - i) * bucketSeconds * 1000;
    const when = i === n - 1 ? "now" : `${shortSpan(age)} ago`;
    const bits = [`${b.succeeded} succeeded`, `${b.failed} failed`, `${b.cancelled} cancelled`, `${b.in_progress} in progress`];
    return { key: b.start, ok: pct(b.succeeded), fail: pct(b.failed), cancel: pct(b.cancelled), run: pct(b.in_progress), tip: `${when}: ${bits.join(", ")}`, label: labelAt.get(i) ?? "" };
  });
  const sum = buckets.reduce((t, b) => t + total(b), 0);
  const ok = buckets.reduce((t, b) => t + b.succeeded, 0);
  return { bars: out, max, summary: `${sum} ${sum === 1 ? "run" : "runs"} started in the last ${rangeOf(hours).label}, ${ok} succeeded` };
}

// --- the counters ---

export type Counter = { id: "running" | "needs" | "ok" | "fail"; label: string; value: number; hint?: string };

/** Running counts what is still going and not waiting on a person: running, queued and sleeping runs. */
export function counters(c: RunCounts): Counter[] {
  return [
    { id: "running", label: "Running", value: c.running + c.sleeping },
    { id: "needs", label: "Needs you", value: c.needs_you },
    { id: "ok", label: "Succeeded", value: c.succeeded },
    { id: "fail", label: "Failed", value: c.failed, hint: c.cancelled > 0 ? `${c.cancelled} cancelled` : undefined },
  ];
}

// --- live updates ---

export type Live = {
  snap: Snapshot;
  /** Runs that appeared since the last update: they animate in. */
  fresh: ReadonlySet<string>;
  /** Runs that were running before and have ended now. */
  ended: ReadonlySet<string>;
};

export function firstState(snap: Snapshot): Live {
  return { snap, fresh: new Set(), ended: new Set() };
}

/**
 * Folds a new snapshot into the screen state. What matters is what changed between two looks: which runs are new, which
 * have just ended. The range changing is a new screen, not an update, so nothing is called new then.
 */
export function applySnapshot(prev: Live, next: Snapshot): Live {
  if (prev.snap.hours !== next.hours) return firstState(next);
  const before = new Set([...prev.snap.active, ...prev.snap.finished].map((r) => r.id));
  const wasActive = new Set(prev.snap.active.map((r) => r.id));
  const fresh = new Set<string>();
  const ended = new Set<string>();
  for (const r of next.active) if (!before.has(r.id)) fresh.add(r.id);
  for (const r of next.finished) {
    if (wasActive.has(r.id)) ended.add(r.id);
    else if (!before.has(r.id)) fresh.add(r.id);
  }
  // Runs the person already loaded with "Load more", and runs the first page has pushed down, stay in the list.
  const inNext = new Set([...next.finished, ...next.active].map((r) => r.id));
  const kept = prev.snap.finished.filter((r) => !inNext.has(r.id));
  const finished = [...next.finished, ...kept].sort((x, y) => Date.parse(y.created_at) - Date.parse(x.created_at));
  return { snap: { ...next, finished, nextCursor: kept.length ? prev.snap.nextCursor : next.nextCursor }, fresh, ended };
}

/** Adds an older page to the end of the list, without repeating a run. */
export function appendFinished(prev: Live, more: RunItem[], nextCursor: string | null): Live {
  const have = new Set(prev.snap.finished.map((r) => r.id));
  return { ...prev, fresh: new Set(), ended: new Set(), snap: { ...prev.snap, finished: [...prev.snap.finished, ...more.filter((r) => !have.has(r.id))], nextCursor } };
}
