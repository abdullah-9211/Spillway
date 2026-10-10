/** The step-event stream of one run, and what a screen does with each thing that happens on it. */

export const EVENT_NAMES = ["step.started", "step.reissued", "step.finished", "step.failed", "run.status"] as const;
export const TERMINAL = new Set(["succeeded", "failed", "cancelled"]);

export type Phase = "connecting" | "live" | "reconnecting" | "polling" | "ended";
export type LiveState = { phase: Phase; lastId: number };

export type LiveEvent =
  | { type: "open" }
  | { type: "step"; id: number; name: string; status?: string }
  | { type: "error"; closed: boolean }
  | { type: "graph"; status: string; lastEvent: number };

export type Step = { state: LiveState; refetch: boolean; close: boolean };

export const initialLive = (lastId: number, status: string): LiveState => ({ phase: TERMINAL.has(status) ? "ended" : "connecting", lastId });

/**
 * Folds one thing into the stream state. An event is acted on once: the browser resends from Last-Event-ID after a
 * reconnect and a row can arrive twice, so ids at or below the last seen are ignored. A new row means the graph is
 * stale (refetch). The final run.status ends the stream. If the browser gives up on the connection the screen falls back
 * to polling.
 */
export function nextLive(s: LiveState, e: LiveEvent): Step {
  switch (e.type) {
    case "open":
      return { state: s.phase === "ended" ? s : { ...s, phase: "live" }, refetch: false, close: false };
    case "step": {
      if (e.id <= s.lastId) return { state: s, refetch: false, close: false };
      const over = e.name === "run.status" && !!e.status && TERMINAL.has(e.status);
      return { state: { phase: over ? "ended" : s.phase === "connecting" ? "live" : s.phase, lastId: e.id }, refetch: true, close: over };
    }
    case "error":
      if (s.phase === "ended") return { state: s, refetch: false, close: false };
      return { state: { ...s, phase: e.closed ? "polling" : "reconnecting" }, refetch: e.closed, close: false };
    case "graph": {
      const over = TERMINAL.has(e.status);
      return { state: { phase: over ? "ended" : s.phase, lastId: Math.max(s.lastId, e.lastEvent) }, refetch: false, close: over };
    }
  }
}

export function liveLabel(s: LiveState): string {
  switch (s.phase) {
    case "connecting":
      return "Connecting…";
    case "live":
      return s.lastId > 0 ? `Live, event ${s.lastId}` : "Live";
    case "reconnecting":
      return "Reconnecting…";
    case "polling":
      return "Updating every few seconds";
    case "ended":
      return s.lastId > 0 ? `Finished, last event ${s.lastId}` : "Finished";
  }
}

/** Pulls the status out of a run.status event's data, if it carries one. */
export function statusOf(data: string): string | undefined {
  try {
    const d = JSON.parse(data) as { type?: string; payload?: { status?: string } };
    return d.type === "run_status" ? d.payload?.status : undefined;
  } catch {
    return undefined;
  }
}
