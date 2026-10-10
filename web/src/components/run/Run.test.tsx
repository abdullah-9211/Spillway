import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { axe } from "vitest-axe";
import type { Role } from "@/lib/api";
import type { GraphNode, RunGraph } from "@/lib/graph";
import { RoleProvider } from "@/lib/role";
import { RunPage } from "./RunPage";

// The three.js stage needs WebGL, which jsdom does not have. The stand-in shows what it was told, and lets a test click a crystal.
vi.mock("./RunStage", () => ({
  STAGE_H: 280,
  RunStage: ({ layout, replayAt, selected, onSelect, onHover }: { layout: { nodes: { id: string; tip: string }[] }; replayAt: number | null; selected: string | null; onSelect: (id: string) => void; onHover: (h: { id: string; x: number; y: number } | null) => void }) => (
    <div data-testid="stage" data-replay={replayAt === null ? "live" : String(replayAt)} data-selected={selected ?? ""}>
      {layout.nodes.filter((n) => n.id !== "goal").map((n) => (
        <button key={n.id} type="button" data-stage-node={n.id} onClick={() => onSelect(n.id)} onPointerEnter={() => onHover({ id: n.id, x: 10, y: 10 })} onPointerLeave={() => onHover(null)} aria-label={`${n.id} in the scene`} />
      ))}
    </div>
  ),
}));

class FakeES {
  static all: FakeES[] = [];
  static CLOSED = 2;
  readyState = 1;
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  listeners = new Map<string, ((e: MessageEvent<string>) => void)[]>();
  closed = false;
  constructor(readonly url: string) {
    FakeES.all.push(this);
  }
  addEventListener(name: string, f: (e: MessageEvent<string>) => void) {
    this.listeners.set(name, [...(this.listeners.get(name) ?? []), f]);
  }
  close() {
    this.closed = true;
    this.readyState = 2;
  }
  emit(name: string, id: number, data = "{}") {
    for (const f of this.listeners.get(name) ?? []) f({ lastEventId: String(id), data } as MessageEvent<string>);
  }
}

function node(o: Partial<GraphNode> & Pick<GraphNode, "step_no" | "type">): GraphNode {
  return { state: "finished", worker: "w-2", epoch: 1, reissued: false, started_at: "2026-10-09T12:00:00Z", duration_ms: 1400, cost_usd: "0.004800", attempts: [], ...o } as GraphNode;
}

const recovered: RunGraph = {
  run: {
    id: "01a121cc-ee68-74de-8171-8af2ee9ad8ff", status: "running", goal: "Research competitor pricing and summarise it into a table", key: "research-bot", model: "default", tools: ["web_search", "fetch_page"],
    step_count: 8, cost_usd: "0.062400", max_steps: 50, max_cost_usd: "1.000000", deadline_seconds: 900, created_at: "2026-10-09T11:56:00Z", finished_at: null,
    deadline_at: "2026-10-09T12:11:00Z", failure_reason: null, cancel_requested: false, lease_owner: "w-4", lease_epoch: 3, lease_expires_at: "2026-10-09T12:00:30Z",
  },
  workers: [{ id: "w-2", epochs: [1] }, { id: "w-4", epochs: [3] }],
  nodes: [
    node({ step_no: 1, type: "model_call", model: "sonnet", provider: "anthropic", message: "I will look up pricing.", tokens: { in: 842, out: 96 }, cache: "miss" }),
    node({ step_no: 2, type: "tool_call", tool: "web_search", arguments: '{"query":"Acme pricing"}', result: "5 results", idempotency_key: "7f3a9c00000000e1", duration_ms: 612 }),
    node({ step_no: 3, type: "model_call", model: "sonnet", provider: "anthropic", attempts: [{ provider: "openai", model: "mini", kind: "primary", latency_ms: 340, status: 503, error_kind: "server" }, { provider: "anthropic", model: "sonnet", kind: "fallback", latency_ms: 2560 }] }),
    node({ step_no: 4, type: "tool_call", tool: "fetch_page", state: "stopped", duration_ms: null, idempotency_key: "4b21d0aaaaaaaa77" }),
    node({ step_no: 4, type: "tool_call", tool: "fetch_page", worker: "w-4", epoch: 3, reissued: true, previous_worker: "w-2", previous_epoch: 1, idempotency_key: "4b21d0aaaaaaaa77", duration_ms: 200, result: "saved result returned" }),
    node({ step_no: 5, type: "tool_call", tool: "web_search", worker: "w-4", epoch: 3, state: "running", duration_ms: 3200 }),
  ],
  recoveries: [{ after_step: 4, from_worker: "w-2", to_worker: "w-4", epoch: 3, at: "2026-10-09T12:00:00Z" }],
  last_event_id: 1842,
};

const ended: RunGraph = { ...recovered, run: { ...recovered.run, status: "succeeded", finished_at: "2026-10-09T12:00:03Z", lease_owner: null }, nodes: recovered.nodes.slice(0, 2) };

beforeEach(() => {
  FakeES.all = [];
  vi.stubGlobal("EventSource", FakeES);
  vi.useFakeTimers({ shouldAdvanceTime: true });
  vi.setSystemTime(new Date("2026-10-09T12:00:00Z"));
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

const show = (g: RunGraph, role: Role = "admin", view: "graph" | "timeline" = "graph") =>
  render(<RoleProvider role={role}><RunPage initial={g} view={view} /></RoleProvider>);

describe("the graph view", () => {
  it("shows the totals, the worker bands and cut, the stopped and re-issued attempts, and passes axe", async () => {
    const { container } = show(recovered);
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Research competitor pricing");
    const kpis = screen.getByRole("region", { name: "Run totals" });
    expect(kpis).toHaveTextContent("8 of 50");
    expect(kpis).toHaveTextContent("$0.062 of $1.000");
    expect(kpis).toHaveTextContent("w-4 recovered once");
    expect(screen.getByText("Lease expired, w-4 took over")).toBeInTheDocument();
    expect(screen.getByText("Worker w-2")).toBeInTheDocument();
    expect(screen.getByText("Worker w-4, lease epoch 3")).toBeInTheDocument();
    expect(screen.getByText("mini 503")).toBeInTheDocument(); // the fallback chip
    expect(screen.getByRole("button", { name: /Step 4, tool call, stopped, on w-2/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Step 4, tool call, finished, re-issued, on w-4/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Step 5, tool call, running/ })).toBeInTheDocument();
    expect(await axe(container)).toHaveNoViolations();
  });

  it("opens on the re-issued step and reads out what happened, tab by tab", async () => {
    const u = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    show(recovered);
    const insp = screen.getByRole("region", { name: "Selected step" });
    expect(insp).toHaveTextContent("Step 4, tool call");
    expect(insp).toHaveTextContent("Re-issued after recovery");
    expect(insp).toHaveTextContent("4b21d0…77");
    expect(insp).toHaveTextContent("Re-issued after w-2 stopped at epoch 1");
    expect(insp).toHaveTextContent("same idempotency key");
    await u.click(within(insp).getByRole("tab", { name: "Output" }));
    expect(within(insp).getByLabelText("Tool result")).toHaveTextContent("saved result returned");
    await u.click(within(insp).getByRole("tab", { name: "Attempts (2)" }));
    expect(insp).toHaveTextContent("First attempt");
    expect(insp).toHaveTextContent("w-2, epoch 1");
    expect(insp).toHaveTextContent("stopped before it finished");
    expect(insp).toHaveTextContent("Second attempt");
    expect(insp).toHaveTextContent("w-4, epoch 3");
    await u.click(within(insp).getByRole("button", { name: /Attempt 1 on w-2/ }));
    expect(screen.getByRole("region", { name: "Selected step" })).toHaveTextContent("Stopped before it finished");
  });

  it("selecting a node shows that step, including a model call's fallback on its Providers tab", async () => {
    const u = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    show(recovered);
    await u.click(screen.getByRole("button", { name: /Step 3, model call/ }));
    const insp = screen.getByRole("region", { name: "Selected step" });
    expect(insp).toHaveTextContent("Step 3, model call");
    await u.click(within(insp).getByRole("tab", { name: "Providers (2)" }));
    expect(insp).toHaveTextContent("openai/mini");
    expect(insp).toHaveTextContent("Failed 503");
    expect(insp).toHaveTextContent("anthropic/sonnet");
    expect(insp).toHaveTextContent("Answered");
    expect(screen.getByRole("button", { name: /Step 3, model call/ })).toHaveAttribute("aria-pressed", "true");
  });

  it("previous and next walk through the attempts", async () => {
    const u = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    show(recovered);
    const insp = () => screen.getByRole("region", { name: "Selected step" });
    expect(insp()).toHaveTextContent("Step 4, tool call");
    await u.click(within(insp()).getByRole("button", { name: "Previous step" }));
    expect(insp()).toHaveTextContent("Stopped before it finished");
    await u.click(within(insp()).getByRole("button", { name: "Previous step" }));
    expect(insp()).toHaveTextContent("Step 3, model call");
    await u.keyboard("{ArrowRight}");
    expect(insp()).toHaveTextContent("Step 4, tool call");
  });

  it("switches to the timeline, which shows the recovery where it happened", async () => {
    const u = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    show(recovered);
    await u.click(screen.getByRole("button", { name: "Timeline" }));
    expect(window.location.search).toBe("?view=timeline");
    const tl = screen.getByRole("region", { name: "Run steps" });
    expect(within(tl).getAllByText(/^Step \d/)).toHaveLength(6);
    expect(within(tl).getByText("Run recovered after a worker stopped")).toBeInTheDocument();
    expect(tl).toHaveTextContent("Worker w-4 claimed the run at lease epoch 3 and re-issued step 4 with the same idempotency key");
    expect(within(tl).getByText("Fell back from openai/mini after a 503")).toBeInTheDocument();
    expect(within(tl).getByText("Same key as the first attempt")).toBeInTheDocument();
    const side = screen.getByRole("complementary", { name: "Run details" });
    expect(side).toHaveTextContent("w-4");
    expect(side).toHaveTextContent("web_search");
    await u.click(screen.getByRole("button", { name: "Graph" }));
    expect(window.location.search).toBe("");
  });

  it("opens the timeline view directly from the address", () => {
    show(ended, "admin", "timeline");
    expect(screen.getByRole("region", { name: "Run timeline" })).toBeInTheDocument();
  });
});

describe("cancelling", () => {
  it("an admin confirms first, and the run is told to cancel", async () => {
    const f = vi.fn(async (url: string) => ({ ok: true, status: 202, json: async () => (url.includes("/graph") ? recovered : { id: "x", status: "running", cancel_requested: true }) }));
    vi.stubGlobal("fetch", f);
    const u = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    show(recovered);
    await u.click(screen.getByRole("button", { name: "Cancel run" }));
    expect(f).not.toHaveBeenCalledWith(expect.stringContaining("/cancel"), expect.anything());
    await u.click(screen.getByRole("button", { name: "Yes, cancel it" }));
    expect(f).toHaveBeenCalledWith(`/api/runs/${recovered.run.id}/cancel`, expect.objectContaining({ method: "POST" }));
  });

  it("a viewer has no cancel button, and a finished run has none either", () => {
    show(recovered, "viewer");
    expect(screen.queryByRole("button", { name: "Cancel run" })).not.toBeInTheDocument();
    expect(screen.getByText("Only admins can cancel")).toBeInTheDocument();
  });

  it("a finished run offers nothing to cancel and opens no stream", () => {
    show(ended);
    expect(screen.queryByRole("button", { name: "Cancel run" })).not.toBeInTheDocument();
    expect(FakeES.all).toHaveLength(0);
    expect(screen.getByText(/Finished, last event 1842/)).toBeInTheDocument();
  });
});

describe("live updates", () => {
  it("a new row on the stream refreshes the graph, so a node appears", async () => {
    const next: RunGraph = { ...recovered, nodes: [...recovered.nodes.slice(0, 5), node({ step_no: 5, type: "tool_call", tool: "web_search", worker: "w-4", epoch: 3, duration_ms: 3500 }), node({ step_no: 6, type: "model_call", model: "sonnet", worker: "w-4", epoch: 3, state: "running", duration_ms: 100 })], last_event_id: 1850 };
    const f = vi.fn(async () => ({ ok: true, status: 200, json: async () => next }));
    vi.stubGlobal("fetch", f);
    show(recovered);
    const es = FakeES.all[0];
    expect(es.url).toBe(`/api/runs/${recovered.run.id}/events?after=1842`);
    act(() => es.onopen?.());
    expect(screen.getByText("Live, event 1842")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Step 6/ })).not.toBeInTheDocument();
    act(() => es.emit("step.finished", 1850));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(200);
    });
    expect(f).toHaveBeenCalledWith(`/api/runs/${recovered.run.id}/graph`, expect.anything());
    expect(screen.getByRole("button", { name: /Step 6, model call, running/ })).toBeInTheDocument();
    expect(screen.getByText("Live, event 1850")).toBeInTheDocument();
  });

  it("a replayed row does not cause another read, and the final status closes the stream", async () => {
    const final: RunGraph = { ...ended, last_event_id: 1900 };
    const f = vi.fn(async () => ({ ok: true, status: 200, json: async () => final }));
    vi.stubGlobal("fetch", f);
    show(recovered);
    const es = FakeES.all[0];
    act(() => es.onopen?.());
    act(() => es.emit("step.started", 1842)); // already seen: the stream started after it
    await act(async () => {
      await vi.advanceTimersByTimeAsync(300);
    });
    expect(f).not.toHaveBeenCalled();
    act(() => es.emit("run.status", 1900, '{"type":"run_status","phase":"finished","payload":{"status":"succeeded"}}'));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(300);
    });
    expect(es.closed).toBe(true);
    expect(screen.getByText(/Finished, last event 1900/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Cancel run" })).not.toBeInTheDocument();
  });

  it("says when it is reconnecting, and falls back to polling if the browser gives up", async () => {
    const f = vi.fn(async () => ({ ok: true, status: 200, json: async () => recovered }));
    vi.stubGlobal("fetch", f);
    show(recovered);
    const es = FakeES.all[0];
    act(() => es.onopen?.());
    act(() => es.onerror?.());
    expect(screen.getByText("Reconnecting…")).toBeInTheDocument();
    es.readyState = 2;
    act(() => es.onerror?.());
    expect(screen.getByText("Updating every few seconds")).toBeInTheDocument();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3500);
    });
    await waitFor(() => expect(f).toHaveBeenCalled());
  });
});


describe("the live stage", () => {
  const up = () => userEvent.setup({ advanceTimers: vi.advanceTimersByTime });

  it("shows the stage with the run's nodes, and clicking one in the scene selects that step", async () => {
    const u = up();
    show(recovered);
    expect(screen.getByRole("region", { name: "Live stage" })).toBeInTheDocument();
    await u.click(screen.getByRole("button", { name: "3:1 in the scene" }));
    expect(screen.getByTestId("stage")).toHaveAttribute("data-selected", "3:1");
    expect(screen.getByRole("region", { name: "Selected step" })).toHaveTextContent("Step 3, model call");
  });

  it("hovering a node shows what it is", async () => {
    const u = up();
    show(recovered);
    await u.hover(screen.getByRole("button", { name: "4:1 in the scene" }));
    expect(screen.getByRole("tooltip")).toHaveTextContent("Step 4, tool call, stopped, on w-2");
    await u.unhover(screen.getByRole("button", { name: "4:1 in the scene" }));
    expect(screen.queryByRole("tooltip")).not.toBeInTheDocument();
  });

  it("replays the run: plays, pauses, scrubs, and goes back to live", async () => {
    const u = up();
    show(recovered);
    expect(screen.getByTestId("stage")).toHaveAttribute("data-replay", "live");
    await u.click(screen.getByRole("button", { name: "Replay this run" }));
    expect(screen.getByTestId("stage")).toHaveAttribute("data-replay", "0");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
    });
    const at = Number(screen.getByTestId("stage").getAttribute("data-replay"));
    expect(at).toBeGreaterThan(500);
    await u.click(screen.getByRole("button", { name: "Pause" }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(500);
    });
    expect(Number(screen.getByTestId("stage").getAttribute("data-replay"))).toBe(at);
    fireEventRange(screen.getByLabelText("Replay position"), 2000);
    expect(screen.getByTestId("stage")).toHaveAttribute("data-replay", "2000");
    await u.selectOptions(screen.getByLabelText("Replay speed"), "4");
    await u.click(screen.getByRole("button", { name: "Back to live" }));
    expect(screen.getByTestId("stage")).toHaveAttribute("data-replay", "live");
    expect(screen.getByRole("button", { name: "Replay this run" })).toBeInTheDocument();
  });

  it("a replay that reaches the end offers to go again", async () => {
    const u = up();
    show(recovered);
    await u.click(screen.getByRole("button", { name: "Replay this run" }));
    await u.selectOptions(screen.getByLabelText("Replay speed"), "4");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(8000);
    });
    expect(screen.getByRole("button", { name: "Again" })).toBeInTheDocument();
  });
});

function fireEventRange(el: HTMLElement, value: number) {
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
  act(() => {
    setter?.call(el, String(value));
    el.dispatchEvent(new Event("input", { bubbles: true }));
  });
}

describe("activity and workers", () => {
  it("lists who holds the run and who was lost, and tells the story newest first", () => {
    show(recovered);
    const side = screen.getByRole("complementary", { name: "Workers and activity" });
    expect(side).toHaveTextContent("w-2");
    expect(side).toHaveTextContent("Lost");
    expect(side).toHaveTextContent("lost: its lease expired");
    expect(side).toHaveTextContent("w-4");
    expect(side).toHaveTextContent("Holding the run");
    const feed = within(side).getByRole("list", { name: "What has happened" });
    const lines = within(feed).getAllByRole("listitem").map((l) => l.textContent ?? "");
    expect(lines.some((l) => l.includes("w-4 took over from w-2 at lease epoch 3"))).toBe(true);
    expect(lines.some((l) => l.includes("Step 4 stopped on w-2: the worker was lost"))).toBe(true);
    expect(lines.at(-1)).toContain("Run created");
  });

  it("marks the moment a run ends in front of you, for a few seconds", async () => {
    const final: RunGraph = { ...ended, last_event_id: 1900 };
    vi.stubGlobal("fetch", vi.fn(async () => ({ ok: true, status: 200, json: async () => final })));
    show(recovered);
    const es = FakeES.all[0];
    act(() => es.onopen?.());
    act(() => es.emit("run.status", 1900, '{"type":"run_status","payload":{"status":"succeeded"}}'));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(300);
    });
    expect(screen.getByText("The run succeeded")).toBeInTheDocument();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(8000);
    });
    expect(screen.queryByText("The run succeeded")).not.toBeInTheDocument();
  });

  it("does not announce a run that was already over when the page opened", () => {
    show(ended);
    expect(screen.queryByText("The run succeeded")).not.toBeInTheDocument();
  });
});


describe("the waterfall and the graph's tools", () => {
  it("shows every attempt along time, and clicking a bar selects that step", async () => {
    const u = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    show(recovered);
    const wf = screen.getByRole("region", { name: "Where the time went" });
    const bars = within(wf).getAllByRole("button");
    expect(bars).toHaveLength(6);
    await u.click(within(wf).getByRole("button", { name: /Step 5, web_search, running/ }));
    expect(screen.getByRole("region", { name: "Selected step" })).toHaveTextContent("Step 5, tool call");
    expect(within(wf).getByRole("button", { name: /Step 5/ })).toHaveAttribute("aria-pressed", "true");
  });

  it("zooms the graph with its buttons", async () => {
    const u = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    show(recovered);
    const tools = screen.getByRole("group", { name: "Zoom" });
    expect(within(tools).getByText("100%")).toBeInTheDocument();
    await u.click(within(tools).getByRole("button", { name: "Zoom in" }));
    expect(within(tools).getByText("115%")).toBeInTheDocument();
    await u.click(within(tools).getByRole("button", { name: "Zoom out" }));
    await u.click(within(tools).getByRole("button", { name: "Zoom out" }));
    expect(within(tools).getByText("90%")).toBeInTheDocument();
  });

  it("filters the timeline to the problems, and expands long output", async () => {
    const u = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    show(recovered, "admin", "timeline");
    const tl = screen.getByRole("region", { name: "Run steps" });
    expect(within(tl).getAllByText(/^Step \d/)).toHaveLength(6);
    await u.click(within(tl).getByRole("button", { name: /Problems and re-issues/ }));
    expect(within(tl).getAllByText(/^Step \d/)).toHaveLength(3); // the stopped attempt, its re-issue, and the fallback
    await u.click(within(tl).getByRole("button", { name: /Tool calls/ }));
    expect(within(tl).getAllByText(/^Step \d/)).toHaveLength(4);
    await u.click(within(tl).getByRole("button", { name: /All steps/ }));
    await u.click(within(tl).getByRole("button", { name: "Select step 4, tool call, attempt on w-4" }));
    expect(screen.getByRole("region", { name: "Run steps" })).toBeInTheDocument();
  });
});
