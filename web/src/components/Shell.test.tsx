import { render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { axe } from "vitest-axe";
import type { Status, User } from "@/lib/api";
import { isActive } from "./NavLinks";
import { Shell } from "./Shell";
import { StatusBadge, statusLabel, type RunStatus } from "./StatusBadge";
import { initials } from "./UserChip";
import { StatTile } from "./ui";

vi.mock("next/navigation", () => ({ usePathname: () => "/usage" }));
vi.mock("next/link", () => ({ default: ({ href, children, ...r }: { href: string; children: React.ReactNode }) => <a href={href} {...r}>{children}</a> }));

const admin: User = { username: "admin", role: "admin" };
const viewer: User = { username: "viewer", role: "viewer" };
const up: Status = { postgres: "up", redis: "up" };

describe("Shell", () => {
  it("shows the four screens and marks the current one", () => {
    render(<Shell user={admin} status={up}>page</Shell>);
    const nav = screen.getByRole("navigation", { name: "Main" });
    expect(within(nav).getAllByRole("link").map((a) => a.textContent)).toEqual(["Runs", "Usage and cost", "API keys", "Playground"]);
    expect(within(nav).getByRole("link", { name: "Usage and cost" })).toHaveAttribute("aria-current", "page");
    expect(within(nav).getByRole("link", { name: "Runs" })).not.toHaveAttribute("aria-current");
  });

  it("shows the signed-in user and their role", () => {
    const { rerender } = render(<Shell user={admin} status={up}>x</Shell>);
    expect(screen.getByText("admin", { selector: ".role" })).toBeInTheDocument();
    rerender(<Shell user={viewer} status={up}>x</Shell>);
    expect(screen.getByText("viewer", { selector: ".role" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Sign out" })).toBeInTheDocument();
  });

  it("states connection status in words", () => {
    const { rerender } = render(<Shell user={admin} status={up}>x</Shell>);
    expect(screen.getByText("Postgres connected")).toBeInTheDocument();
    expect(screen.getByText("Redis connected")).toBeInTheDocument();
    rerender(<Shell user={admin} status={{ postgres: "down", redis: "disabled" }}>x</Shell>);
    expect(screen.getByText("Postgres is down")).toBeInTheDocument();
    expect(screen.getByText(/Redis is off/)).toBeInTheDocument();
    rerender(<Shell user={admin} status={null}>x</Shell>);
    expect(screen.getByText("Cannot reach the Spillway service")).toBeInTheDocument();
  });

  it("has no accessibility violations", async () => {
    const { container } = render(<Shell user={viewer} status={up}><h1>Runs</h1></Shell>);
    expect(await axe(container)).toHaveNoViolations();
  });

  it("has no accessibility violations when degraded", async () => {
    const { container } = render(<Shell user={admin} status={null}><h1>Runs</h1></Shell>);
    expect(await axe(container)).toHaveNoViolations();
  });
});

describe("isActive", () => {
  it("matches sections and their children", () => {
    expect(isActive("/", "/")).toBe(true);
    expect(isActive("/runs/abc", "/")).toBe(true);
    expect(isActive("/usage", "/")).toBe(false);
    expect(isActive("/keys/new", "/keys")).toBe(true);
    expect(isActive("/keysmith", "/keys")).toBe(false);
  });
});

describe("initials", () => {
  it("uses two letters", () => {
    expect(initials("admin")).toBe("AD");
    expect(initials("jane.doe")).toBe("JD");
    expect(initials("x")).toBe("X");
  });
});

describe("StatusBadge", () => {
  const all: RunStatus[] = ["succeeded", "running", "waiting", "sleeping", "failed", "cancelled", "queued"];

  it.each(all)("%s is a shape plus a word, never colour alone", (s) => {
    const { container } = render(<StatusBadge status={s} />);
    expect(container.querySelector("svg path")).not.toBeNull();
    expect(container).toHaveTextContent(statusLabel(s));
  });

  it("uses the design's wording", () => {
    expect(statusLabel("waiting")).toBe("Waiting for approval");
    expect(statusLabel("succeeded")).toBe("Succeeded");
  });

  it("has no accessibility violations", async () => {
    const { container } = render(<ul>{all.map((s) => <li key={s}><StatusBadge status={s} /></li>)}</ul>);
    expect(await axe(container)).toHaveNoViolations();
  });
});

describe("StatTile", () => {
  it("shows the label and the value", () => {
    render(<StatTile label="Running" value={6} tone="run" />);
    expect(screen.getByText("Running")).toBeInTheDocument();
    expect(screen.getByText("6")).toBeInTheDocument();
  });
});
