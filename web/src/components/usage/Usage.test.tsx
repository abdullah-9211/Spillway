import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { axe } from "vitest-axe";
import { sumUsd, type UsageGroup, type UsageSummary } from "@/lib/usage";
import { CacheAndHealth } from "./CacheAndHealth";
import { Kpis } from "./Kpis";
import { ModelTable } from "./ModelTable";
import { SpendChart } from "./SpendChart";
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

const days: UsageGroup[] = ["2026-09-30", "2026-10-01", "2026-10-02"].map((d, i) =>
  grp({ key: d, label: d, requests: 100 * (i + 1), cost_usd: ["9.200000", "10.800000", "0.000000"][i], models: i === 0 ? { sonnet: "7.100000", mini: "2.100000" } : i === 1 ? { sonnet: "8.400000", mini: "2.400000" } : {} }));

const summary: UsageSummary = {
  range: { from: "2026-09-30", to: "2026-10-02", days: 3 }, group_by: "model", fallbacks_fired: 212, added_latency: { p50_ms: 3.2, p95_ms: 14.1, p99_ms: 29, samples: 480, since: "2026-10-08T00:00:00Z" },
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

describe("SpendChart", () => {
  it("has a legend naming each series, including a free model that has no bar", () => {
    render(<SpendChart days={days} byModel={models} />);
    const legend = document.querySelector(".legend") as HTMLElement;
    expect(within(legend).getByText("sonnet")).toBeInTheDocument();
    expect(within(legend).getByText("mini")).toBeInTheDocument();
    expect(within(legend).getByText("local (free, no bar)")).toBeInTheDocument();
  });

  it("gives every bar a spoken figure and focus, so it is not hover-only", () => {
    render(<SpendChart days={days} byModel={models} />);
    const bars = screen.getAllByRole("img");
    expect(bars).toHaveLength(3);
    expect(bars[0]).toHaveAccessibleName(/Sep 30: \$9\.20 spent \(sonnet \$7\.10, mini \$2\.10\), 100 requests/);
    expect(bars[0]).toHaveAttribute("tabindex", "0");
    expect(bars[0]).toHaveAttribute("title");
  });

  it("switches to a table whose totals equal the chart's", async () => {
    render(<SpendChart days={days} byModel={models} />);
    await userEvent.click(screen.getByRole("button", { name: "View as table" }));
    const table = screen.getByRole("table", { name: "Spend per day by model" });
    const rows = within(table).getAllByRole("row");
    expect(rows).toHaveLength(1 + 3 + 1); // header, three days, total
    const total = within(rows[rows.length - 1]).getAllByRole("cell").map((c) => c.textContent);
    expect(within(rows[rows.length - 1]).getByRole("rowheader")).toHaveTextContent("Total");
    const expected = sumUsd(days.map((d) => d.cost_usd)); // 20.000000
    expect(total).toContain("$20.00");
    expect(expected).toBe("20.000000");
    expect(screen.getByRole("button", { name: "View as chart" })).toHaveAttribute("aria-pressed", "true");
    await userEvent.click(screen.getByRole("button", { name: "View as chart" }));
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("says when nothing was spent", () => {
    render(<SpendChart days={days.map((d) => ({ ...d, cost_usd: "0.000000", models: {} }))} byModel={[]} />);
    expect(screen.getByText("No spend in this range.")).toBeInTheDocument();
  });

  it("has no accessibility violations, as a chart and as a table", async () => {
    const { container } = render(<SpendChart days={days} byModel={models} />);
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
  it("lists models with latency, cache hit and error rates", () => {
    render(<ModelTable models={models} days={days} />);
    const row = screen.getByRole("rowheader", { name: "sonnet" }).closest("tr") as HTMLElement;
    const cells = within(row).getAllByRole("cell").map((c) => c.textContent);
    expect(cells).toEqual(["21,480", "31.2M", "8.9M", "$142.18", "1.9s", "5.2s", "21%", "0.4%"]);
  });

  it("shows a dash instead of a latency it never measured", () => {
    render(<ModelTable models={[grp({ key: "m", label: "m", requests: 4, cache_hits: 4 })]} days={[]} />);
    const cells = within(screen.getByRole("rowheader", { name: "m" }).closest("tr") as HTMLElement).getAllByRole("cell").map((c) => c.textContent);
    expect(cells.slice(4, 6)).toEqual(["—", "—"]);
  });

  it("has a totals row that adds up exactly", () => {
    render(<ModelTable models={models} days={days} />);
    const foot = screen.getByRole("rowheader", { name: "Total" }).closest("tr") as HTMLElement;
    const cells = within(foot).getAllByRole("cell").map((c) => c.textContent);
    expect(cells[0]).toBe("48,210"); // 21,480 + 19,730 + 7,000
    expect(cells[3]).toBe("$186.42"); // 142.18 + 44.24 + 0, summed in exact micro-dollars
  });

  it("explains an empty range, and has no accessibility violations", async () => {
    const { container, rerender } = render(<ModelTable models={models} days={days} />);
    expect(await axe(container)).toHaveNoViolations();
    rerender(<ModelTable models={[]} days={[]} />);
    expect(screen.getByText("No requests in this range.")).toBeInTheDocument();
  });
});

describe("UsageControls", () => {
  const keys = [{ id: "k1", name: "support-agent" }, { id: "k2", name: "ci (revoked)" }];

  it("marks the current range and keeps the key when the range changes", () => {
    render(<UsageControls preset="14d" keyId="k1" keys={keys} exportQuery="from=2026-09-25&to=2026-10-08&key_id=k1" />);
    expect(screen.getByRole("link", { name: "14 days" })).toHaveAttribute("aria-current", "true");
    expect(screen.getByRole("link", { name: "7 days" })).not.toHaveAttribute("aria-current");
    expect(screen.getByRole("link", { name: "7 days" })).toHaveAttribute("href", "/usage?range=7d&key=k1");
    expect(screen.getByRole("link", { name: "This month" })).toHaveAttribute("href", "/usage?range=month&key=k1");
  });

  it("changes the key through the URL and keeps the range", async () => {
    push.mockClear();
    render(<UsageControls preset="7d" keyId="" keys={keys} exportQuery="from=a&to=b" />);
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "API key" }), "support-agent");
    expect(push).toHaveBeenCalledWith("/usage?range=7d&key=k1", { scroll: false });
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "API key" }), "All API keys");
  });

  it("exports exactly what is on screen", () => {
    render(<UsageControls preset="14d" keyId="k1" keys={keys} exportQuery="from=2026-09-25&to=2026-10-08&key_id=k1" />);
    expect(screen.getByRole("link", { name: "Export CSV" })).toHaveAttribute("href", "/api/usage/export?from=2026-09-25&to=2026-10-08&key_id=k1");
  });

  it("has no accessibility violations", async () => {
    const { container } = render(<UsageControls preset="14d" keyId="" keys={keys} exportQuery="from=a&to=b" />);
    expect(await axe(container)).toHaveNoViolations();
  });
});
