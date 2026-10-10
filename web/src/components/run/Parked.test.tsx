import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { axe } from "vitest-axe";
import { approvalView, countdown, waitedLabel, type Approval } from "@/lib/approval";
import type { GraphNode, RunGraph } from "@/lib/graph";
import { layoutGraph } from "@/lib/graph";
import { RoleProvider } from "@/lib/role";
import type { Role } from "@/lib/api";
import { ApprovalCard, SleepCard } from "./Parked";
import { RunPage } from "./RunPage";

vi.mock("./RunStage", () => ({ STAGE_H: 280, RunStage: () => <div data-testid="stage" /> }));

const NOW = new Date("2026-10-09T12:00:00Z");

const emailApproval: Approval = {
  step_no: 2, gate: true, tool: "send_email", since: "2026-10-09T11:56:00Z", reason: "Approval needed to call send_email",
  arguments: JSON.stringify({ to: "dana@example.com", subject: "Your refund has been issued", body: "Hi Dana,\n\nWe have refunded $42.00." }),
};

const asked: Approval = { step_no: 2, gate: false, since: "2026-10-09T11:59:00Z", reason: "I am about to archive 214 inactive accounts. Please confirm.", arguments: "" };

function card(a: Approval, role: Role = "admin", onDecided = vi.fn()) {
  const out = render(
    <RoleProvider role={role}>
      <ApprovalCard runId="01a121cc-ee68-74de-8171-8af2ee9ad8ff" approval={a} isAdmin={role === "admin"} now={NOW} onDecided={onDecided} />
    </RoleProvider>,
  );
  return { ...out, onDecided };
}

let fetchMock: ReturnType<typeof vi.fn>;
beforeEach(() => {
  fetchMock = vi.fn(async () => new Response(JSON.stringify({ id: "x", status: "running" }), { status: 202 }));
  vi.stubGlobal("fetch", fetchMock);
});
afterEach(() => vi.unstubAllGlobals());

describe("approvalView", () => {
  it("shows an email as an email, other calls as their fields, and bad JSON as text", () => {
    expect(approvalView(emailApproval)).toEqual({ kind: "email", to: "dana@example.com", cc: "", subject: "Your refund has been issued", body: "Hi Dana,\n\nWe have refunded $42.00." });
    expect(approvalView({ tool: "delete_records", arguments: '{"table":"accounts","older_than_days":90}' })).toEqual({ kind: "fields", rows: [["table", "accounts"], ["older_than_days", "90"]] });
    expect(approvalView({ tool: "x", arguments: "not json" })).toEqual({ kind: "fields", rows: [["arguments", "not json"]] });
    expect(approvalView({ tool: "x", arguments: "" })).toEqual({ kind: "none" });
    // a recipient with no subject or body is not an email
    expect(approvalView({ tool: "x", arguments: '{"to":"a@b.c"}' }).kind).toBe("fields");
  });

  it("clips very long values", () => {
    const v = approvalView({ tool: "x", arguments: JSON.stringify({ note: "y".repeat(5000) }) });
    expect(v.kind === "fields" && v.rows[0][1].length).toBeLessThan(700);
  });

  it("words the wait and the countdown", () => {
    expect(waitedLabel("2026-10-09T11:56:00Z", NOW)).toBe("4m");
    expect(waitedLabel("2026-10-09T09:30:00Z", NOW)).toBe("2h 30m");
    expect(countdown("2026-10-09T12:09:12Z", NOW)).toBe("9m 12s");
    expect(countdown("2026-10-09T12:00:45Z", NOW)).toBe("45s");
    expect(countdown("2026-10-09T11:00:00Z", NOW)).toBe("0s");
  });
});

describe("the approval card", () => {
  it("shows the email preview, the reason and how long it has waited, and passes axe", async () => {
    const { container } = card(emailApproval);
    const region = screen.getByRole("region", { name: "Approval needed" });
    expect(region).toHaveTextContent("Waiting for your approval");
    expect(region).toHaveTextContent("Wants to call send_email");
    expect(region).toHaveTextContent("4m");
    const mail = within(region).getByRole("figure", { name: "Preview of the email" });
    expect(mail).toHaveTextContent("dana@example.com");
    expect(mail).toHaveTextContent("Your refund has been issued");
    expect(mail).toHaveTextContent("We have refunded $42.00.");
    expect(mail).toHaveTextContent("Nothing has been sent yet");
    expect(await axe(container)).toHaveNoViolations();
  });

  it("when the model asked, shows its reason and no preview", () => {
    card(asked);
    expect(screen.getByRole("region", { name: "Approval needed" })).toHaveTextContent("The model is asking before it goes on");
    expect(screen.getByText(/archive 214 inactive accounts/)).toBeInTheDocument();
    expect(screen.queryByRole("figure")).toBeNull();
  });

  it("approves with the note, over the dashboard's route", async () => {
    const u = userEvent.setup();
    const { onDecided } = card(emailApproval);
    await u.type(screen.getByLabelText(/Note/), "  fine to send ");
    await u.click(screen.getByRole("button", { name: "Approve and continue" }));
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Approved. The run is picking up where it stopped."));
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/runs/01a121cc-ee68-74de-8171-8af2ee9ad8ff/approve");
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body as string)).toEqual({ note: "fine to send" });
    expect(onDecided).toHaveBeenCalled();
  });

  it("will not reject without a reason, and rejects with one", async () => {
    const u = userEvent.setup();
    card(emailApproval);
    await u.click(screen.getByRole("button", { name: "Reject" }));
    expect(fetchMock).not.toHaveBeenCalled();
    expect(screen.getByRole("alert")).toHaveTextContent("Add a reason");
    expect(screen.getByLabelText(/Note/)).toHaveFocus();
    await u.type(screen.getByLabelText(/Note/), "wrong customer");
    expect(screen.queryByRole("alert")).toBeNull();
    await u.click(screen.getByRole("button", { name: "Reject" }));
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Rejected."));
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toMatch(/\/reject$/);
    expect(JSON.parse(init.body as string)).toEqual({ note: "wrong customer" });
  });

  it("a viewer sees the whole card with the decisions disabled and told why", async () => {
    const u = userEvent.setup();
    const { container } = card(emailApproval, "viewer");
    expect(screen.getByRole("figure", { name: "Preview of the email" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Approve and continue" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Reject" })).toBeDisabled();
    expect(screen.getByLabelText(/Note/)).toBeDisabled();
    expect(screen.getByText(/Only admins can approve or reject/)).toBeInTheDocument();
    await u.click(screen.getByRole("button", { name: "Approve and continue" }));
    expect(fetchMock).not.toHaveBeenCalled();
    expect(await axe(container)).toHaveNoViolations();
  });

  it("shows the server's reason when the decision fails, and refreshes if someone else decided first", async () => {
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({ code: "run_not_waiting", message: "The run is not waiting for a decision." }), { status: 409 }));
    const u = userEvent.setup();
    const { onDecided } = card(emailApproval);
    await u.click(screen.getByRole("button", { name: "Approve and continue" }));
    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("not waiting for a decision"));
    expect(onDecided).toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Approve and continue" })).toBeEnabled();
  });
});

describe("the sleeping card", () => {
  it("counts down to the wake time", () => {
    const { container } = render(<SleepCard wakeAt="2026-10-09T12:09:12Z" seconds={600} now={NOW} />);
    expect(screen.getByRole("region", { name: "Sleeping" })).toHaveTextContent("wakes in 9m 12s");
    expect(container).toHaveTextContent("10 minutes");
    expect(container).toHaveTextContent("No worker holds the run");
  });
});

// --- on the run page ---

function node(o: Partial<GraphNode> & Pick<GraphNode, "step_no" | "type">): GraphNode {
  return { state: "finished", worker: "w-1", epoch: 1, reissued: false, started_at: "2026-10-09T11:56:00Z", duration_ms: 900, cost_usd: "0.001000", attempts: [], ...o } as GraphNode;
}

const run = {
  id: "01a121cc-ee68-74de-8171-8af2ee9ad8ff", status: "waiting_human" as const, goal: "Email Dana about her refund", key: "support-bot", model: "default", tools: ["send_email"],
  step_count: 2, cost_usd: "0.002000", max_steps: 50, max_cost_usd: "1.000000", deadline_seconds: 900, created_at: "2026-10-09T11:55:00Z", finished_at: null,
  deadline_at: "2026-10-09T12:10:00Z", failure_reason: null, cancel_requested: false, lease_owner: null, lease_epoch: 1, lease_expires_at: null, wake_at: null,
  approval: emailApproval as RunGraph["run"]["approval"],
};

const waitingGraph: RunGraph = {
  run, workers: [{ id: "w-1", epochs: [1] }], recoveries: [], last_event_id: 9,
  nodes: [
    node({ step_no: 1, type: "model_call", model: "sonnet", provider: "anthropic", message: "Asked for: send_email" }),
    node({ step_no: 2, type: "wait_human", state: "waiting", duration_ms: 240_000, gate: true, tool: "send_email", reason: "Approval needed to call send_email", arguments: emailApproval.arguments }),
  ],
};

class ES {
  addEventListener() {}
  close() {}
  onopen = null;
  onerror = null;
  readyState = 1;
  static CLOSED = 2;
}

describe("the run page of a run that waits", () => {
  beforeEach(() => {
    vi.stubGlobal("EventSource", ES);
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.setSystemTime(NOW);
  });
  afterEach(() => vi.useRealTimers());

  it("shows the approval card, the waiting node and the status, for an admin", async () => {
    const { container } = render(<RoleProvider role="admin"><RunPage initial={waitingGraph} view="graph" /></RoleProvider>);
    expect(screen.getByRole("region", { name: "Approval needed" })).toBeInTheDocument();
    expect(within(screen.getByRole("region", { name: "Run graph" })).getByRole("button", { name: /Step 2, approval, waiting for approval/ })).toBeInTheDocument();
    expect(screen.getAllByText("Waiting for approval").length).toBeGreaterThan(1); // the status tag and the card's title
    expect(screen.getByRole("button", { name: "Approve and continue" })).toBeEnabled();
    expect(await axe(container)).toHaveNoViolations();
  });

  it("shows the viewer the card with the buttons disabled", () => {
    render(<RoleProvider role="viewer"><RunPage initial={waitingGraph} view="graph" /></RoleProvider>);
    expect(screen.getByRole("region", { name: "Approval needed" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Approve and continue" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Reject" })).toBeDisabled();
  });

  it("keeps the decision on the page after the run moves on and the card is gone", async () => {
    const moved: RunGraph = { ...waitingGraph, run: { ...run, status: "running", approval: null, lease_owner: "w-2" }, nodes: [waitingGraph.nodes[0], { ...waitingGraph.nodes[1], state: "finished", decision: "approve", by: "admin" }] };
    fetchMock.mockImplementation(async (url: string) =>
      url.endsWith("/graph") ? new Response(JSON.stringify(moved), { status: 200 }) : new Response(JSON.stringify({ id: "x", status: "running" }), { status: 202 }),
    );
    const u = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    render(<RoleProvider role="admin"><RunPage initial={waitingGraph} view="graph" /></RoleProvider>);
    await u.click(screen.getByRole("button", { name: "Approve and continue" }));
    await waitFor(() => expect(screen.queryByRole("region", { name: "Approval needed" })).toBeNull());
    expect(screen.getByRole("status", { name: "Decision recorded" })).toHaveTextContent("Approved.");
  });

  it("shows a sleeping run's countdown and its sleeping node", () => {
    const g: RunGraph = {
      ...waitingGraph,
      run: { ...run, status: "sleeping", approval: null, wake_at: "2026-10-09T12:09:00Z" },
      nodes: [waitingGraph.nodes[0], node({ step_no: 2, type: "sleep", state: "sleeping", seconds: 600, wake_at: "2026-10-09T12:09:00Z", duration_ms: 60_000 })],
    };
    render(<RoleProvider role="admin"><RunPage initial={g} view="graph" /></RoleProvider>);
    expect(screen.getByRole("region", { name: "Sleeping" })).toHaveTextContent("wakes in 9m 00s");
    expect(within(screen.getByRole("region", { name: "Run graph" })).getByRole("button", { name: /Step 2, sleep, sleeping/ })).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Approval needed" })).toBeNull();
  });

  it("lays a decided approval and a woken sleep out as finished attempts with their words", () => {
    const g: RunGraph = {
      ...waitingGraph,
      run: { ...run, status: "running", approval: null },
      nodes: [
        waitingGraph.nodes[0],
        node({ step_no: 2, type: "wait_human", gate: true, tool: "send_email", decision: "reject", by: "ana", note: "wrong customer" }),
        node({ step_no: 3, type: "sleep", seconds: 30 }),
      ],
    };
    const l = layoutGraph(g);
    const metas = l.nodes.filter((n) => n.id !== "goal").map((n) => `${n.label}/${n.meta}`);
    expect(metas).toEqual(["sonnet/900 ms", "send_email/rejected", "sleep/900 ms"]);
    expect(l.nodes[2].mark).toBe("M3 3l6 6M9 3l-6 6"); // a rejection is marked with the failure shape
  });
});
