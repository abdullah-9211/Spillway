import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { axe } from "vitest-axe";
import type { Role } from "@/lib/api";
import type { ApiKey } from "@/lib/format";
import { RoleProvider } from "@/lib/role";
import { BudgetBar } from "./BudgetBar";
import { KeysView } from "./KeysView";
import { KeyTable } from "./KeyTable";

const refresh = vi.fn();
vi.mock("next/navigation", () => ({ useRouter: () => ({ refresh, replace: vi.fn(), push: vi.fn() }) }));

const NOW = new Date("2026-10-08T12:00:00Z");

function key(over: Partial<ApiKey> = {}): ApiKey {
  return {
    id: "00000000-0000-4000-8000-000000000001", name: "support-agent", prefix: "spw_7Hq2", rate_limit_rpm: 120,
    monthly_budget_usd: "100.000000", spend_usd: "42.100000", semantic_cache: true, cache_nonzero_temp: false,
    created_at: "2026-10-01T09:00:00Z", revoked_at: null, last_used_at: "2026-10-08T11:58:00Z", builtin: false, ...over,
  };
}

const states = [
  key({ id: "1", name: "normal-key" }),
  key({ id: "2", name: "close-key", spend_usd: "88.400000" }),
  key({ id: "3", name: "over-key", spend_usd: "100.000000" }),
  key({ id: "4", name: "unlimited-key", rate_limit_rpm: null, monthly_budget_usd: null, spend_usd: "9.300000", semantic_cache: false }),
  key({ id: "5", name: "playground", builtin: true, spend_usd: "0.840000", monthly_budget_usd: "5.000000" }),
  key({ id: "6", name: "old-demo", revoked_at: "2026-09-28T10:00:00Z", last_used_at: "2026-09-27T10:00:00Z" }),
];

afterEach(() => vi.unstubAllGlobals());
beforeEach(() => refresh.mockClear());

const withRole = (role: Role, ui: React.ReactNode) => render(<RoleProvider role={role}>{ui}</RoleProvider>);

describe("BudgetBar", () => {
  it("states the amounts and the situation in words, not only colour", () => {
    const { rerender } = render(<BudgetBar spend="42.100000" budget="100.000000" />);
    expect(screen.getByText("$42.10")).toBeInTheDocument();
    expect(screen.getByText("of $100.00")).toBeInTheDocument();
    expect(screen.queryByText(/close to the limit|over budget/i)).not.toBeInTheDocument();

    rerender(<BudgetBar spend="88.400000" budget="100.000000" />);
    expect(screen.getByText("Close to the limit")).toBeInTheDocument();

    rerender(<BudgetBar spend="100.000000" budget="100.000000" />);
    expect(screen.getByText("Over budget, requests are refused")).toBeInTheDocument();
    expect(screen.getByRole("img")).toHaveAccessibleName("100% of the monthly budget spent");
  });

  it("has no bar when there is no budget", () => {
    render(<BudgetBar spend="9.300000" budget={null} />);
    expect(screen.getByText("no limit")).toBeInTheDocument();
    expect(screen.queryByRole("img")).not.toBeInTheDocument();
  });

  it("never overflows its track", () => {
    const { container } = render(<BudgetBar spend="500" budget="100" />);
    expect(container.querySelector(".trk i")).toHaveStyle({ width: "100%" });
  });
});

describe("KeyTable", () => {
  const table = (canEdit: boolean) => (
    <KeyTable keys={states} canEdit={canEdit} now={NOW} onEdit={() => {}} onRevoke={() => {}} />
  );

  it("shows each state: normal, close to the limit, over budget, no limit, built in, revoked", () => {
    render(table(true));
    const row = (name: string) => screen.getByText(name, { selector: ".nm" }).closest('[role="row"]') as HTMLElement;
    expect(within(row("normal-key")).queryByText(/close to|over budget/i)).toBeNull();
    expect(within(row("close-key")).getByText("Close to the limit")).toBeInTheDocument();
    expect(within(row("over-key")).getByText("Over budget, requests are refused")).toBeInTheDocument();
    expect(within(row("unlimited-key")).getByText("No limit")).toBeInTheDocument();
    expect(within(row("playground")).getByText("Built in, used by this dashboard")).toBeInTheDocument();
    expect(row("old-demo")).toHaveAttribute("data-state", "revoked");
    expect(within(row("old-demo")).getByText("Revoked on Sep 28")).toBeInTheDocument();
  });

  it("shows only the first characters of a key", () => {
    render(table(true));
    expect(screen.getAllByText("spw_7Hq2…").length).toBeGreaterThan(0);
  });

  it("offers Edit and Revoke only for ordinary, active keys", () => {
    render(table(true));
    expect(screen.getByRole("button", { name: "Edit normal-key" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Revoke over-key" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /playground/ })).toBeNull(); // built in
    expect(screen.queryByRole("button", { name: /old-demo/ })).toBeNull(); // revoked
  });

  it("offers nothing to a viewer", () => {
    render(table(false));
    expect(screen.queryAllByRole("button")).toHaveLength(0);
    expect(screen.getByRole("columnheader", { name: "Read only" })).toBeInTheDocument();
  });

  it("has no accessibility violations", async () => {
    const { container } = render(table(true));
    expect(await axe(container)).toHaveNoViolations();
  });
});

function mockApi(handlers: Record<string, (body?: unknown) => Response>) {
  const f = vi.fn(async (url: string, init?: RequestInit) => {
    const h = handlers[`${init?.method ?? "GET"} ${url}`];
    if (!h) throw new Error(`unexpected ${init?.method} ${url}`);
    return h(init?.body ? JSON.parse(init.body as string) : undefined);
  });
  vi.stubGlobal("fetch", f);
  return f;
}

describe("KeysView as a viewer", () => {
  it("is read only: Create is disabled, the lock note shows, no actions", async () => {
    withRole("viewer", <KeysView keys={states} />);
    expect(screen.getByRole("button", { name: "Create key" })).toBeDisabled();
    expect(screen.getByText(/signed in as a viewer/i)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^(Edit|Revoke) / })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Create key" }));
    expect(screen.queryByRole("region", { name: "Key details" })).toBeNull();
  });

  it("has no accessibility violations", async () => {
    const { container } = withRole("viewer", <KeysView keys={states} />);
    expect(await axe(container)).toHaveNoViolations();
  });
});

describe("KeysView as an admin", () => {
  it("creates a key and shows its secret once", async () => {
    const created = key({ id: "9", name: "ci-pipeline", rate_limit_rpm: 60, monthly_budget_usd: "30.000000", spend_usd: "0.000000", last_used_at: null });
    const f = mockApi({ "POST /api/keys": () => Response.json({ key: created, secret: "spw_SECRET_VALUE_123" }, { status: 201 }) });
    withRole("admin", <KeysView keys={states} />);

    await userEvent.click(screen.getByRole("button", { name: "Create key" }));
    const panel = screen.getByRole("region", { name: "Key details" });
    await userEvent.type(within(panel).getByLabelText("Name"), "ci-pipeline");
    await userEvent.type(within(panel).getByLabelText("Requests a minute"), "60");
    await userEvent.type(within(panel).getByLabelText("Monthly budget (USD)"), "30");
    await userEvent.click(within(panel).getByLabelText(/Use the semantic cache/));
    await userEvent.click(within(panel).getByRole("button", { name: "Create key" }));

    expect(await screen.findByTestId("secret")).toHaveTextContent("spw_SECRET_VALUE_123");
    expect(screen.getByText(/cannot show this key again/i)).toBeInTheDocument();
    const [, init] = f.mock.calls[0] as unknown as [string, RequestInit];
    expect(JSON.parse(init.body as string)).toEqual({ name: "ci-pipeline", rate_limit_rpm: 60, monthly_budget_usd: "30", semantic_cache: true, cache_nonzero_temp: false });
    expect(refresh).toHaveBeenCalled();

    await userEvent.click(screen.getByRole("button", { name: "Done" }));
    expect(screen.queryByTestId("secret")).toBeNull();
    expect(document.body.textContent).not.toContain("spw_SECRET_VALUE_123");
  });

  it("copies the key", async () => {
    mockApi({ "POST /api/keys": () => Response.json({ key: key({ name: "k" }), secret: "spw_COPYME" }, { status: 201 }) });
    const writeText = vi.fn(async () => {});
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    withRole("admin", <KeysView keys={[]} />);
    await userEvent.click(screen.getByRole("button", { name: "Create key" }));
    await userEvent.type(screen.getByLabelText("Name"), "k");
    await userEvent.click(within(screen.getByRole("region", { name: "Key details" })).getByRole("button", { name: "Create key" }));
    await userEvent.click(await screen.findByRole("button", { name: "Copy key" }));
    expect(writeText).toHaveBeenCalledWith("spw_COPYME");
    expect(await screen.findByText("Copied to the clipboard.")).toBeInTheDocument();
  });

  it("does not call the server when the form is invalid", async () => {
    const f = mockApi({});
    withRole("admin", <KeysView keys={[]} />);
    await userEvent.click(screen.getByRole("button", { name: "Create key" }));
    await userEvent.type(screen.getByLabelText("Requests a minute"), "0");
    await userEvent.type(screen.getByLabelText("Monthly budget (USD)"), "lots");
    await userEvent.click(within(screen.getByRole("region", { name: "Key details" })).getByRole("button", { name: "Create key" }));
    expect(screen.getByText("Give the key a name.")).toBeInTheDocument();
    expect(screen.getByText(/whole number from 1/)).toBeInTheDocument();
    expect(screen.getByText(/dollars such as 30/)).toBeInTheDocument();
    expect(screen.getByLabelText("Name")).toBeInvalid();
    expect(f).not.toHaveBeenCalled();
  });

  it("shows the server's message when creating fails, and keeps what was typed", async () => {
    mockApi({ "POST /api/keys": () => Response.json({ code: "invalid_request", message: "The name is taken." }, { status: 400 }) });
    withRole("admin", <KeysView keys={[]} />);
    await userEvent.click(screen.getByRole("button", { name: "Create key" }));
    await userEvent.type(screen.getByLabelText("Name"), "dup");
    await userEvent.click(within(screen.getByRole("region", { name: "Key details" })).getByRole("button", { name: "Create key" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("The name is taken.");
    expect(screen.getByLabelText("Name")).toHaveValue("dup");
  });

  it("edits in place: the editor opens directly under that key's row, not in a distant panel", async () => {
    const f = mockApi({ "PATCH /api/keys/2": () => Response.json(key({ id: "2", name: "second" })) });
    const list = [key({ id: "1", name: "first" }), key({ id: "2", name: "second", rate_limit_rpm: 60, monthly_budget_usd: "30.000000" }), key({ id: "3", name: "third" })];
    withRole("admin", <KeysView keys={list} />);
    await userEvent.click(screen.getByRole("button", { name: "Edit second" }));

    expect(screen.queryByRole("region", { name: "Key details" })).toBeNull(); // edits happen in the row, not in the create card
    const rows = screen.getAllByRole("row").map((r) => r.textContent ?? "");
    const at = rows.findIndex((t) => t.startsWith("second"));
    expect(rows[at + 1]).toContain("Save changes"); // the editor is the very next row
    expect(rows[at + 2]).toContain("third");

    const editor = screen.getByRole("heading", { name: "Edit second" }).parentElement as HTMLElement;
    expect(within(editor).getByLabelText("Name")).toHaveValue("second");
    expect(within(editor).getByLabelText("Name")).toHaveFocus();
    expect(within(editor).getByLabelText("Requests a minute")).toHaveValue("60");
    expect(within(editor).getByLabelText("Monthly budget (USD)")).toHaveValue("30");
    await userEvent.clear(within(editor).getByLabelText("Monthly budget (USD)"));
    await userEvent.click(within(editor).getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(refresh).toHaveBeenCalled());
    const [, init] = f.mock.calls[0] as unknown as [string, RequestInit];
    expect(JSON.parse(init.body as string)).toMatchObject({ monthly_budget_usd: null, rate_limit_rpm: 60 });
    expect(screen.queryByRole("heading", { name: "Edit second" })).toBeNull();
    expect(screen.getByText("Saved changes to second.")).toBeInTheDocument();
  });

  it("scrolls the editor into view without jumping the page to the top", async () => {
    const scroll = vi.fn();
    Element.prototype.scrollIntoView = scroll;
    withRole("admin", <KeysView keys={[key({ id: "1", name: "only" })]} />);
    await userEvent.click(screen.getByRole("button", { name: "Edit only" }));
    expect(scroll).toHaveBeenCalledWith({ block: "nearest", behavior: "smooth" });
    Element.prototype.scrollIntoView = () => {};
  });

  it("only one editor is open at a time", async () => {
    withRole("admin", <KeysView keys={[key({ id: "1", name: "a" }), key({ id: "2", name: "b" })]} />);
    await userEvent.click(screen.getByRole("button", { name: "Edit a" }));
    await userEvent.click(screen.getByRole("button", { name: "Edit b" }));
    expect(screen.queryByRole("heading", { name: "Edit a" })).toBeNull();
    expect(screen.getByRole("heading", { name: "Edit b" })).toBeInTheDocument();
  });

  it("cancelling an edit changes nothing", async () => {
    const f = mockApi({});
    withRole("admin", <KeysView keys={[key({ id: "1", name: "a" })]} />);
    await userEvent.click(screen.getByRole("button", { name: "Edit a" }));
    await userEvent.type(screen.getByLabelText("Name"), "zzz");
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("heading", { name: "Edit a" })).toBeNull();
    expect(f).not.toHaveBeenCalled();
  });

  it("asks before revoking, in place, and revokes on confirmation", async () => {
    const f = mockApi({ "DELETE /api/keys/1": () => Response.json(key({ id: "1", name: "doomed", revoked_at: "2026-10-08T12:00:00Z" })) });
    withRole("admin", <KeysView keys={[key({ id: "1", name: "doomed" })]} />);
    await userEvent.click(screen.getByRole("button", { name: "Revoke doomed" }));
    expect(screen.getByRole("heading", { name: "Revoke doomed?" })).toBeInTheDocument();
    expect(f).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Keep key" }));
    expect(f).not.toHaveBeenCalled();

    await userEvent.click(screen.getByRole("button", { name: "Revoke doomed" }));
    await userEvent.click(screen.getByRole("button", { name: "Revoke key" }));
    await waitFor(() => expect(f).toHaveBeenCalledTimes(1));
    expect(refresh).toHaveBeenCalled();
    expect(await screen.findByText(/Revoked doomed/)).toBeInTheDocument();
  });

  it("shows a failed save in the editor and keeps it open", async () => {
    mockApi({ "PATCH /api/keys/1": () => Response.json({ code: "key_revoked", message: "A revoked key cannot be changed." }, { status: 409 }) });
    withRole("admin", <KeysView keys={[key({ id: "1", name: "a" })]} />);
    await userEvent.click(screen.getByRole("button", { name: "Edit a" }));
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("A revoked key cannot be changed.");
    expect(screen.getByRole("heading", { name: "Edit a" })).toBeInTheDocument();
  });

  it("explains an empty list", () => {
    withRole("admin", <KeysView keys={[]} />);
    expect(screen.getByText(/No keys yet/)).toBeInTheDocument();
  });

  it("has no accessibility violations with an inline editor open", async () => {
    const { container } = withRole("admin", <KeysView keys={states} />);
    await userEvent.click(screen.getByRole("button", { name: "Edit normal-key" }));
    expect(await axe(container)).toHaveNoViolations();
  });

  it("has no accessibility violations with the create panel open", async () => {
    const { container } = withRole("admin", <KeysView keys={states} />);
    await userEvent.click(screen.getByRole("button", { name: "Create key" }));
    expect(await axe(container)).toHaveNoViolations();
  });
});

function many(n: number): ApiKey[] {
  return Array.from({ length: n }, (_, i) => key({ id: String(i + 1), name: `key-${String(i + 1).padStart(2, "0")}`, prefix: `spw_${String(i + 1).padStart(4, "0")}`, created_at: `2026-10-${String((i % 28) + 1).padStart(2, "0")}T09:00:00Z` }));
}

describe("finding keys", () => {
  const names = () =>
    screen.getAllByRole("row").slice(1).map((r) => r.querySelector(".nm")?.firstChild?.textContent).filter(Boolean);

  it("pages a long list instead of one endless table", async () => {
    withRole("admin", <KeysView keys={many(23)} />);
    expect(names()).toHaveLength(10);
    expect(screen.getByText("Showing 1 to 10 of 23")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Previous" })).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getByText("Showing 11 to 20 of 23")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    expect(names()).toHaveLength(3);
    expect(screen.getByRole("button", { name: "Next" })).toBeDisabled();
  });

  it("shows no pager when everything fits on one page", () => {
    withRole("admin", <KeysView keys={many(5)} />);
    expect(screen.queryByRole("navigation", { name: "Pages of keys" })).toBeNull();
  });

  it("searches by name or key prefix as you type, and goes back to the first page", async () => {
    withRole("admin", <KeysView keys={many(23)} />);
    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    await userEvent.type(screen.getByRole("searchbox", { name: "Search keys" }), "key-07");
    expect(names()).toEqual(["key-07"]);
    expect(screen.getByText("1 of 23 keys")).toBeInTheDocument();
    await userEvent.clear(screen.getByRole("searchbox", { name: "Search keys" }));
    await userEvent.type(screen.getByRole("searchbox", { name: "Search keys" }), "spw_0012");
    expect(names()).toEqual(["key-12"]);
  });

  it("says so when nothing matches, and offers to clear", async () => {
    withRole("admin", <KeysView keys={many(3)} />);
    await userEvent.type(screen.getByRole("searchbox", { name: "Search keys" }), "nothing like this");
    expect(screen.getByText(/No keys match these filters/)).toBeInTheDocument();
    await userEvent.click(screen.getAllByRole("button", { name: "Clear filters" })[0]);
    expect(names()).toHaveLength(3);
  });

  it("filters by status with counts, hiding revoked keys by default", async () => {
    withRole("admin", <KeysView keys={states} />);
    expect(names()).not.toContain("old-demo");
    expect(screen.getByRole("button", { name: /Active/ })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: /Revoked/ })).toHaveTextContent("1");
    await userEvent.click(screen.getByRole("button", { name: /Revoked/ }));
    expect(names()).toEqual(["old-demo"]);
    await userEvent.click(screen.getByRole("button", { name: /All/ }));
    expect(names()).toHaveLength(6);
  });

  it("filters by budget: needs attention, over budget, no limit", async () => {
    withRole("admin", <KeysView keys={states} />);
    const sel = screen.getByRole("combobox", { name: "Filter by budget" });
    await userEvent.selectOptions(sel, "Needs attention");
    expect(names().sort()).toEqual(["close-key", "over-key"]);
    await userEvent.selectOptions(sel, "Over budget");
    expect(names()).toEqual(["over-key"]);
    await userEvent.selectOptions(sel, "No budget limit");
    expect(names()).toEqual(["unlimited-key"]);
  });

  it("sorts", async () => {
    withRole("admin", <KeysView keys={states} />);
    const sel = screen.getByRole("combobox", { name: "Sort keys" });
    await userEvent.selectOptions(sel, "Name, A to Z");
    expect(names()).toEqual(["close-key", "normal-key", "over-key", "playground", "unlimited-key"]);
    await userEvent.selectOptions(sel, "Highest spend");
    expect(names()[0]).toBe("over-key");
  });

  it("closes an open editor when the list changes under it", async () => {
    withRole("admin", <KeysView keys={states} />);
    await userEvent.click(screen.getByRole("button", { name: "Edit normal-key" }));
    await userEvent.type(screen.getByRole("searchbox", { name: "Search keys" }), "over");
    expect(screen.queryByRole("heading", { name: /Edit/ })).toBeNull();
  });

  it("has no accessibility violations", async () => {
    const { container } = withRole("admin", <KeysView keys={many(23)} />);
    expect(await axe(container)).toHaveNoViolations();
  });

  it("offers the same search to a viewer", async () => {
    withRole("viewer", <KeysView keys={many(23)} />);
    await userEvent.type(screen.getByRole("searchbox", { name: "Search keys" }), "key-03");
    expect(names()).toEqual(["key-03"]);
  });
});
