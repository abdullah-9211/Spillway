import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { axe } from "vitest-axe";
import { LoginCard } from "./LoginCard";
import { failureText } from "./LoginForm";

const replace = vi.fn();
const refresh = vi.fn();
vi.mock("next/navigation", () => ({ useRouter: () => ({ replace, refresh }) }));

beforeEach(() => {
  replace.mockClear();
  refresh.mockClear();
});
afterEach(() => vi.unstubAllGlobals());

async function submit(user = "admin", pw = "secret") {
  await userEvent.type(screen.getByLabelText("Username"), user);
  await userEvent.type(screen.getByLabelText("Password"), pw);
  await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
}

describe("sign in", () => {
  it("shows no error before an attempt", () => {
    render(<LoginCard />);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Sign in" })).toBeInTheDocument();
  });

  it("shows the design's message after a wrong password", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => Response.json({ code: "invalid_credentials", message: "x" }, { status: 401 })));
    render(<LoginCard />);
    await submit("admin", "wrong");
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("That username and password do not match.");
    expect(alert).toHaveTextContent("After 5 failed tries in a minute");
    expect(replace).not.toHaveBeenCalled();
  });

  it("tells a throttled user how long to wait", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => Response.json({ code: "too_many_attempts", message: "x" }, { status: 429, headers: { "Retry-After": "45" } })));
    render(<LoginCard />);
    await submit();
    expect(await screen.findByRole("alert")).toHaveTextContent("Try again in 45 seconds.");
  });

  it("explains an unreachable service", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => { throw new TypeError("offline"); }));
    render(<LoginCard />);
    await submit();
    expect(await screen.findByRole("alert")).toHaveTextContent(/could not reach/i);
  });

  it("goes to the app after a good sign-in", async () => {
    const f = vi.fn(async () => Response.json({ user: { username: "admin", role: "admin" } }));
    vi.stubGlobal("fetch", f);
    render(<LoginCard />);
    await submit("admin", "right");
    await waitFor(() => expect(replace).toHaveBeenCalledWith("/"));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    const init = (f.mock.calls[0] as unknown as [string, RequestInit])[1];
    expect(JSON.parse(init.body as string)).toEqual({ username: "admin", password: "right" });
  });

  it("disables the button while signing in", async () => {
    let release!: (r: Response) => void;
    vi.stubGlobal("fetch", vi.fn(() => new Promise<Response>((r) => (release = r))));
    render(<LoginCard />);
    await submit();
    const btn = await screen.findByRole("button", { name: "Signing in…" });
    expect(btn).toBeDisabled();
    release(Response.json({ code: "invalid_credentials", message: "" }, { status: 401 }));
    await screen.findByRole("alert");
    expect(screen.getByRole("button", { name: "Sign in" })).toBeEnabled();
  });

  it("clears an earlier error on the next attempt", async () => {
    const f = vi.fn()
      .mockResolvedValueOnce(Response.json({ code: "invalid_credentials", message: "" }, { status: 401 }))
      .mockResolvedValueOnce(Response.json({ user: { username: "a", role: "admin" } }));
    vi.stubGlobal("fetch", f);
    render(<LoginCard />);
    await submit("a", "bad");
    await screen.findByRole("alert");
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
    await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
  });

  it("has no accessibility violations, with or without an error", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => Response.json({ code: "invalid_credentials", message: "" }, { status: 401 })));
    const { container } = render(<LoginCard />);
    expect(await axe(container)).toHaveNoViolations();
    await submit();
    await screen.findByRole("alert");
    expect(await axe(container)).toHaveNoViolations();
  });
});

describe("failureText", () => {
  it("covers each case", () => {
    expect(failureText({ status: 401, message: "" })).toMatch(/do not match/);
    expect(failureText({ status: 429, message: "", retryAfter: 1 })).toMatch(/1 second\./);
    expect(failureText({ status: 429, message: "" })).toMatch(/paused\.$/);
    expect(failureText({ status: 502, message: "Cannot reach the Spillway service." })).toBe("Cannot reach the Spillway service.");
    expect(failureText({ status: 500, message: "" })).toMatch(/Could not sign in/);
  });
});
