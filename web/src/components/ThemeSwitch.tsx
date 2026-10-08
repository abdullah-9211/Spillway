"use client";

import { useState } from "react";
import { nextTheme, parseTheme, themeCookie, type Theme } from "@/lib/theme";
import { MoonIcon, SunIcon } from "./icons";

function currentTheme(): Theme {
  return parseTheme(document.documentElement.getAttribute("data-theme")) ?? "dark";
}

/** Switches between the dark and the light theme and remembers the choice in a cookie. */
export function ThemeSwitch() {
  // The server cannot know the system preference, so the real theme is read from <html> after mount.
  const [theme, setTheme] = useState<Theme | null>(null);
  const shown = theme ?? "dark";
  const target = nextTheme(theme ?? (typeof document === "undefined" ? "dark" : currentTheme()));

  function toggle() {
    const next = nextTheme(currentTheme());
    document.documentElement.setAttribute("data-theme", next);
    document.cookie = themeCookie(next);
    setTheme(next);
  }

  return (
    <button type="button" className="btn sm theme-switch" onClick={toggle} aria-label={`Switch to the ${target} theme`} data-theme-now={shown}>
      {target === "light" ? <SunIcon /> : <MoonIcon />}
      <span>{target === "light" ? "Light" : "Dark"}</span>
    </button>
  );
}
