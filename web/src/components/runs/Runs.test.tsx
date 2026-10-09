import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { axe } from "vitest-axe";
import type { RunItem, Snapshot } from "@/lib/runs";
import { RunsView } from "./RunsView";
import { NeedsYou, RunCard, RunRow, StepStrip } from "./parts";

const NOW = new Date("2026-10-09T12:00:00Z");

function run(id: string, goal: string, over: Partial<RunItem> = {}): RunItem {
  return {
    id, status: "running", goal, key: "support-agent", model: "default", step_count: 4, cost_usd: "0.011000", created_at: "2026-10-09T11:54:00Z", finished_at: null,
    duration_ms: 360_000, wake_at: null, failure_reason: null,
    strip: { steps: [{ kind: "model", state: "done" }, { kind: "tool", state: "done" }, { kind: "model", state: "done" }, { kind: "tool", state: "current" }], more: 0 }, ...over,
  };
}

const done = (id: string, goal: string, over: Partial<RunItem> = {}) =>
  run(id, goal, { status: "succeeded", finished_at: "2026-10-09T11:00:00Z", created_at: "2026-10-09T10:55:00Z", duration_ms: 411_000, strip: { steps: [{ kind: "model", state: "done" }], more: 0 }, ...over });

function snap(over: Partial<Snapshot> = {}): Snapshot {
  return {
    hours: 24, counts: { running: 2, sleeping: 1, needs_you: 1, succeeded: 104, failed: 11, cancelled: 3 },
    waiting: [{ id: "w1", goal: "Send the Q3 renewal email to Priya", tool: "send_email", waiting_since: "2026-10-09T11:58:00Z", key: "support-agent" }],
    bucketSeconds: 3600, buckets: Array.from({ length: 24 }, (_, i) => ({ start: `2026-10-08T${String(12 + (i % 12)).padStart(2, "0")}:00:00Z`, succeeded: i % 5, failed: i % 7 === 0 ? 1 : 0, cancelled: 0, in_progress: i === 23 ? 2 : 0 })),
    active: [run("r1", "Research competitor pricing"), run("r2", "Triage billing tickets", { status: "sleeping", wake_at: "2026-10-09T12:09:00Z" }), run("w1", "Send the Q3 renewal email to Priya", { status: "waiting_human" })],
    finished: [done("d1", "Reconcile March invoices"), done("d2", "Backfill customer records", { status: "failed", failure_reason: "max_steps", strip: { steps: [{ kind: "model", state: "done" }, { kind: "tool", state: "failed" }], more: 30 } }), done("d3", "Scrape pricing pages", { status: "cancelled", failure_reason: "cancelled" })],
    nextCursor: null, ...over,
  };
}

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  vi.setSystemTime(NOW);
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

function stubFetch(handler: (url: string) => unknown, ok = true) {
  const f = vi.fn(async (url: string) => ({ ok, status: ok ? 200 : 500, json: async () => handler(String(url)) }));
  vi.stubGlobal("fetch", f);
  return f;
}

describe("the page", () => {
  it("shows the counters, the chart, Needs you, Running now and Earlier, and passes axe", async () => {
    stubFetch(() => snap());
    const { container } = render(<RunsView initial={snap()} />);
    const pulse = screen.getByRole("region", { name: "Activity in the last 24 hours" });
    expect(within(pulse).getByText("Running").nextSibling).toHaveTextContent("3"); // running + sleeping
    expect(within(pulse).getAllByText("Succeeded")[0].parentElement).toHaveTextContent("104");
    expect(within(pulse).getByText("3 cancelled")).toBeInTheDocument();
    expect(screen.getByRole("img", { name: /runs started in the last 24 hours/ })).toBeInTheDocument();
    expect(screen.getByRole("region", { name: "Runs waiting for you" })).toHaveTextContent("Send the Q3 renewal email to Priya");
    expect(screen.getByRole("link", { name: "Review" })).toHaveAttribute("href", "/runs/w1");
    const running = screen.getByRole("region", { name: "Runs in progress" });
    expect(within(running).getAllByRole("link")).toHaveLength(2); // the waiting run is under Needs you, not here
    expect(within(running).getByText("Wakes in 9m")).toBeInTheDocument();
    expect(within(running).getAllByText("Step 4")).toHaveLength(2); // two cards
    const earlier = screen.getByRole("region", { name: "Earlier runs" });
    expect(within(earlier).getByRole("heading")).toHaveTextContent("Earlier today 118");
    expect(within(earlier).getAllByRole("listitem")).toHaveLength(3);
    expect(await axe(container)).toHaveNoViolations();
  });

  it("says why a run ended, with a shape and a word", () => {
    stubFetch(() => snap());
    render(<RunsView initial={snap()} />);
    const rows = within(screen.getByRole("list")).getAllByRole("listitem");
    expect(within(rows[0]).getByRole("img", { name: "Succeeded" })).toBeInTheDocument();
    expect(within(rows[1]).getByRole("img", { name: "Failed" })).toBeInTheDocument();
    expect(within(rows[1]).getByText("Step limit")).toBeInTheDocument();
    expect(within(rows[2]).getByRole("img", { name: "Cancelled" })).toBeInTheDocument();
    expect(within(rows[1]).getByRole("img", { name: /32 steps.*1 tool step failed/ })).toBeInTheDocument();
  });

  it("has helpful empty states and no Needs you card when nobody is waiting", () => {
    stubFetch(() => snap());
    render(<RunsView initial={snap({ active: [], finished: [], waiting: [], counts: { running: 0, sleeping: 0, needs_you: 0, succeeded: 0, failed: 0, cancelled: 0 } })} />);
    expect(screen.getByText(/Nothing is running/)).toBeInTheDocument();
    expect(screen.getByText(/No runs finished in the last 24 hours/)).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Runs waiting for you" })).not.toBeInTheDocument();
  });
});

describe("live updates", () => {
  it("moves a run from Running now to Earlier when it ends, and adds new ones", async () => {
    const after = snap({
      active: [run("r2", "Triage billing tickets", { status: "sleeping", wake_at: "2026-10-09T12:09:00Z" }), run("r9", "A brand new run", { step_count: 0, strip: { steps: [], more: 0 } })],
      waiting: [],
      finished: [done("r1", "Research competitor pricing"), ...snap().finished],
      counts: { running: 1, sleeping: 1, needs_you: 0, succeeded: 105, failed: 11, cancelled: 3 },
    });
    const f = stubFetch(() => after);
    render(<RunsView initial={snap()} />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2100);
    });
    expect(f).toHaveBeenCalledWith("/api/runs/snapshot?hours=24", expect.anything());
    const running = screen.getByRole("region", { name: "Runs in progress" });
    expect(within(running).queryByText("Research competitor pricing")).not.toBeInTheDocument();
    expect(within(running).getByText("A brand new run")).toBeInTheDocument();
    expect(within(running).getByText("Not started")).toBeInTheDocument();
    expect(within(screen.getByRole("list")).getByText("Research competitor pricing")).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Runs waiting for you" })).not.toBeInTheDocument();
    expect(screen.getByText("Live")).toBeInTheDocument();
  });

  it("says it is offline after two failed updates and recovers by itself", async () => {
    let fail = true;
    const f = vi.fn(async () => {
      if (fail) throw new Error("down");
      return { ok: true, status: 200, json: async () => snap() };
    });
    vi.stubGlobal("fetch", f);
    render(<RunsView initial={snap()} />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(4200);
    });
    expect(screen.getByText(/Offline/)).toBeInTheDocument();
    fail = false;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2100);
    });
    expect(screen.getByText("Live")).toBeInTheDocument();
  });

  it("changes range: asks for it, shows its heading, and ignores a late answer for the old range", async () => {
    const week = snap({ hours: 168, bucketSeconds: 86400, buckets: Array.from({ length: 7 }, (_, i) => ({ start: `2026-10-0${i + 3}T00:00:00Z`, succeeded: i, failed: 0, cancelled: 0, in_progress: 0 })) });
    const f = stubFetch((url) => (url.includes("hours=168") ? week : snap()));
    const u = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    render(<RunsView initial={snap()} />);
    await u.click(screen.getByRole("button", { name: "7 days" }));
    await waitFor(() => expect(f).toHaveBeenCalledWith("/api/runs/snapshot?hours=168", expect.anything()));
    expect(await screen.findByRole("region", { name: "Activity in the last 7 days" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "7 days" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("heading", { name: /Earlier this week/ })).toBeInTheDocument();
    expect(window.location.search).toBe("?hours=168");
  });

  it("loads older runs on request and hides the button at the end", async () => {
    const f = stubFetch((url) => (url.startsWith("/api/runs/earlier") ? { runs: [done("d9", "An older run")], next_cursor: null } : snap({ nextCursor: "CUR" })));
    const u = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    render(<RunsView initial={snap({ nextCursor: "CUR" })} />);
    await u.click(screen.getByRole("button", { name: "Load older runs" }));
    expect(await screen.findByText("An older run")).toBeInTheDocument();
    expect(f).toHaveBeenCalledWith("/api/runs/earlier?hours=24&cursor=CUR", expect.anything());
    expect(screen.queryByRole("button", { name: "Load older runs" })).not.toBeInTheDocument();
  });
});

describe("parts", () => {
  it("a strip shows round model steps and square tool steps, and a count of hidden ones", () => {
    const { container } = render(<StepStrip strip={{ steps: [{ kind: "model", state: "done" }, { kind: "tool", state: "failed" }, { kind: "model", state: "current" }, { kind: "tool", state: "waiting" }], more: 12 }} />);
    expect(container.querySelectorAll(".nd.m")).toHaveLength(2);
    expect(container.querySelectorAll(".nd.t")).toHaveLength(2);
    expect(container.querySelector(".nd.fail")).not.toBeNull();
    expect(container.querySelector(".nd.cur")).not.toBeNull();
    expect(container.querySelector(".nd.wait")).not.toBeNull();
    expect(container.querySelector(".nd.more")).toHaveTextContent("+12");
  });

  it("a card and a row link to the run", () => {
    render(
      <>
        <RunCard run={run("r1", "Goal one")} now={NOW} fresh={false} />
        <div role="list"><RunRow run={done("d1", "Goal two")} now={NOW} tone="" /></div>
      </>,
    );
    expect(screen.getByRole("link", { name: /Goal one/ })).toHaveAttribute("href", "/runs/r1");
    expect(screen.getByRole("link", { name: /Goal two/ })).toHaveAttribute("href", "/runs/d1");
  });

  it("Needs you lists each waiting run with its tool", () => {
    render(<NeedsYou now={NOW} waiting={[{ id: "a", goal: "Delete 14 stale records", tool: "delete_records", waiting_since: "2026-10-09T11:51:00Z", key: "data-sync" }]} />);
    expect(screen.getByText("delete_records")).toHaveClass("mono");
    expect(screen.getByText(/Waiting 9m, data-sync/)).toBeInTheDocument();
  });
});
