import { describe, expect, it } from "vitest";
import { nextTheme, parseTheme, readThemeCookie, systemTheme, themeBootScript, themeCookie } from "./theme";

describe("theme", () => {
  it("accepts only the two themes", () => {
    expect(parseTheme("dark")).toBe("dark");
    expect(parseTheme("light")).toBe("light");
    for (const bad of ["", "Dark", "blue", null, undefined]) expect(parseTheme(bad)).toBeNull();
  });

  it("toggles", () => {
    expect(nextTheme("dark")).toBe("light");
    expect(nextTheme("light")).toBe("dark");
  });

  it("defaults to the system preference", () => {
    expect(systemTheme(true)).toBe("light");
    expect(systemTheme(false)).toBe("dark");
  });

  it("round-trips through the cookie", () => {
    const set = themeCookie("light");
    expect(set).toContain("spillway_theme=light");
    expect(set).toContain("Path=/");
    expect(set).toContain("SameSite=Lax");
    expect(readThemeCookie("a=1; " + set.split(";")[0] + "; b=2")).toBe("light");
    expect(readThemeCookie("a=1")).toBeNull();
    expect(readThemeCookie("spillway_theme=purple")).toBeNull();
  });

  it("boot script picks the saved theme, else the system one", () => {
    const run = (cookie: string, prefersLight: boolean) => {
      const attrs: Record<string, string> = {};
      new Function("document", "window", themeBootScript)(
        { cookie, documentElement: { setAttribute: (k: string, v: string) => (attrs[k] = v) } },
        { matchMedia: () => ({ matches: prefersLight }) },
      );
      return attrs["data-theme"];
    };
    expect(run("spillway_theme=light", false)).toBe("light");
    expect(run("spillway_theme=dark", true)).toBe("dark");
    expect(run("", true)).toBe("light");
    expect(run("", false)).toBe("dark");
    expect(run("spillway_theme=nonsense", true)).toBe("light");
  });
});
