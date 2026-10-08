export type Theme = "dark" | "light";

export const THEME_COOKIE = "spillway_theme";

export function parseTheme(value: string | null | undefined): Theme | null {
  return value === "dark" || value === "light" ? value : null;
}

export function nextTheme(t: Theme): Theme {
  return t === "dark" ? "light" : "dark";
}

/** The theme to use when the user has not chosen one: their system preference. */
export function systemTheme(prefersLight: boolean): Theme {
  return prefersLight ? "light" : "dark";
}

export function themeCookie(t: Theme): string {
  return `${THEME_COOKIE}=${t}; Path=/; Max-Age=31536000; SameSite=Lax`;
}

export function readThemeCookie(cookieHeader: string): Theme | null {
  const m = cookieHeader.match(new RegExp(`(?:^|; )${THEME_COOKIE}=([^;]*)`));
  return parseTheme(m?.[1]);
}

/**
 * Runs in <head> before the first paint, so the page never flashes the wrong theme: the saved choice if there
 * is one, otherwise the system preference. Kept as plain ES5 text because it is inlined.
 */
export const themeBootScript = `(function(){try{var m=document.cookie.match(/(?:^|; )${THEME_COOKIE}=(dark|light)/);var t=m?m[1]:(window.matchMedia('(prefers-color-scheme: light)').matches?'light':'dark');document.documentElement.setAttribute('data-theme',t)}catch(e){}})()`;
