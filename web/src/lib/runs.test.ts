import { describe, expect, it } from "vitest";
import {
  agoLabel, appendFinished, applySnapshot, bars, counters, durationLabel, firstState, parseHours, reasonLabel, runCost, runDuration, shortSpan,
  statusView, stripClass, stripLabel, type ActivityBucket, type RunItem, type Snapshot, type Strip,
} from "./runs";

const NOW = new Date("2026-10-09T12:00:00Z");

function run(id: string, over: Partial<RunItem> = {}): RunItem {
  return {
    id, status: "succeeded", goal: `goal ${id}`, key: "support-agent", model: "default", step_count: 3, cost_usd: "0.147000",
    created_at: "2026-10-09T11:00:00Z", finished_at: "2026-10-09T11:06:51Z", duration_ms: 411_000, wake_at: null, failure_reason: null,
    strip: { steps: [], more: 0 }, ...over,
  };
}

function snap(over: Partial<Snapshot> = {}): Snapshot {
  return {
    hours: 24, counts: { running: 1, sleeping: 0, needs_you: 0, succeeded: 2, failed: 0, cancelled: 0 }, waiting: [], bucketSeconds: 3600, buckets: [],
    active: [], finished: [], nextCursor: null, ...over,
  };
}

describe("range", () => {
  it("accepts only the three ranges", () => {
    expect([parseHours("1"), parseHours("24"), parseHours("168"), parseHours("5"), parseHours(undefined), parseHours(["1", "24"]), parseHours("")]).toEqual([1, 24, 168, 24, 24, 1, 24]);
  });
});

describe("words and numbers", () => {
  it("names why a run ended", () => {
    expect(["max_steps", "max_cost", "deadline", "provider_failed", "tool_failed", "cancelled", "key_revoked", "weird_new_one", null].map(reasonLabel)).toEqual([
      "Step limit", "Cost limit", "Deadline passed", "Provider failed", "Tool failed", "Cancelled", "Key revoked", "weird new one", "",
    ]);
  });
  it("gives each status a word and a shape", () => {
    const views = (["succeeded", "failed", "cancelled", "running", "waiting_tool", "queued", "waiting_human"] as const).map((s) => statusView({ status: s, wake_at: null }, NOW));
    expect(views.map((v) => v.word)).toEqual(["Succeeded", "Failed", "Cancelled", "Running", "Running", "Queued", "Waiting for approval"]);
    expect(new Set(views.map((v) => v.icon)).size).toBe(6); // running and waiting_tool share the spinner
    expect(statusView({ status: "sleeping", wake_at: "2026-10-09T12:09:00Z" }, NOW).word).toBe("Wakes in 9m");
    expect(statusView({ status: "sleeping", wake_at: null }, NOW).word).toBe("Sleeping");
  });
  it("shows durations in the two biggest units", () => {
    expect([2_000, 59_400, 60_000, 411_000, 41 * 60_000, 3_600_000, 3_900_000, 0, -5].map(durationLabel)).toEqual(["2s", "59s", "1m", "6m 51s", "41m", "1h", "1h 5m", "0s", "0s"]);
  });
  it("shows a short span in one unit", () => {
    expect([20_000, 9 * 60_000, 2 * 3_600_000, 3 * 86_400_000].map(shortSpan)).toEqual(["20s", "9m", "2h", "3d"]);
  });
  it("says how long ago", () => {
    expect([agoLabel("2026-10-09T11:59:40Z", NOW), agoLabel("2026-10-09T11:00:00Z", NOW), agoLabel("2026-10-08T12:00:00Z", NOW), agoLabel(null, NOW)]).toEqual(["just now", "1h ago", "24h ago", ""]);
  });
  it("shows money to three places", () => {
    expect([runCost("0.147000"), runCost("1.002000"), runCost("0.000000"), runCost("x")]).toEqual(["$0.147", "$1.002", "$0.000", "x"]);
  });
  it("uses the clock for a live run and the stored time for a finished one", () => {
    expect(runDuration(run("a", { finished_at: null, created_at: "2026-10-09T11:54:00Z" }), NOW)).toBe("6m");
    expect(runDuration(run("a"), NOW)).toBe("6m 51s");
  });
});

describe("strips", () => {
  const strip: Strip = { steps: [{ kind: "model", state: "done" }, { kind: "tool", state: "done" }, { kind: "model", state: "current" }], more: 0 };
  it("gives each node its shape and state class", () => {
    expect(strip.steps.map(stripClass)).toEqual(["nd m", "nd t", "nd m cur"]);
    expect([{ kind: "tool", state: "failed" }, { kind: "tool", state: "waiting" }].map((n) => stripClass(n as never))).toEqual(["nd t fail", "nd t wait"]);
  });
  it("describes itself in words, so state is never only colour", () => {
    expect(stripLabel(strip)).toBe("3 steps: 2 done, 1 model step running");
    expect(stripLabel({ steps: [{ kind: "tool", state: "waiting" }, { kind: "model", state: "failed" }], more: 0 })).toBe("2 steps: 1 tool step waiting for a person, 1 model step failed");
    expect(stripLabel({ steps: strip.steps, more: 24 })).toBe("27 steps: 26 done, 1 model step running");
    expect(stripLabel({ steps: [], more: 0 })).toBe("No steps yet");
    expect(stripLabel({ steps: [{ kind: "model", state: "done" }], more: 0 })).toBe("1 step: 1 done");
  });
});

describe("activity bars", () => {
  const b = (start: string, s: number, f = 0, c = 0, p = 0): ActivityBucket => ({ start, succeeded: s, failed: f, cancelled: c, in_progress: p });
  it("scales to the tallest bar and never hides a small count", () => {
    const out = bars([b("a", 10), b("b", 1), b("c", 0), b("d", 2, 1, 0, 1)], 3600, 24);
    expect(out.max).toBe(10);
    expect(out.bars.map((x) => Math.round(x.ok))).toEqual([100, 10, 0, 20]);
    expect(out.bars[1].ok).toBeGreaterThanOrEqual(6);
    expect(out.bars[3].fail).toBeGreaterThanOrEqual(6);
    expect(out.bars[2].ok + out.bars[2].fail + out.bars[2].run).toBe(0);
  });
  it("labels the ends and puts the counts in each tooltip", () => {
    const out = bars([b("a", 3), b("b", 2, 1)], 3600, 24);
    expect(out.bars[0].label).toBe("24h ago");
    expect(out.bars[1].label).toBe("now");
    expect(out.bars[1].tip).toBe("now: 2 succeeded, 1 failed, 0 cancelled, 0 in progress");
    expect(out.bars[0].tip.startsWith("2h ago:")).toBe(true);
    expect(out.summary).toBe("6 runs started in the last 24 hours, 5 succeeded");
  });
  it("copes with no data", () => {
    const out = bars([], 3600, 168);
    expect(out.bars).toEqual([]);
    expect(out.summary).toBe("0 runs started in the last 7 days, 0 succeeded");
  });
});

describe("counters", () => {
  it("counts sleeping runs as running and shows cancelled beside failed", () => {
    const c = counters({ running: 4, sleeping: 2, needs_you: 2, succeeded: 104, failed: 11, cancelled: 3 });
    expect(c.map((x) => [x.id, x.value])).toEqual([["running", 6], ["needs", 2], ["ok", 104], ["fail", 11]]);
    expect(c[3].hint).toBe("3 cancelled");
    expect(counters({ running: 0, sleeping: 0, needs_you: 0, succeeded: 0, failed: 0, cancelled: 0 })[3].hint).toBeUndefined();
  });
});

describe("live updates", () => {
  const live = firstState(snap({ active: [run("a", { status: "running", finished_at: null }), run("b", { status: "running", finished_at: null })], finished: [run("old")] }));

  it("knows which runs are new and which have just ended", () => {
    const next = applySnapshot(live, snap({
      active: [run("b", { status: "running", finished_at: null }), run("c", { status: "running", finished_at: null })],
      finished: [run("a"), run("old")],
    }));
    expect([...next.fresh]).toEqual(["c"]);
    expect([...next.ended]).toEqual(["a"]);
    expect(next.snap.finished.map((r) => r.id)).toEqual(["a", "old"]);
  });
  it("does not call anything new when nothing changed", () => {
    const next = applySnapshot(live, live.snap);
    expect(next.fresh.size + next.ended.size).toBe(0);
  });
  it("a run that finished between two looks is new, not ended", () => {
    const next = applySnapshot(live, snap({ active: live.snap.active, finished: [run("quick"), run("old")] }));
    expect([...next.fresh]).toEqual(["quick"]);
    expect(next.ended.size).toBe(0);
  });
  it("changing the range starts over", () => {
    const next = applySnapshot(live, snap({ hours: 168, active: [run("z", { status: "running", finished_at: null })] }));
    expect(next.fresh.size).toBe(0);
    expect(next.snap.hours).toBe(168);
  });
  it("keeps older runs the person already loaded", () => {
    const withMore = appendFinished(live, [run("older1"), run("older2")], "CUR2");
    expect(withMore.snap.finished.map((r) => r.id)).toEqual(["old", "older1", "older2"]);
    expect(withMore.snap.nextCursor).toBe("CUR2");
    const next = applySnapshot(withMore, snap({ active: live.snap.active, finished: [run("new1"), run("old")], nextCursor: "FIRST" }));
    expect(next.snap.finished.map((r) => r.id)).toEqual(["new1", "old", "older1", "older2"]);
    expect(next.snap.nextCursor).toBe("CUR2");
  });
  it("appending never repeats a run", () => {
    const next = appendFinished(live, [run("old"), run("x")], null);
    expect(next.snap.finished.map((r) => r.id)).toEqual(["old", "x"]);
    expect(next.snap.nextCursor).toBeNull();
  });
});
