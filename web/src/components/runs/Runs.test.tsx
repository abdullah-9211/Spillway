import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { axe } from "vitest-axe";
import type { RunItem, Snapshot } from "@/lib/runs";
import { RoleProvider } from "@/lib/role";
import type { Role } from "@/lib/api";
import { RunsView } from "./RunsView";
import { NeedsYou, RunCard, RunRow, StepStrip } from "./parts";

const push = vi.fn();
vi.mock("next/navigation", () => ({ useRouter: () => ({ push, replace: vi.fn(), refresh: vi.fn() }) }));

const POLICIES = [{ name: "default", description: "Fallback order: sonnet, mini" }, { name: "cheap-fast", description: "Cheapest model tagged fast" }];
const KEYS = [{ id: "11111111-1111-4111-8111-111111111111", name: "support-agent" }, { id: "22222222-2222-4222-8222-222222222222", name: "research-bot" }];

function view(initial: Snapshot, role: Role = "admin", extra: Partial<React.ComponentProps<typeof RunsView>> = {}) {
  return render(<RoleProvider role={role}><RunsView initial={initial} policies={POLICIES} keys={KEYS} {...extra} /></RoleProvider>);
}

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
    const { container } = view(snap());
    const pulse = screen.getByRole("region", { name: "Activity in the last 24 hours" });
    expect(within(pulse).getByText("Running").nextSibling).toHaveTextContent("3"); // running + sleeping
    expect(within(pulse).getAllByText("Succeeded")[0].parentElement).toHaveTextContent("104");
    expect(within(pulse).getByText("3 cancelled")).toBeInTheDocument();
    expect(screen.getByRole("img", { name: /runs started in the last 24 hours/ })).toBeInTheDocument();
    expect(screen.getByRole("region", { name: "Runs waiting for you" })).toHaveTextContent("Send the Q3 renewal email to Priya");
    expect(screen.getByRole("link", { name: "Review" })).toHaveAttribute("href", "/runs/w1");
    const running = screen.getByRole("region", { name: "Runs in progress" });
    expect(within(running).getAllByRole("button")).toHaveLength(2); // the waiting run is under Needs you, not here
    expect(within(running).getByText("Wakes in 9m")).toBeInTheDocument();
    expect(within(running).getAllByText("Step 4")).toHaveLength(2); // two cards
    const earlier = screen.getByRole("region", { name: "Earlier runs" });
    expect(within(earlier).getByRole("heading", { level: 2 })).toHaveTextContent("Earlier today 118");
    expect(within(earlier).getAllByRole("listitem")).toHaveLength(3);
    expect(within(earlier).getAllByRole("list")).toHaveLength(1); // one day, one group
    expect(await axe(container)).toHaveNoViolations();
  });

  it("says why a run ended, with a shape and a word", () => {
    stubFetch(() => snap());
    view(snap());
    const rows = within(screen.getByRole("list")).getAllByRole("listitem");
    expect(within(rows[0]).getByRole("img", { name: "Succeeded" })).toBeInTheDocument();
    expect(within(rows[1]).getByRole("img", { name: "Failed" })).toBeInTheDocument();
    expect(within(rows[1]).getByText("Step limit")).toBeInTheDocument();
    expect(within(rows[2]).getByRole("img", { name: "Cancelled" })).toBeInTheDocument();
    expect(within(rows[1]).getByRole("img", { name: /32 steps.*1 tool step failed/ })).toBeInTheDocument();
  });

  it("has helpful empty states and no Needs you card when nobody is waiting", () => {
    stubFetch(() => snap());
    view(snap({ active: [], finished: [], waiting: [], counts: { running: 0, sleeping: 0, needs_you: 0, succeeded: 0, failed: 0, cancelled: 0 } }));
    expect(screen.getByText(/No runs yet/)).toBeInTheDocument();
    expect(screen.getByText("New run", { selector: "strong" })).toBeInTheDocument(); // the empty state points at the button
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
    view(snap());
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
    view(snap());
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
    view(snap());
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
    view(snap({ nextCursor: "CUR" }));
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

  it("a card and a row can be selected, and double-clicking opens them", async () => {
    const onSelect = vi.fn();
    const onOpen = vi.fn();
    const u = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    render(<RunCard run={run("r1", "Goal one")} now={NOW} fresh={false} selected={false} onSelect={onSelect} onOpen={onOpen} />);
    await u.click(screen.getByRole("button", { name: /Goal one/ }));
    expect(onSelect).toHaveBeenCalledTimes(1);
    await u.dblClick(screen.getByRole("button", { name: /Goal one/ }));
    expect(onOpen).toHaveBeenCalled();
  });

  it("a finished row is a button too, marked when selected, and says how it ended", async () => {
    const onSelect = vi.fn();
    const u = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    render(<div role="list"><RunRow run={done("d1", "Backfill", { status: "failed", failure_reason: "max_cost" })} now={NOW} tone="" selected onSelect={onSelect} onOpen={() => {}} /></div>);
    const b = screen.getByRole("button", { name: /Backfill/ });
    expect(b).toHaveAttribute("aria-pressed", "true");
    expect(b).toHaveTextContent("Cost limit");
    await u.click(b);
    expect(onSelect).toHaveBeenCalled();
  });

  it("Needs you lists each waiting run with its tool", () => {
    render(<NeedsYou now={NOW} waiting={[{ id: "a", goal: "Delete 14 stale records", tool: "delete_records", waiting_since: "2026-10-09T11:51:00Z", key: "data-sync" }]} />);
    expect(screen.getByText("delete_records")).toHaveClass("mono");
    expect(screen.getByText(/Waiting 9m, data-sync/)).toBeInTheDocument();
  });
});


describe("finding runs", () => {
  const up = () => userEvent.setup({ advanceTimers: vi.advanceTimersByTime });

  it("searches after typing stops, asks the service, and keeps the address in step", async () => {
    const f = stubFetch(() => snap({ finished: [done("d2", "Backfill customer records")], active: [] }));
    const u = up();
    view(snap());
    await u.type(screen.getByRole("searchbox", { name: "Search runs by task" }), "backfill");
    expect(f).not.toHaveBeenCalledWith(expect.stringContaining("q=backfill"), expect.anything()); // not on every key
    await act(async () => {
      await vi.advanceTimersByTimeAsync(400);
    });
    expect(f).toHaveBeenCalledWith("/api/runs/snapshot?hours=24&q=backfill", expect.anything());
    expect(window.location.search).toBe("?q=backfill");
    expect(await screen.findByText("Backfill customer records")).toBeInTheDocument();
    expect(screen.queryByText("Reconcile March invoices")).not.toBeInTheDocument();
  });

  it("filters by status with a chip, and by clicking a counter, and says what is showing", async () => {
    stubFetch(() => snap({ active: [], waiting: [], finished: [done("d2", "Backfill", { status: "failed", failure_reason: "max_steps" })] }));
    const u = up();
    view(snap());
    const chips = screen.getByRole("group", { name: "Show runs that are" });
    await u.click(within(chips).getByRole("button", { name: /^Failed/ }));
    expect(within(chips).getByRole("button", { name: /^Failed/ })).toHaveAttribute("aria-pressed", "true");
    expect(screen.queryByRole("region", { name: "Runs in progress" })).not.toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Runs waiting for you" })).not.toBeInTheDocument();
    expect(window.location.search).toBe("?status=failed");
    // The counter does the same, and a second click clears it.
    await u.click(within(screen.getByRole("region", { name: /Activity in the last/ })).getByRole("button", { name: /Succeeded/ }));
    expect(within(chips).getByRole("button", { name: /^Succeeded/ })).toHaveAttribute("aria-pressed", "true");
    await u.click(within(screen.getByRole("region", { name: /Activity in the last/ })).getByRole("button", { name: /Succeeded/ }));
    expect(within(chips).getByRole("button", { name: /^All/ })).toHaveAttribute("aria-pressed", "true");
  });

  it("filters by API key, and offers to clear every filter", async () => {
    const f = stubFetch(() => snap({ active: [], waiting: [], finished: [] }));
    const u = up();
    view(snap());
    await u.selectOptions(screen.getByLabelText("API key"), KEYS[1].id);
    expect(f).toHaveBeenCalledWith(`/api/runs/snapshot?hours=24&key=${KEYS[1].id}`, expect.anything());
    expect(await screen.findByText(/No runs match/)).toBeInTheDocument();
    await u.click(screen.getAllByRole("button", { name: "Clear filters" })[0]);
    expect(window.location.search).toBe("");
    expect(screen.getByLabelText("API key")).toHaveValue("");
  });

  it("orders the loaded runs and says so", async () => {
    const two = snap({ finished: [done("a", "Short one", { duration_ms: 1000 }), done("b", "Long one", { duration_ms: 90_000 })] });
    stubFetch(() => two);
    const u = up();
    view(two);
    await u.selectOptions(screen.getByLabelText("Order"), "longest");
    const rows = within(screen.getByRole("region", { name: "Earlier runs" })).getAllByRole("listitem");
    expect(within(rows[0]).getByText("Long one")).toBeInTheDocument();
    expect(screen.getByText(/ordering the 2 loaded/)).toBeInTheDocument();
    expect(window.location.search).toBe("?sort=longest");
  });

  it("groups finished runs by day, newest first", () => {
    stubFetch(() => snap());
    view(snap({ finished: [done("a", "Today one", { finished_at: "2026-10-09T10:00:00Z" }), done("b", "Yesterday one", { finished_at: "2026-10-08T10:00:00Z" })] }));
    expect(screen.getByRole("heading", { name: "Today" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Yesterday" })).toBeInTheDocument();
  });

  it("the keys: / goes to search, j and k move through the runs, Escape lets go", async () => {
    stubFetch(() => snap());
    const u = up();
    view(snap());
    await u.keyboard("/");
    expect(screen.getByRole("searchbox")).toHaveFocus();
    await u.keyboard("{Escape}");
    (document.activeElement as HTMLElement).blur();
    await u.keyboard("j");
    const panel = () => screen.getByRole("complementary", { name: "Run details" });
    expect(panel()).toHaveTextContent("Send the Q3 renewal email to Priya"); // the first in the order: what waits for you
    await u.keyboard("jj");
    expect(panel()).toHaveTextContent("Triage billing tickets");
    await u.keyboard("k");
    expect(panel()).toHaveTextContent("Research competitor pricing");
    await u.keyboard("{Escape}");
    expect(panel()).toHaveTextContent("Select a run");
  });
});

describe("looking at a run", () => {
  const up = () => userEvent.setup({ advanceTimers: vi.advanceTimersByTime });

  it("selecting shows its details beside the list, with a way to open it", async () => {
    stubFetch(() => snap());
    const u = up();
    view(snap());
    expect(screen.getByRole("complementary", { name: "Run details" })).toHaveTextContent("Select a run");
    await u.click(screen.getByRole("button", { name: /Research competitor pricing/ }));
    const peek = screen.getByRole("complementary", { name: "Run details" });
    expect(within(peek).getByText("support-agent")).toBeInTheDocument();
    expect(within(peek).getByText("$0.011")).toBeInTheDocument();
    expect(within(peek).getByRole("link", { name: "Open run" })).toHaveAttribute("href", "/runs/r1");
    expect(within(peek).getByRole("img", { name: /4 steps/ })).toBeInTheDocument();
    await u.click(screen.getByRole("button", { name: /Research competitor pricing/ })); // again: let go
    expect(screen.getByRole("complementary", { name: "Run details" })).toHaveTextContent("Select a run");
  });

  it("double-clicking opens the run page", async () => {
    stubFetch(() => snap());
    const u = up();
    view(snap());
    await u.dblClick(screen.getByRole("button", { name: /Research competitor pricing/ }));
    expect(push).toHaveBeenCalledWith("/runs/r1");
  });

  it("an admin cancels a live run after confirming", async () => {
    const f = vi.fn(async (url: string, init?: RequestInit) => ({ ok: true, status: url.includes("/cancel") ? 202 : 200, json: async () => (url.includes("/cancel") ? { id: "r1", status: "running", cancel_requested: true } : snap()), init }));
    vi.stubGlobal("fetch", f);
    const u = up();
    view(snap());
    await u.click(screen.getByRole("button", { name: /Research competitor pricing/ }));
    await u.click(screen.getByRole("button", { name: "Cancel run" }));
    expect(f).not.toHaveBeenCalledWith("/api/runs/r1/cancel", expect.anything()); // asked first
    expect(screen.getByRole("group", { name: "Confirm cancelling" })).toBeInTheDocument();
    await u.click(screen.getByRole("button", { name: "Keep running" }));
    expect(screen.queryByRole("group", { name: "Confirm cancelling" })).not.toBeInTheDocument();
    await u.click(screen.getByRole("button", { name: "Cancel run" }));
    await u.click(screen.getByRole("button", { name: "Yes, cancel it" }));
    expect(f).toHaveBeenCalledWith("/api/runs/r1/cancel", expect.objectContaining({ method: "POST" }));
  });

  it("a viewer can look but not cancel, and a finished run has nothing to cancel", async () => {
    stubFetch(() => snap());
    const u = up();
    view(snap(), "viewer");
    await u.click(screen.getByRole("button", { name: /Research competitor pricing/ }));
    expect(screen.queryByRole("button", { name: "Cancel run" })).not.toBeInTheDocument();
    expect(screen.getByText("Only admins can cancel runs.")).toBeInTheDocument();
    await u.click(screen.getByRole("button", { name: /Reconcile March invoices/ }));
    expect(screen.queryByText("Only admins can cancel runs.")).not.toBeInTheDocument();
    expect(screen.getByRole("complementary", { name: "Run details" })).toHaveTextContent("Reconcile March invoices");
  });
});

describe("starting a run", () => {
  const up = () => userEvent.setup({ advanceTimers: vi.advanceTimersByTime });

  it("an admin opens the form, is told what is missing, and starts a run", async () => {
    const f = vi.fn(async (url: string, init?: RequestInit) => ({ ok: true, status: url === "/api/runs" ? 202 : 200, json: async () => (url === "/api/runs" ? { id: "new-run", status: "queued" } : snap({ active: [run("new-run", "Summarise the open incidents")] })), init }));
    vi.stubGlobal("fetch", f);
    const u = up();
    const { container } = view(snap());
    await u.click(screen.getByRole("button", { name: "New run" }));
    const form = screen.getByRole("region", { name: "Start a run" });
    expect(await axe(container)).toHaveNoViolations();
    await u.click(within(form).getByRole("button", { name: "Start run" }));
    expect(within(form).getByText("Say what the run should do.")).toBeInTheDocument();
    expect(f).not.toHaveBeenCalledWith("/api/runs", expect.anything());
    await u.click(within(form).getByRole("button", { name: "Summarise the open incidents and who owns them" }));
    await u.selectOptions(within(form).getByLabelText("Routing policy"), "cheap-fast");
    expect(within(form).getByText("Cheapest model tagged fast")).toBeInTheDocument();
    await u.click(within(form).getByText("Limits"));
    await u.type(within(form).getByLabelText("Most steps"), "5");
    await u.click(within(form).getByRole("button", { name: "Start run" }));
    const call = f.mock.calls.find((c) => c[0] === "/api/runs");
    expect(JSON.parse(String(call?.[1]?.body))).toEqual({ input: "Summarise the open incidents and who owns them", model: "cheap-fast", limits: { max_steps: 5 } });
    expect(await screen.findByText("Run started.")).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Start a run" })).not.toBeInTheDocument();
    expect(await screen.findByRole("complementary", { name: "Run details" })).toBeInTheDocument();
  });

  it("offers the registered tools, with approval, and sends the choice", async () => {
    const tools = [
      { name: "send_email", kind: "http", description: "Sends an email", requires_approval: true },
      { name: "demo", kind: "mcp", description: "", requires_approval: false, mcp_tools: [{ name: "demo.lookup_order", description: "Looks up an order" }] },
    ];
    const f = vi.fn(async (url: string) => ({
      ok: true,
      status: url === "/api/runs" ? 202 : 200,
      json: async () => (url === "/api/tools" ? { tools } : url === "/api/runs" ? { id: "new-run", status: "queued" } : snap()),
    }));
    vi.stubGlobal("fetch", f);
    const u = up();
    view(snap());
    await u.click(screen.getByRole("button", { name: "New run" }));
    const form = screen.getByRole("region", { name: "Start a run" });
    const group = await within(form).findByRole("group", { name: "Tools the run may call" });
    await u.type(within(form).getByLabelText("What should the run do?"), "refund Dana");
    await u.click(within(group).getByRole("checkbox", { name: "send_email" }));
    await u.click(within(group).getByRole("checkbox", { name: "demo.lookup_order" }));
    expect(within(group).getByText("Asks first (set on the tool)")).toBeInTheDocument(); // a tool that requires approval cannot be unchecked
    await u.click(within(group).getByRole("checkbox", { name: "Ask me first" }));
    await u.click(within(form).getByRole("button", { name: "Start run" }));
    const call = f.mock.calls.find((c) => c[0] === "/api/runs") as unknown as [string, RequestInit];
    expect(JSON.parse(String(call[1].body))).toEqual({ input: "refund Dana", tools: ["send_email", "demo.lookup_order"], approval_required: ["demo.lookup_order"] }); // send_email asks first because the registry says so
  });

  it("shows the service's refusal and keeps what was typed", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => ({ ok: false, status: 400, json: async () => ({ code: "invalid_request", message: "unknown model or policy \"x\"" }) })));
    const u = up();
    view(snap());
    await u.click(screen.getByRole("button", { name: "New run" }));
    await u.type(screen.getByLabelText("What should the run do?"), "do a thing");
    await u.click(screen.getByRole("button", { name: "Start run" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("unknown model or policy");
    expect(screen.getByLabelText("What should the run do?")).toHaveValue("do a thing");
  });

  it("a viewer sees the button disabled, with the reason", () => {
    stubFetch(() => snap());
    view(snap(), "viewer");
    const b = screen.getByRole("button", { name: "New run" });
    expect(b).toBeDisabled();
    expect(b).toHaveAttribute("title", "Only admins can start runs");
  });
});
