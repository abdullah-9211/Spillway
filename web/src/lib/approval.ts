import type { RunGraph } from "./graph";

export type Approval = NonNullable<RunGraph["run"]["approval"]>;

export type ApprovalView =
  | { kind: "email"; to: string; cc: string; subject: string; body: string }
  | { kind: "fields"; rows: [string, string][] }
  | { kind: "none" };

const MAX_VALUE = 600;

const clip = (s: string) => (s.length > MAX_VALUE ? `${s.slice(0, MAX_VALUE)}…` : s);
const text = (v: unknown): string => (typeof v === "string" ? v : v === null || v === undefined ? "" : typeof v === "object" ? JSON.stringify(v) : String(v));
const first = (o: Record<string, unknown>, keys: string[]): string => {
  for (const k of keys) if (typeof o[k] === "string" && (o[k] as string).trim()) return o[k] as string;
  return "";
};

/**
 * What the person is being asked to approve, as something they can read. A call that looks like an email (a recipient
 * and a subject or a body) is shown as one. Anything else is shown as its fields, so nothing is hidden behind JSON.
 */
export function approvalView(a: Pick<Approval, "tool" | "arguments">): ApprovalView {
  const raw = a.arguments;
  if (!raw) return { kind: "none" };
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return { kind: "fields", rows: [["arguments", clip(raw)]] };
  }
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) return { kind: "fields", rows: [["arguments", clip(text(parsed))]] };
  const o = parsed as Record<string, unknown>;
  const to = first(o, ["to", "recipient", "recipients", "email"]);
  const subject = first(o, ["subject", "title"]);
  const body = first(o, ["body", "message", "text", "content"]);
  if (to && (subject || body)) return { kind: "email", to, cc: first(o, ["cc"]), subject, body };
  const rows = Object.entries(o).map(([k, v]): [string, string] => [k, clip(text(v))]);
  return rows.length ? { kind: "fields", rows } : { kind: "none" };
}

/** How long a decision has been waited for: 4m, 2h. */
export function waitedLabel(since: string, now: Date): string {
  const s = Math.max(0, Math.round((now.getTime() - new Date(since).getTime()) / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  return h < 48 ? `${h}h ${m % 60}m` : `${Math.round(h / 24)}d`;
}

/** A countdown to a wake time: 9m 12s, 45s, 2h 5m. */
export function countdown(wakeAt: string, now: Date): string {
  const s = Math.max(0, Math.ceil((new Date(wakeAt).getTime() - now.getTime()) / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${String(s % 60).padStart(2, "0")}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}
