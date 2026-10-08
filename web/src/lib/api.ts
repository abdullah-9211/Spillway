import type { components } from "./api-types";

export type User = components["schemas"]["User"];
export type Role = components["schemas"]["Role"];
export type LoginResponse = components["schemas"]["LoginResponse"];
export type Status = components["schemas"]["Status"];

export type ApiResult<T> =
  | { ok: true; status: number; data: T }
  | { ok: false; status: number; code: string; message: string; retryAfter?: number };

export function adminUrl(): string {
  return (process.env.SPILLWAY_ADMIN_URL ?? "http://localhost:8080").replace(/\/$/, "");
}

type Options = {
  method?: "GET" | "POST" | "PATCH" | "PUT" | "DELETE";
  token?: string;
  body?: unknown;
  /** The browser's address, passed on so the Go service can throttle sign-ins per client. */
  forwardedFor?: string;
};

/**
 * Calls the Go admin API. Only server code uses this: the token travels from the server to the Go service
 * and never reaches the browser's JavaScript. Failures come back as values, not exceptions.
 */
export async function adminFetch<T>(path: string, opts: Options = {}): Promise<ApiResult<T>> {
  const headers: Record<string, string> = { Accept: "application/json" };
  if (opts.token) headers.Authorization = `Bearer ${opts.token}`;
  if (opts.body !== undefined) headers["Content-Type"] = "application/json";
  if (opts.forwardedFor) headers["X-Forwarded-For"] = opts.forwardedFor;

  let res: Response;
  try {
    res = await fetch(adminUrl() + path, {
      method: opts.method ?? "GET",
      headers,
      body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
      cache: "no-store",
    });
  } catch {
    return { ok: false, status: 0, code: "unreachable", message: "Cannot reach the Spillway service." };
  }

  let payload: unknown = null;
  try {
    payload = await res.json();
  } catch {
    // an empty or non-JSON body is handled below
  }
  if (res.ok) return { ok: true, status: res.status, data: payload as T };

  const err = (payload as components["schemas"]["Error"] | null)?.error;
  const retry = Number(res.headers.get("Retry-After"));
  return {
    ok: false,
    status: res.status,
    code: err?.code ?? "error",
    message: err?.message ?? `The service answered ${res.status}.`,
    ...(Number.isFinite(retry) && retry > 0 ? { retryAfter: retry } : {}),
  };
}
