import { describe, expect, it } from "vitest";
import { initialLive, liveLabel, nextLive, statusOf, type LiveState } from "./live";

const live = (lastId = 10): LiveState => ({ phase: "live", lastId });

describe("the event stream", () => {
  it("starts connecting, or ended if the run is already over", () => {
    expect(initialLive(5, "running")).toEqual({ phase: "connecting", lastId: 5 });
    expect(initialLive(9, "succeeded")).toEqual({ phase: "ended", lastId: 9 });
  });

  it("a new row means the graph is stale; the same row twice is acted on once", () => {
    const a = nextLive(live(10), { type: "step", id: 11, name: "step.started" });
    expect(a).toEqual({ state: { phase: "live", lastId: 11 }, refetch: true, close: false });
    const again = nextLive(a.state, { type: "step", id: 11, name: "step.started" });
    expect(again.refetch).toBe(false);
    expect(nextLive(a.state, { type: "step", id: 7, name: "step.finished" }).refetch).toBe(false); // a replay after a reconnect
  });

  it("the final run.status ends the stream and asks for one last look", () => {
    const r = nextLive(live(10), { type: "step", id: 12, name: "run.status", status: "failed" });
    expect(r).toEqual({ state: { phase: "ended", lastId: 12 }, refetch: true, close: true });
    // A status that is not final does not.
    expect(nextLive(live(10), { type: "step", id: 12, name: "run.status", status: "running" }).close).toBe(false);
  });

  it("opening moves to live, and a drop is reconnecting until the browser gives up, then polling", () => {
    expect(nextLive({ phase: "connecting", lastId: 0 }, { type: "open" }).state.phase).toBe("live");
    const drop = nextLive(live(), { type: "error", closed: false });
    expect(drop).toEqual({ state: { phase: "reconnecting", lastId: 10 }, refetch: false, close: false });
    expect(nextLive(drop.state, { type: "open" }).state.phase).toBe("live");
    const gaveUp = nextLive(drop.state, { type: "error", closed: true });
    expect(gaveUp).toEqual({ state: { phase: "polling", lastId: 10 }, refetch: true, close: false });
  });

  it("an ended stream stays ended", () => {
    const e: LiveState = { phase: "ended", lastId: 20 };
    expect(nextLive(e, { type: "open" }).state.phase).toBe("ended");
    expect(nextLive(e, { type: "error", closed: true })).toEqual({ state: e, refetch: false, close: false });
  });

  it("a refetched graph that shows the run over closes the stream, and moves the cursor forward only", () => {
    expect(nextLive(live(10), { type: "graph", status: "cancelled", lastEvent: 14 })).toEqual({ state: { phase: "ended", lastId: 14 }, refetch: false, close: true });
    expect(nextLive(live(10), { type: "graph", status: "running", lastEvent: 4 }).state).toEqual({ phase: "live", lastId: 10 });
  });

  it("says where it is in words", () => {
    expect([{ phase: "connecting", lastId: 0 }, live(1842), { phase: "reconnecting", lastId: 3 }, { phase: "polling", lastId: 3 }, { phase: "ended", lastId: 9 }].map((s) => liveLabel(s as LiveState))).toEqual([
      "Connecting…", "Live, event 1842", "Reconnecting…", "Updating every few seconds", "Finished, last event 9",
    ]);
  });

  it("reads the status out of an event's data", () => {
    expect(statusOf('{"type":"run_status","phase":"finished","payload":{"status":"succeeded"}}')).toBe("succeeded");
    expect(statusOf('{"type":"model_call","payload":{"status":"x"}}')).toBeUndefined();
    expect(statusOf("not json")).toBeUndefined();
  });
});
