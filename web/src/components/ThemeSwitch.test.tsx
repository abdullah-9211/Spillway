import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it } from "vitest";
import { ThemeSwitch } from "./ThemeSwitch";

beforeEach(() => {
  document.documentElement.removeAttribute("data-theme");
  document.cookie = "spillway_theme=; Max-Age=0; Path=/";
});

describe("ThemeSwitch", () => {
  it("offers the other theme and switches to it", async () => {
    document.documentElement.setAttribute("data-theme", "dark");
    render(<ThemeSwitch />);
    await userEvent.click(screen.getByRole("button", { name: "Switch to the light theme" }));
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
    expect(document.cookie).toContain("spillway_theme=light");
    expect(screen.getByRole("button", { name: "Switch to the dark theme" })).toBeInTheDocument();
  });

  it("switches back", async () => {
    document.documentElement.setAttribute("data-theme", "light");
    render(<ThemeSwitch />);
    await userEvent.click(screen.getByRole("button"));
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
    expect(document.cookie).toContain("spillway_theme=dark");
  });
});
