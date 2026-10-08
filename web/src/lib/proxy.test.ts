import { afterEach, describe, expect, it, vi } from "vitest";
import { DELETE, PATCH } from "@/app/api/keys/[id]/route";
import { POST } from "@/app/api/keys/route";

afterEach(() => vi.unstubAllGlobals());

const req = (method: string, body?: string, cookie = "spillway_session=TOK; spillway_theme=dark") =>
  new Request("http://dash/api/keys", { method, body, headers: cookie ? { cookie } : {} });

function goReturns(res: Response) {
  const f = vi.fn(async () => res);
  vi.stubGlobal("fetch", f);
  return f;
}

describe("key routes", () => {
  it("forwards a create to the Go service with the session token", async () => {
    const f = goReturns(Response.json({ key: { id: "1" }, secret: "spw_x" }, { status: 201 }));
    const res = await POST(req("POST", '{"name":"a"}'));
    expect(res.status).toBe(201);
    expect(await res.json()).toEqual({ key: { id: "1" }, secret: "spw_x" });
    const [url, init] = f.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe("http://localhost:8080/admin/keys");
    expect(init.headers).toMatchObject({ Authorization: "Bearer TOK" });
    expect(init.body).toBe('{"name":"a"}');
  });

  it("sends PATCH and DELETE to the key's own path", async () => {
    const f = goReturns(Response.json({ id: "abc" }));
    await PATCH(req("PATCH", '{"name":"b"}'), { params: Promise.resolve({ id: "abc" }) });
    await DELETE(req("DELETE"), { params: Promise.resolve({ id: "abc" }) });
    const calls = f.mock.calls as unknown as [string, RequestInit][];
    expect(calls[0][0]).toBe("http://localhost:8080/admin/keys/abc");
    expect(calls[0][1].method).toBe("PATCH");
    expect(calls[1][0]).toBe("http://localhost:8080/admin/keys/abc");
    expect(calls[1][1].method).toBe("DELETE");
    expect(calls[1][1].body).toBeUndefined();
  });

  it("cannot be tricked into another path through the id", async () => {
    const f = goReturns(Response.json({}));
    await DELETE(req("DELETE"), { params: Promise.resolve({ id: "../users" }) });
    expect((f.mock.calls[0] as unknown as [string])[0]).toBe("http://localhost:8080/admin/keys/..%2Fusers");
  });

  it("refuses without a session and never calls the Go service", async () => {
    const f = vi.fn();
    vi.stubGlobal("fetch", f);
    const res = await POST(req("POST", "{}", ""));
    expect(res.status).toBe(401);
    expect(f).not.toHaveBeenCalled();
  });

  it("passes the Go service's refusal through as a message", async () => {
    goReturns(Response.json({ error: { code: "forbidden", message: "This needs the admin role." } }, { status: 403 }));
    const res = await POST(req("POST", '{"name":"a"}'));
    expect(res.status).toBe(403);
    expect(await res.json()).toEqual({ code: "forbidden", message: "This needs the admin role." });
  });

  it("rejects a body that is not JSON without calling the Go service", async () => {
    const f = vi.fn();
    vi.stubGlobal("fetch", f);
    expect((await POST(req("POST", "{"))).status).toBe(400);
    expect(f).not.toHaveBeenCalled();
  });

  it("answers 502 when the Go service is down", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => { throw new TypeError("down"); }));
    expect((await POST(req("POST", "{}"))).status).toBe(502);
  });
});
