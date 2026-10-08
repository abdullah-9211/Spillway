import { afterEach, describe, expect, it, vi } from "vitest";
import { POST } from "./route";

const post = (body: unknown, headers: Record<string, string> = {}) =>
  new Request("http://dash/api/auth/login", { method: "POST", body: typeof body === "string" ? body : JSON.stringify(body), headers });

afterEach(() => {
  vi.unstubAllGlobals();
  vi.unstubAllEnvs();
});

const goLogin = {
  token: "SECRET.TOKEN",
  expires_at: "2030-01-01T12:00:00Z",
  user: { username: "admin", role: "admin" },
};

describe("POST /api/auth/login", () => {
  it("sets an httpOnly, SameSite=Strict session cookie and keeps the token out of the body", async () => {
    vi.stubEnv("SESSION_COOKIE_SECURE", "true");
    vi.stubGlobal("fetch", vi.fn(async () => Response.json(goLogin)));
    const res = await POST(post({ username: "admin", password: "pw" }));
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ user: { username: "admin", role: "admin" } });
    const cookie = res.headers.get("set-cookie") ?? "";
    expect(cookie).toContain("spillway_session=SECRET.TOKEN");
    expect(cookie).toMatch(/HttpOnly/i);
    expect(cookie).toMatch(/SameSite=strict/i);
    expect(cookie).toMatch(/Secure/i);
    expect(cookie).toContain("Path=/");
    expect(cookie).toMatch(/Expires=/i);
  });

  it("is not Secure when SESSION_COOKIE_SECURE=false (plain http in development)", async () => {
    vi.stubEnv("SESSION_COOKIE_SECURE", "false");
    vi.stubGlobal("fetch", vi.fn(async () => Response.json(goLogin)));
    const res = await POST(post({ username: "admin", password: "pw" }));
    expect(res.headers.get("set-cookie")).not.toMatch(/Secure/i);
  });

  it("passes the browser address to the Go service", async () => {
    const f = vi.fn(async () => Response.json(goLogin));
    vi.stubGlobal("fetch", f);
    await POST(post({ username: "a", password: "b" }, { "x-forwarded-for": "9.9.9.9, 10.0.0.1" }));
    const init = (f.mock.calls[0] as unknown as [string, RequestInit])[1];
    expect(init.headers).toMatchObject({ "X-Forwarded-For": "9.9.9.9" });
  });

  it("relays a failed sign-in without a cookie", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => Response.json({ error: { code: "invalid_credentials", message: "no" } }, { status: 401 })));
    const res = await POST(post({ username: "a", password: "wrong" }));
    expect(res.status).toBe(401);
    expect(await res.json()).toMatchObject({ code: "invalid_credentials" });
    expect(res.headers.get("set-cookie")).toBeNull();
  });

  it("relays throttling with Retry-After", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => Response.json({ error: { code: "too_many_attempts", message: "wait" } }, { status: 429, headers: { "Retry-After": "30" } })));
    const res = await POST(post({ username: "a", password: "b" }));
    expect(res.status).toBe(429);
    expect(res.headers.get("retry-after")).toBe("30");
  });

  it("answers 502 when the Go service is unreachable", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => { throw new TypeError("down"); }));
    const res = await POST(post({ username: "a", password: "b" }));
    expect(res.status).toBe(502);
    expect(res.headers.get("set-cookie")).toBeNull();
  });

  it.each([["not json", "{"], ["empty", {}], ["no password", { username: "a" }], ["numbers", { username: 1, password: 2 }]])("rejects a bad body: %s", async (_n, body) => {
    const f = vi.fn();
    vi.stubGlobal("fetch", f);
    const res = await POST(post(body));
    expect(res.status).toBe(400);
    expect(f).not.toHaveBeenCalled();
  });
});
