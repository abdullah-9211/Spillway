import { fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { axe } from "vitest-axe";
import type { UsageGroup, UsageSummary } from "@/lib/usage";
import { CacheAndHealth } from "./CacheAndHealth";
import { Kpis } from "./Kpis";
import { ModelTable } from "./ModelTable";
import { UsageChart } from "./UsageChart";
import { UsageControls } from "./UsageControls";

const push = vi.fn();
vi.mock("next/navigation", () => ({ useRouter: () => ({ push }) }));
vi.mock("next/link", () => ({
  default: ({ href, children, scroll, ...rest }: { href: string; children: React.ReactNode; scroll?: boolean }) => {
    void scroll; // a Next-only prop
    return (
      <a href={href} {...rest}>
        {children}
      </a>
    );
  },
}));

const grp = (o: Partial<UsageGroup>): UsageGroup => ({
  key: "x", label: "x", requests: 0, input_tokens: 0, output_tokens: 0, cost_usd: "0.000000", saved_usd: "0.000000", cache_hits: 0, errors: 0,
  timed_requests: 0, p50_ms: null, p95_ms: null, ...o,
});

const models = [
  grp({ key: "sonnet", label: "sonnet", requests: 21480, input_tokens: 31_200_000, output_tokens: 8_900_000, cost_usd: "142.180000", cache_hits: 4500, errors: 86, timed_requests: 16000, p50_ms: 1900, p95_ms: 5200 }),
  grp({ key: "mini", label: "mini", requests: 19730, input_tokens: 14_100_000, output_tokens: 3_800_000, cost_usd: "44.240000", cache_hits: 5700, errors: 217, timed_requests: 14000, p50_ms: 800, p95_ms: 2100 }),
  grp({ key: "local", label: "local", requests: 7000, input_tokens: 2_600_000, output_tokens: 700_000, cost_usd: "0.000000", cache_hits: 980, errors: 14, timed_requests: 6000, p50_ms: 1400, p95_ms: 3600 }),
];

const stat = (label: string, requests: number, cost: string, saved = "0.000000") => ({ label, requests, input_tokens: requests * 100, output_tokens: requests * 10, cost_usd: cost, saved_usd: saved });
const mk = (key: string, series: Record<string, ReturnType<typeof stat>>, o: Partial<UsageGroup> = {}): UsageGroup => {
  const v = Object.values(series);
  const cost = v.reduce((a, x) => a + Number(x.cost_usd), 0).toFixed(6);
  return grp({ key, label: key, requests: v.reduce((a, x) => a + x.requests, 0), input_tokens: v.reduce((a, x) => a + x.input_tokens, 0), output_tokens: v.reduce((a, x) => a + x.output_tokens, 0),
    cost_usd: cost, saved_usd: v.reduce((a, x) => a + Number(x.saved_usd), 0).toFixed(6), series, ...o });
};
const days: UsageGroup[] = [
  mk("2026-09-30", { sonnet: stat("sonnet", 60, "7.100000", "0.500000"), mini: stat("mini", 40, "2.100000"), local: stat("local", 20, "0.000000") }),
  mk("2026-10-01", { sonnet: stat("sonnet", 120, "8.400000"), mini: stat("mini", 80, "2.400000"), local: stat("local", 10, "0.000000") }),
  mk("2026-10-02", {}),
];
const daysByKey: UsageGroup[] = [
  mk("2026-09-30", { k1: stat("support-agent", 100, "6.000000"), k2: stat("ci", 20, "3.200000") }),
  mk("2026-10-01", { k1: stat("support-agent", 150, "10.800000") }),
  mk("2026-10-02", {}),
];

const summary: UsageSummary = {
  range: { from: "2026-09-30", to: "2026-10-02", days: 3 }, group_by: "model", stack: "model", fallbacks_fired: 212, added_latency: { p50_ms: 3.2, p95_ms: 14.1, p99_ms: 29, samples: 480, since: "2026-10-08T00:00:00Z" },
  totals: { requests: 48210, input_tokens: 47_900_000, output_tokens: 13_400_000, cost_usd: "186.420000", saved_usd: "41.070000", saved_share: 0.18, cache_hits: 11180, errors: 317, rejected: 4,
    cache: { miss: 28000, hit_exact: 12000, hit_semantic: 6000, bypass: 2210 } },
  providers: [
    { name: "anthropic", state: "closed", open_remaining_seconds: null, failed_attempts: 0, fallbacks_to: 0 },
    { name: "google", state: "open", open_remaining_seconds: 21, failed_attempts: 5, fallbacks_to: 0 },
    { name: "openai", state: "half_open", open_remaining_seconds: null, failed_attempts: 1, fallbacks_to: 212 },
    { name: "ollama", state: "not_configured", open_remaining_seconds: null, reason: "no URL", failed_attempts: 0, fallbacks_to: 0 },
  ],
  groups: models,
};

describe("Kpis", () => {
  it("shows the five headline figures with their details", () => {
    render(<Kpis summary={summary} />);
    const kpi = (name: string) => screen.getByText(name).closest(".kpi") as HTMLElement;
    expect(within(kpi("Requests")).getByText("48,210")).toBeInTheDocument();
    expect(within(kpi("Requests")).getByText("16,070 a day on average")).toBeInTheDocument();
    expect(within(kpi("Tokens in and out")).getByText("61.3M")).toBeInTheDocument();
    expect(within(kpi("Tokens in and out")).getByText("47.9M in, 13.4M out")).toBeInTheDocument();
    expect(within(kpi("Spend")).getByText("$186.42")).toBeInTheDocument();
    expect(within(kpi("Spend")).getByText("$62.14 a day on average")).toBeInTheDocument();
    expect(within(kpi("Saved by cache")).getByText("$41.07")).toBeInTheDocument();
    expect(within(kpi("Saved by cache")).getByText("18% of what you would have spent")).toBeInTheDocument();
    expect(within(kpi("Added latency, p95")).getByText("14 ms")).toBeInTheDocument();
    expect(within(kpi("Added latency, p95")).getByText(/p50 3 ms, p99 29 ms/)).toBeInTheDocument();
  });

  it("says so when no latency has been measured, and copes with an empty range", () => {
    const empty: UsageSummary = { ...summary, added_latency: null, totals: { ...summary.totals, requests: 0, input_tokens: 0, output_tokens: 0, cost_usd: "0.000000", saved_usd: "0.000000", saved_share: 0 } };
    render(<Kpis summary={empty} />);
    expect(screen.getByText("No requests since the service started")).toBeInTheDocument();
    expect(screen.getByText("0 a day on average")).toBeInTheDocument();
    expect(screen.getAllByText("$0.00").length).toBeGreaterThan(0);
  });
});

describe("UsageChart", () => {
  const renderChart = (keyId = "") => render(<UsageChart dayModel={days} dayKey={daysByKey} keyId={keyId} />);
  const pressed = (name: string) => screen.getByRole("button", { name }).getAttribute("aria-pressed");
  const bars = () => screen.getAllByRole("button").filter((b) => b.className.includes("col"));

  it("has a legend naming each series, including a free model that has no bar", () => {
    renderChart();
    const legend = document.querySelector(".legend") as HTMLElement;
    expect(within(legend).getByRole("button", { name: "sonnet" })).toBeInTheDocument();
    expect(within(legend).getByRole("button", { name: "mini" })).toBeInTheDocument();
    expect(within(legend).getByText("local (free, no bar)")).toBeInTheDocument();
  });

  it("gives every bar a spoken figure, and makes the chart reachable with the keyboard", () => {
    renderChart();
    expect(bars()).toHaveLength(3);
    expect(bars()[0]).toHaveAccessibleName("Sep 30: $9.20 spent (sonnet $7.10, mini $2.10)");
    // One tab stop for the whole chart, then the arrow keys.
    expect(bars().filter((b) => b.getAttribute("tabindex") === "0")).toHaveLength(1);
  });

  it("moves between bars with the arrow keys", async () => {
    renderChart();
    bars()[0].focus();
    await userEvent.keyboard("{ArrowRight}");
    expect(bars()[1]).toHaveFocus();
    await userEvent.keyboard("{End}");
    expect(bars()[2]).toHaveFocus();
    await userEvent.keyboard("{ArrowRight}");
    expect(bars()[2]).toHaveFocus();
    await userEvent.keyboard("{Home}");
    expect(bars()[0]).toHaveFocus();
  });

  it("shows a tooltip with the breakdown on hover and focus, and hides it after", async () => {
    renderChart();
    expect(screen.queryByRole("tooltip")).toBeNull();
    await userEvent.hover(bars()[0]);
    const tip = screen.getByRole("tooltip");
    expect(within(tip).getByText("Sep 30")).toBeInTheDocument();
    expect(within(tip).getByText("$9.20")).toBeInTheDocument();
    expect(within(tip).getByText("sonnet")).toBeInTheDocument();
    expect(within(tip).getByText("$7.10")).toBeInTheDocument();
    expect(within(tip).getByText("120 requests")).toBeInTheDocument();
    await userEvent.unhover(bars()[0]);
    fireEvent.mouseLeave(document.querySelector(".plot") as HTMLElement);
    expect(screen.queryByRole("tooltip")).toBeNull();
  });

  it("pins a bar's details on click, and the details link zooms to that day", async () => {
    renderChart("");
    await userEvent.click(bars()[1]);
    expect(bars()[1]).toHaveAttribute("aria-pressed", "true");
    const detail = screen.getByRole("region", { name: "Details for Oct 1" });
    expect(within(detail).getByText("$10.80")).toBeInTheDocument(); // spend
    expect(within(detail).getByText("210")).toBeInTheDocument(); // requests
    const zoom = within(detail).getByRole("link", { name: "Open only this day" });
    expect(zoom).toHaveAttribute("href", "/usage?range=custom&from=2026-10-01&to=2026-10-01");
    await userEvent.click(within(detail).getByRole("button", { name: "Close" }));
    expect(screen.queryByRole("region", { name: /Details for/ })).toBeNull();
    await userEvent.click(bars()[1]);
    await userEvent.click(bars()[1]);
    expect(screen.queryByRole("region", { name: /Details for/ })).toBeNull(); // clicking again unpins
  });

  it("keeps the key filter in the zoom link", async () => {
    renderChart("11111111-2222-3333-4444-555555555555");
    await userEvent.click(bars()[0]);
    expect(screen.getByRole("link", { name: "Open only this day" }).getAttribute("href")).toContain("key=11111111-2222-3333-4444-555555555555");
  });

  it("hides and shows a series from the legend, and the axis rescales", async () => {
    renderChart();
    const top = () => (document.querySelector(".yax span") as HTMLElement).textContent;
    expect(top()).toBe("$20");
    await userEvent.click(screen.getByRole("button", { name: "sonnet" }));
    expect(pressed("sonnet")).toBe("false");
    expect(top()).toBe("$2.50");
    expect(bars()[0]).toHaveAccessibleName("Sep 30: $2.10 spent");
    await userEvent.click(screen.getByRole("button", { name: "Show all" }));
    expect(pressed("sonnet")).toBe("true");
    expect(top()).toBe("$20");
  });

  it("says when every series is hidden", async () => {
    renderChart();
    await userEvent.click(screen.getByRole("button", { name: "sonnet" }));
    await userEvent.click(screen.getByRole("button", { name: "mini" }));
    expect(screen.getByText("Every series is hidden.")).toBeInTheDocument();
  });

  it("switches what is plotted", async () => {
    renderChart();
    await userEvent.click(screen.getByRole("button", { name: "Requests" }));
    expect(pressed("Requests")).toBe("true");
    expect(bars()[0]).toHaveAccessibleName(/^Sep 30: 120 requests \(sonnet 60, mini 40, other models 20\)/);
    await userEvent.click(screen.getByRole("button", { name: "Tokens" }));
    expect(bars()[1]).toHaveAccessibleName(/^Oct 1: 23\.1K tokens/);
    await userEvent.click(screen.getByRole("button", { name: "Saved by cache" }));
    expect(bars()[0]).toHaveAccessibleName("Sep 30: $0.50 saved by cache");
    expect(screen.getByRole("button", { name: "Saved by cache" })).toHaveAttribute("aria-pressed", "true");
  });

  it("clears hidden series and the pinned bar when what is plotted changes", async () => {
    renderChart();
    await userEvent.click(screen.getByRole("button", { name: "sonnet" }));
    await userEvent.click(bars()[0]);
    await userEvent.click(screen.getByRole("button", { name: "Requests" }));
    expect(screen.queryByRole("region", { name: /Details for/ })).toBeNull();
    expect(screen.queryByRole("button", { name: "Show all" })).toBeNull();
  });

  it("keeps the legend and the pinned details short when there are very many keys", async () => {
    const lots = (key: string): UsageGroup => {
      const series: Record<string, ReturnType<typeof stat>> = { big: stat("demo-data", 100, "5.000000") };
      for (let i = 0; i < 40; i++) series[`d${i}`] = stat("usage-test", 1, "0.030000"); // 40 keys with one name
      for (let i = 0; i < 25; i++) series[`z${i}`] = stat("usage-fail", 1, "0.000000"); // 25 keys with no spend
      return mk(key, series);
    };
    const d = [lots("2026-10-01"), lots("2026-10-02")];
    render(<UsageChart dayModel={d} dayKey={d} keyId="" />);
    await userEvent.click(screen.getByRole("button", { name: "By key" }));
    const legend = document.querySelector(".legend") as HTMLElement;
    expect(within(legend).getAllByText(/usage-fail/)).toHaveLength(1); // one entry, not 25
    expect(within(legend).getByText("usage-fail (no spend)")).toBeInTheDocument();
    await userEvent.click(bars()[0]);
    const detail = screen.getByRole("region", { name: "Details for Oct 1" });
    expect(within(detail).getAllByRole("listitem").length).toBeLessThanOrEqual(5);
    expect(within(detail).getByText("usage-test")).toBeInTheDocument();
    expect(within(detail).getByText("×40")).toBeInTheDocument();
    expect(within(detail).getByText("25 with none")).toBeInTheDocument();
  });

  it("splits by API key instead of by model", async () => {
    renderChart();
    await userEvent.click(screen.getByRole("button", { name: "By key" }));
    expect(pressed("By key")).toBe("true");
    const legend = document.querySelector(".legend") as HTMLElement;
    expect(within(legend).getByRole("button", { name: "support-agent" })).toBeInTheDocument();
    expect(within(legend).getByRole("button", { name: "ci" })).toBeInTheDocument();
    expect(bars()[0]).toHaveAccessibleName("Sep 30: $9.20 spent (support-agent $6.00, ci $3.20)");
  });

  it("cannot split by key when already filtered to one key", () => {
    renderChart("some-key");
    expect(screen.getByRole("button", { name: "By key" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "By model" })).toBeDisabled();
  });

  it("groups into weeks, and is only offered when there is enough to group", async () => {
    const { unmount } = renderChart();
    expect(screen.getByRole("button", { name: "Weekly" })).toBeDisabled(); // 3 days
    unmount();
    const long = Array.from({ length: 20 }, (_, i) => mk(`2026-09-${String(10 + i).padStart(2, "0")}`.replace("2026-09-3", "2026-09-3"), { sonnet: stat("sonnet", 10, "1.000000") }));
    const valid = long.filter((d) => Number(d.key.slice(8)) <= 30);
    render(<UsageChart dayModel={valid} dayKey={valid} keyId="" />);
    expect(bars()).toHaveLength(valid.length);
    await userEvent.click(screen.getByRole("button", { name: "Weekly" }));
    expect(bars().length).toBeLessThan(valid.length);
    expect(bars()[0].getAttribute("aria-label")).toMatch(/^Sep 10 to Sep 13: \$4\.00 spent/);
  });

  it("switches to a table whose totals match the figures", async () => {
    renderChart();
    await userEvent.click(screen.getByRole("button", { name: "View as table" }));
    const table = screen.getByRole("table");
    const rows = within(table).getAllByRole("row");
    expect(rows).toHaveLength(1 + 3 + 1);
    const total = within(rows[rows.length - 1]).getAllByRole("cell").map((c) => c.textContent);
    expect(total).toEqual(["$15.50", "$4.50", "$20.00", "330"]); // sonnet, mini, total, requests
    await userEvent.click(screen.getByRole("button", { name: "Requests" }));
    expect(within(table).getAllByRole("row")[1]).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "View as chart" }));
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("says when nothing was spent", () => {
    render(<UsageChart dayModel={[mk("2026-10-01", {}), mk("2026-10-02", {})]} dayKey={[]} keyId="" />);
    expect(screen.getByText("No spend in this range.")).toBeInTheDocument();
  });

  it("has no accessibility violations: chart, pinned, table, by key", async () => {
    const { container } = renderChart();
    expect(await axe(container)).toHaveNoViolations();
    await userEvent.click(bars()[0]);
    expect(await axe(container)).toHaveNoViolations();
    await userEvent.click(screen.getByRole("button", { name: "By key" }));
    expect(await axe(container)).toHaveNoViolations();
    await userEvent.click(screen.getByRole("button", { name: "View as table" }));
    expect(await axe(container)).toHaveNoViolations();
  });
});

describe("CacheAndHealth", () => {
  it("splits outcomes into percentages that add up to 100", () => {
    render(<CacheAndHealth summary={summary} />);
    // 30,210 miss or not cached, 12,000 exact, 6,000 semantic of 48,210
    const row = (label: string) => screen.getByText(label).closest(".row2") as HTMLElement;
    const pct = (label: string) => Number(row(label).textContent!.match(/(\d+)%/)![1]);
    expect(pct("Miss or not cached") + pct("Exact hit") + pct("Semantic hit, striped")).toBe(100);
    expect(pct("Miss or not cached")).toBe(63);
    expect(screen.getByRole("img", { name: /63 percent misses or not cached, 25 percent exact hits, 12 percent semantic hits/ })).toBeInTheDocument();
  });

  it("shows each provider's state as a shape and words", () => {
    render(<CacheAndHealth summary={summary} />);
    const prov = (n: string) => screen.getByText(n, { selector: ".brk > span:first-child" }).closest(".brk") as HTMLElement;
    expect(within(prov("anthropic")).getByText("Breaker closed")).toBeInTheDocument();
    expect(within(prov("google")).getByText("Breaker open for 21s")).toBeInTheDocument();
    expect(within(prov("openai")).getByText("Half open, probing")).toBeInTheDocument();
    expect(within(prov("ollama")).getByText("Not configured")).toBeInTheDocument();
    for (const n of ["anthropic", "google", "openai", "ollama"]) expect(prov(n).querySelector(".st svg path")).not.toBeNull();
    expect(within(prov("google")).getByText("5 failed calls")).toBeInTheDocument();
    expect(within(prov("openai")).getByText("1 failed call")).toBeInTheDocument();
    expect(screen.getByText("Fallbacks fired: 212")).toBeInTheDocument();
  });

  it("copes with no requests and no providers", () => {
    render(<CacheAndHealth summary={{ ...summary, providers: [], totals: { ...summary.totals, cache: { miss: 0, hit_exact: 0, hit_semantic: 0, bypass: 0 } } }} />);
    expect(screen.getByText("No requests in this range.")).toBeInTheDocument();
    expect(screen.getByText("No providers are configured.")).toBeInTheDocument();
  });

  it("has no accessibility violations", async () => {
    const { container } = render(<CacheAndHealth summary={summary} />);
    expect(await axe(container)).toHaveNoViolations();
  });
});

describe("ModelTable", () => {
  const names = () => within(screen.getAllByRole("table")[0].querySelector("tbody") as HTMLElement).getAllByRole("row").map((r) => within(r).getByRole("rowheader").textContent);
  const head = (n: string) => screen.getByRole("columnheader", { name: new RegExp(`^${n}`) });

  it("lists models with latency, cache hit and error rates, highest spend first", () => {
    render(<ModelTable models={models} />);
    expect(names()).toEqual(["sonnet", "mini", "local"]);
    const row = screen.getByRole("rowheader", { name: "sonnet" }).closest("tr") as HTMLElement;
    expect(within(row).getAllByRole("cell").map((c) => c.textContent)).toEqual(["21,480", "31.2M", "8.9M", "$142.18", "1.9s", "5.2s", "21%", "0.4%"]);
    expect(head("Cost")).toHaveAttribute("aria-sort", "descending");
    expect(head("Model")).toHaveAttribute("aria-sort", "none");
  });

  it("sorts by any column, and a second click reverses it", async () => {
    render(<ModelTable models={models} />);
    await userEvent.click(within(head("Model")).getByRole("button"));
    expect(names()).toEqual(["local", "mini", "sonnet"]); // names start A to Z
    expect(head("Model")).toHaveAttribute("aria-sort", "ascending");
    await userEvent.click(within(head("Model")).getByRole("button"));
    expect(names()).toEqual(["sonnet", "mini", "local"]);
    expect(head("Model")).toHaveAttribute("aria-sort", "descending");

    await userEvent.click(within(head("Requests")).getByRole("button"));
    expect(names()).toEqual(["sonnet", "mini", "local"].sort((a, b) => ({ sonnet: 21480, mini: 19730, local: 7000 }[b]!) - ({ sonnet: 21480, mini: 19730, local: 7000 }[a]!))); // numbers start highest first
    await userEvent.click(within(head("Requests")).getByRole("button"));
    expect(names()).toEqual(["local", "mini", "sonnet"]);

    await userEvent.click(within(head("p95")).getByRole("button"));
    expect(names()).toEqual(["sonnet", "local", "mini"]);
    await userEvent.click(within(head("Errors")).getByRole("button"));
    expect(names()).toEqual(["mini", "sonnet", "local"]); // 1.1% mini, 0.4% sonnet, 0.2% local
    expect(screen.getByRole("status")).toHaveTextContent("sorted by errors, highest first");
  });

  it("filters by name and totals only what is shown", async () => {
    render(<ModelTable models={models} />);
    await userEvent.type(screen.getByRole("searchbox", { name: "Filter models" }), "mini");
    expect(names()).toEqual(["mini"]);
    expect(screen.getByRole("status")).toHaveTextContent("1 of 3 models");
    const foot = screen.getByRole("rowheader", { name: "Total of those shown" }).closest("tr") as HTMLElement;
    expect(within(foot).getAllByRole("cell")[0]).toHaveTextContent("19,730");
    await userEvent.clear(screen.getByRole("searchbox", { name: "Filter models" }));
    await userEvent.type(screen.getByRole("searchbox", { name: "Filter models" }), "zzz");
    expect(screen.getByText("No model matches “zzz”.")).toBeInTheDocument();
  });

  it("shows a dash instead of a latency it never measured, and sorts it last", async () => {
    render(<ModelTable models={[...models, grp({ key: "idle", label: "idle", requests: 4, cache_hits: 4 })]} />);
    const cells = within(screen.getByRole("rowheader", { name: "idle" }).closest("tr") as HTMLElement).getAllByRole("cell").map((c) => c.textContent);
    expect(cells.slice(4, 6)).toEqual(["—", "—"]);
    await userEvent.click(within(head("p50")).getByRole("button"));
    expect(names().at(-1)).toBe("idle");
    await userEvent.click(within(head("p50")).getByRole("button"));
    expect(names().at(-1)).toBe("idle");
  });

  it("has a totals row that adds up exactly", () => {
    render(<ModelTable models={models} />);
    const foot = screen.getByRole("rowheader", { name: "Total" }).closest("tr") as HTMLElement;
    const cells = within(foot).getAllByRole("cell").map((c) => c.textContent);
    expect(cells[0]).toBe("48,210");
    expect(cells[3]).toBe("$186.42");
  });

  it("explains an empty range, and has no accessibility violations", async () => {
    const { container, rerender } = render(<ModelTable models={models} />);
    expect(await axe(container)).toHaveNoViolations();
    rerender(<ModelTable models={[]} />);
    expect(screen.getByText("No requests in this range.")).toBeInTheDocument();
  });
});

describe("UsageControls", () => {
  const keys = [{ id: "k1", name: "support-agent" }, { id: "k2", name: "ci (revoked)" }];
  const base = { preset: "14d" as const, from: "2026-09-25", to: "2026-10-08", today: "2026-10-08", keyId: "", keys, exportQuery: "from=2026-09-25&to=2026-10-08" };

  it("marks the current range and keeps the key when the range changes", () => {
    render(<UsageControls {...base} keyId="k1" />);
    expect(screen.getByRole("link", { name: "14 days" })).toHaveAttribute("aria-current", "true");
    expect(screen.getByRole("link", { name: "7 days" })).not.toHaveAttribute("aria-current");
    expect(screen.getByRole("link", { name: "7 days" })).toHaveAttribute("href", "/usage?range=7d&key=k1");
    expect(screen.getByRole("link", { name: "30 days" })).toHaveAttribute("href", "/usage?range=30d&key=k1");
    expect(screen.getByRole("link", { name: "90 days" })).toHaveAttribute("href", "/usage?range=90d&key=k1");
  });

  it("offers longer ranges in a menu, and highlights it when one is chosen", async () => {
    push.mockClear();
    const { rerender } = render(<UsageControls {...base} />);
    const more = screen.getByRole("combobox", { name: "More ranges" });
    expect(within(more).getAllByRole("option").map((o) => o.textContent)).toEqual(["More ranges", "This month", "Last month", "Last 6 months", "Last 12 months", "Custom range…"]);
    await userEvent.selectOptions(more, "Last month");
    expect(push).toHaveBeenCalledWith("/usage?range=last-month", { scroll: false });
    rerender(<UsageControls {...base} preset="last-month" />);
    expect(screen.getByRole("combobox", { name: "More ranges" })).toHaveValue("last-month");
    for (const l of ["7 days", "14 days", "30 days", "90 days"]) expect(screen.getByRole("link", { name: l })).not.toHaveAttribute("aria-current");
  });

  it("opens date fields for a custom range, validates them, and applies a good one", async () => {
    push.mockClear();
    render(<UsageControls {...base} />);
    expect(screen.queryByRole("form", { name: "Custom range" })).toBeNull();
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "More ranges" }), "Custom range…");
    const form = screen.getByRole("form", { name: "Custom range" });
    expect(within(form).getByLabelText("Last day")).toHaveAttribute("max", "2026-10-08");

    fireEvent.change(within(form).getByLabelText("First day"), { target: { value: "2026-10-09" } });
    expect(within(form).getByRole("alert")).toHaveTextContent("The first day is after the last day.");
    expect(within(form).getByRole("button", { name: "Apply" })).toBeDisabled();

    fireEvent.change(within(form).getByLabelText("First day"), { target: { value: "2026-08-01" } });
    fireEvent.change(within(form).getByLabelText("Last day"), { target: { value: "2026-08-31" } });
    expect(within(form).queryByRole("alert")).toBeNull();
    await userEvent.click(within(form).getByRole("button", { name: "Apply" }));
    expect(push).toHaveBeenCalledWith("/usage?range=custom&from=2026-08-01&to=2026-08-31", { scroll: false });
  });

  it("starts with the custom fields open and filled in when the range is custom", () => {
    render(<UsageControls {...base} preset="custom" from="2026-08-01" to="2026-08-31" />);
    const form = screen.getByRole("form", { name: "Custom range" });
    expect(within(form).getByLabelText("First day")).toHaveValue("2026-08-01");
    expect(within(form).getByLabelText("Last day")).toHaveValue("2026-08-31");
  });

  it("changes the key through the URL and keeps the range, including a custom one", async () => {
    push.mockClear();
    const { rerender } = render(<UsageControls {...base} preset="7d" />);
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "API key" }), "support-agent");
    expect(push).toHaveBeenCalledWith("/usage?range=7d&key=k1", { scroll: false });
    rerender(<UsageControls {...base} preset="custom" from="2026-08-01" to="2026-08-31" />);
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "API key" }), "support-agent");
    expect(push).toHaveBeenLastCalledWith("/usage?range=custom&from=2026-08-01&to=2026-08-31&key=k1", { scroll: false });
  });

  it("exports exactly what is on screen", () => {
    render(<UsageControls {...base} keyId="k1" exportQuery="from=2026-09-25&to=2026-10-08&key_id=k1" />);
    expect(screen.getByRole("link", { name: "Export CSV" })).toHaveAttribute("href", "/api/usage/export?from=2026-09-25&to=2026-10-08&key_id=k1");
  });

  it("has no accessibility violations, with the custom fields open too", async () => {
    const { container } = render(<UsageControls {...base} preset="custom" />);
    expect(await axe(container)).toHaveNoViolations();
  });
});
