import { afterEach, describe, expect, it, vi } from "vitest";
import { adminFetch, adminUrl } from "./api";

function mockFetch(res: Response | Error) {
  const f = vi.fn(async () => {
    if (res instanceof Error) throw res;
    return res;
  });
  vi.stubGlobal("fetch", f);
  return f;
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.unstubAllEnvs();
});

describe("adminFetch", () => {
  it("calls the configured URL with the bearer token and the client address", async () => {
    vi.stubEnv("SPILLWAY_ADMIN_URL", "http://go:8080/");
    const f = mockFetch(Response.json({ username: "a", role: "admin" }));
    const r = await adminFetch("/admin/me", { token: "tok", forwardedFor: "1.2.3.4" });
    expect(r).toEqual({ ok: true, status: 200, data: { username: "a", role: "admin" } });
    const [url, init] = f.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe("http://go:8080/admin/me");
    expect(init.headers).toMatchObject({ Authorization: "Bearer tok", "X-Forwarded-For": "1.2.3.4" });
    expect(init.cache).toBe("no-store");
  });

  it("sends JSON bodies", async () => {
    const f = mockFetch(Response.json({}));
    await adminFetch("/admin/login", { method: "POST", body: { username: "u", password: "p" } });
    const init = (f.mock.calls[0] as unknown as [string, RequestInit])[1];
    expect(init.method).toBe("POST");
    expect(init.body).toBe('{"username":"u","password":"p"}');
    expect(init.headers).toMatchObject({ "Content-Type": "application/json" });
  });

  it("does not send an Authorization header without a token", async () => {
    const f = mockFetch(Response.json({}));
    await adminFetch("/admin/login");
    const init = (f.mock.calls[0] as unknown as [string, RequestInit])[1];
    expect(init.headers).not.toHaveProperty("Authorization");
  });

  it("turns an error body into a value", async () => {
    mockFetch(Response.json({ error: { code: "invalid_credentials", message: "nope" } }, { status: 401 }));
    expect(await adminFetch("/admin/login")).toEqual({ ok: false, status: 401, code: "invalid_credentials", message: "nope" });
  });

  it("carries Retry-After on a 429", async () => {
    mockFetch(Response.json({ error: { code: "too_many_attempts", message: "slow down" } }, { status: 429, headers: { "Retry-After": "42" } }));
    expect(await adminFetch("/admin/login")).toMatchObject({ ok: false, status: 429, retryAfter: 42 });
  });

  it("copes with a non-JSON error", async () => {
    mockFetch(new Response("<html>bad gateway</html>", { status: 502 }));
    expect(await adminFetch("/admin/me")).toMatchObject({ ok: false, status: 502, code: "error" });
  });

  it("reports an unreachable service instead of throwing", async () => {
    mockFetch(new TypeError("fetch failed"));
    expect(await adminFetch("/admin/me")).toMatchObject({ ok: false, status: 0, code: "unreachable" });
  });

  it("defaults the URL to localhost", () => {
    vi.stubEnv("SPILLWAY_ADMIN_URL", "");
    delete process.env.SPILLWAY_ADMIN_URL;
    expect(adminUrl()).toBe("http://localhost:8080");
  });
});
