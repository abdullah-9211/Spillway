import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { axe } from "vitest-axe";
import type { Role } from "@/lib/api";
import type { Attempt, HistoryItem, PlaygroundResult, PlaygroundState } from "@/lib/playground";
import { RoleProvider } from "@/lib/role";
import { PlaygroundView } from "./PlaygroundView";
import { RouteTrace } from "./RouteTrace";

// The three.js stage needs WebGL, which jsdom does not have. The stand-in reports the replay finished at once and
// lets a test click a provider, as the real stage does.
vi.mock("./RouteScene", () => ({
  RouteScene: ({ nodes, phase, onDone, onNode }: { nodes: { id: string; label: string; provider?: string }[]; phase: string; onDone?: () => void; onNode?: (p: string) => void }) => {
    if (phase === "playing") queueMicrotask(() => onDone?.());
    return (
      <div data-testid="scene" data-phase={phase}>
        {nodes.map((n) => (n.provider && onNode ? <button key={n.id} type="button" onClick={() => onNode(n.provider as string)}>{`stage ${n.label}`}</button> : <span key={n.id}>{n.label}</span>))}
      </div>
    );
  },
}));

const state: PlaygroundState = {
  key: { prefix: "spw_play", monthly_budget_usd: "5.000000", spend_usd: "0.840000", rate_limit_rpm: 20 },
  fault_injection: true,
  policies: [
    { name: "default", type: "fallback", description: "Fallback order: sonnet, mini", models: [{ id: "sonnet", provider: "anthropic" }, { id: "mini", provider: "openai" }], providers: ["anthropic", "openai"], weights: [], tag: "", hedge_after_ms: 0 },
    { name: "cheap-fast", type: "cheapest", description: "Cheapest model tagged fast", models: [{ id: "mini", provider: "openai" }], providers: ["openai"], weights: [], tag: "fast", hedge_after_ms: 0 },
  ],
};

const bad: Attempt = { provider: "anthropic", model: "sonnet", kind: "primary", latency_ms: 212, status: 429, error_kind: "rate_limited", injected: true };
const good: Attempt = { provider: "openai", model: "mini", kind: "fallback", latency_ms: 1060 };

function result(over: Partial<PlaygroundResult> = {}): PlaygroundResult {
  return {
    id: "00000000-0000-4000-8000-000000000001", created_at: "2026-10-08T12:00:00Z", policy: "default", stream: false, answer: "A breaker stops doomed calls.",
    finish_reason: "stop", provider: "openai", model: "mini", cache: "miss", input_tokens: 412, output_tokens: 188, cost_usd: "0.003100", saved_usd: "0.000000",
    latency_ms: 1620, overhead_ms: 4, outcome: "ok", error: null, attempts: [bad, good], faults: [], unused_faults: [], ...over,
  };
}

const past = (over: Partial<HistoryItem> = {}): HistoryItem => ({ ...result(), prompt: "Explain a circuit breaker", system: "", ...over });

afterEach(() => vi.unstubAllGlobals());

const view = (role: Role, history: HistoryItem[] = [past()], cursor: string | null = null) =>
  render(<RoleProvider role={role}><PlaygroundView state={state} history={history} nextCursor={cursor} /></RoleProvider>);

describe("RouteTrace", () => {
  const cases: [string, Attempt[], string[]][] = [
    ["a straight success", [{ ...good, kind: "primary" }], ["Request", "mini"]],
    ["one failure then an answer", [bad, good], ["Request", "sonnet", "mini, fallback"]],
    ["two failures then an answer", [bad, { ...bad, model: "mini", status: 503 }, { ...good, kind: "retry" }], ["Request", "sonnet", "mini", "mini, retry"]],
    ["a stream cut halfway", [{ ...bad, status: undefined, error_kind: "server" }], ["Request", "sonnet"]],
  ];
  it.each(cases)("draws %s", async (_n, attempts, names) => {
    const { container } = render(<RouteTrace result={{ policy: "default", attempts, latency_ms: 900, overhead_ms: 3 }} />);
    const items = within(screen.getByRole("list", { name: "Route" })).getAllByRole("listitem");
    expect(items.map((i) => i.querySelector(".hn")?.textContent)).toEqual(names);
    expect(screen.getByRole("img", { name: /Time spent/ })).toBeInTheDocument();
    expect(await axe(container)).toHaveNoViolations();
  });

  it("says each stop's outcome in words", () => {
    render(<RouteTrace result={{ policy: "default", attempts: [bad, good], latency_ms: 1, overhead_ms: 1 }} />);
    expect(screen.getByLabelText(/sonnet, failed, 429, 212 ms, made to fail on purpose/)).toBeInTheDocument();
    expect(screen.getByLabelText(/mini, fallback, answered/)).toBeInTheDocument();
  });
});

describe("PlaygroundView as an admin", () => {
  it("shows the policies, the chips for the chosen policy, the budget and the recent list, and passes axe", async () => {
    const { container } = view("admin");
    expect(screen.getByRole("button", { name: /default/ })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByText(/\$0\.84 of \$5 used this month/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Anthropic returns 429" })).toBeInTheDocument();
    expect(screen.getByText("Explain a circuit breaker")).toBeInTheDocument();
    expect(await axe(container)).toHaveNoViolations();
  });

  it("changing the policy changes the chips and drops faults for providers off the route", async () => {
    const u = userEvent.setup();
    view("admin");
    await u.click(screen.getByRole("button", { name: "Anthropic returns 429" }));
    expect(screen.getByText("1 fault will be injected")).toBeInTheDocument();
    await u.click(screen.getByRole("button", { name: /cheap-fast/ }));
    expect(screen.queryByRole("button", { name: "Anthropic returns 429" })).not.toBeInTheDocument();
    expect(screen.getByText("No faults")).toBeInTheDocument();
  });

  it("clicking a provider on the stage cycles its fault: 429, 503, slow, then off", async () => {
    const u = userEvent.setup();
    view("admin");
    const kinds = ["Anthropic returns 429", "Anthropic returns 503", "Slow response"];
    for (const k of kinds) {
      await u.click(screen.getByRole("button", { name: "stage sonnet" }));
      expect(screen.getByRole("button", { name: k })).toHaveAttribute("aria-pressed", "true");
    }
    await u.click(screen.getByRole("button", { name: "stage sonnet" }));
    expect(screen.getByText("No faults")).toBeInTheDocument();
  });

  it("Surprise me fills in a prompt and some faults, and Clear faults removes them", async () => {
    const u = userEvent.setup();
    view("admin");
    await u.click(screen.getByRole("button", { name: "Surprise me" }));
    expect((screen.getByLabelText("Prompt") as HTMLTextAreaElement).value).not.toBe("");
    expect(screen.getByText(/will be injected/)).toBeInTheDocument();
    await u.click(screen.getByRole("button", { name: "Clear faults" }));
    expect(screen.getByText("No faults")).toBeInTheDocument();
  });

  it("refuses an empty prompt without calling the service", async () => {
    const f = vi.fn();
    vi.stubGlobal("fetch", f);
    const u = userEvent.setup();
    view("admin");
    await u.click(screen.getByRole("button", { name: "Send" }));
    expect(screen.getByRole("alert")).toHaveTextContent("Write a prompt first.");
    expect(f).not.toHaveBeenCalled();
  });

  it("sends the prompt with the chosen policy and faults, then shows the route, answer and facts, and adds it to Recent", async () => {
    const f = vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => result({ id: "00000000-0000-4000-8000-000000000002" }) });
    vi.stubGlobal("fetch", f);
    const u = userEvent.setup();
    view("admin", []);
    await u.click(screen.getByRole("button", { name: "Anthropic returns 429" }));
    await u.click(screen.getByRole("button", { name: "Write a SQL query for monthly active users" }));
    await u.click(screen.getByRole("button", { name: "Send" }));
    await screen.findByText("A breaker stops doomed calls.");
    expect(JSON.parse(f.mock.calls[0][1].body)).toEqual({ policy: "default", prompt: "Write a SQL query for monthly active users", faults: [{ provider: "anthropic", kind: "rate_limit" }] });
    expect(screen.getByRole("list", { name: "Route" })).toBeInTheDocument();
    expect(within(screen.getByLabelText("Answer and route")).getByText("$0.0031")).toBeInTheDocument();
    expect(screen.getByText(/\$0\.84 of \$5/)).toBeInTheDocument(); // 0.84 + 0.0031 reads as $0.84
    expect(within(screen.getByLabelText("Recent requests")).getByText("Write a SQL query for monthly active users")).toBeInTheDocument();
  });

  it("shows why there is no answer when every provider failed, and a partial answer for a cut stream", async () => {
    const f = vi.fn()
      .mockResolvedValueOnce({ ok: true, status: 200, json: async () => result({ answer: "", model: "", provider: "", error: { code: "all_providers_failed", message: "Every provider failed." }, outcome: "all_providers_failed", attempts: [bad] }) })
      .mockResolvedValueOnce({ ok: true, status: 200, json: async () => result({ id: "00000000-0000-4000-8000-000000000003", answer: "A brea", error: { code: "stream_interrupted", message: "The stream broke." }, outcome: "upstream_error" }) });
    vi.stubGlobal("fetch", f);
    const u = userEvent.setup();
    view("admin", []);
    await u.click(screen.getByRole("button", { name: "Summarise this changelog" }));
    await u.click(screen.getByRole("button", { name: "Send" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("No provider could answer: Every provider failed.");
    await u.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("The answer was cut off: The stream broke."));
    expect(screen.getByText("A brea")).toBeInTheDocument();
  });

  it("shows the service's refusal (budget, rate limit) instead of a result", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false, status: 402, json: async () => ({ code: "budget_exceeded", message: "The playground key has used its monthly budget." }) }));
    const u = userEvent.setup();
    view("admin", []);
    await u.click(screen.getByRole("button", { name: "Summarise this changelog" }));
    await u.click(screen.getByRole("button", { name: "Send" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("used its monthly budget");
    expect(screen.queryByRole("list", { name: "Route" })).not.toBeInTheDocument();
  });

  it("opens a past request from Recent and loads older ones", async () => {
    const f = vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => ({ requests: [past({ id: "00000000-0000-4000-8000-000000000009", prompt: "An older prompt" })], next_cursor: null }) });
    vi.stubGlobal("fetch", f);
    const u = userEvent.setup();
    view("admin", [past()], "CUR");
    await u.click(screen.getByText("Explain a circuit breaker"));
    expect(screen.getByRole("list", { name: "Route" })).toBeInTheDocument();
    await u.click(screen.getByRole("button", { name: "Older requests" }));
    expect(await screen.findByText("An older prompt")).toBeInTheDocument();
    expect(f.mock.calls[0][0]).toBe("/api/playground/history?cursor=CUR");
    expect(screen.queryByRole("button", { name: "Older requests" })).not.toBeInTheDocument();
  });
});

describe("PlaygroundView as a viewer", () => {
  it("can read but not send: prompt, chips, options and Send are disabled and the reason is shown", async () => {
    const { container } = view("viewer");
    expect(screen.getByLabelText("Prompt")).toBeDisabled();
    expect(screen.getByRole("button", { name: "Send" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Options" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Anthropic returns 429" })).toBeDisabled();
    expect(screen.getByText(/only admins can/)).toBeInTheDocument();
    expect(screen.queryByText("Try")).not.toBeInTheDocument();
    expect(screen.getByText("Explain a circuit breaker")).toBeInTheDocument();
    expect(await axe(container)).toHaveNoViolations();
  });
});

describe("PlaygroundView with fault injection off", () => {
  it("offers no chips and says why", () => {
    render(<RoleProvider role="admin"><PlaygroundView state={{ ...state, fault_injection: false }} history={[]} nextCursor={null} /></RoleProvider>);
    expect(screen.queryByRole("group", { name: "Make a provider fail" })).not.toBeInTheDocument();
    expect(screen.getByText(/Fault injection is switched off/)).toBeInTheDocument();
  });
});
