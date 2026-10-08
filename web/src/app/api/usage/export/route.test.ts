import { afterEach, describe, expect, it, vi } from "vitest";
import { GET } from "./route";

afterEach(() => vi.unstubAllGlobals());

const req = (qs: string, cookie = "spillway_session=TOK") => new Request(`http://dash/api/usage/export?${qs}`, { headers: cookie ? { cookie } : {} });

describe("GET /api/usage/export", () => {
  it("streams the CSV from the Go service with the session token and a download name", async () => {
    const f = vi.fn(async () => new Response("day,key\n2026-10-08,ci\n", { headers: { "Content-Type": "text/csv", "Content-Disposition": 'attachment; filename="x.csv"' } }));
    vi.stubGlobal("fetch", f);
    const res = await GET(req("from=2026-10-01&to=2026-10-08&key_id=11111111-2222-3333-4444-555555555555"));
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toContain("text/csv");
    expect(res.headers.get("content-disposition")).toBe('attachment; filename="x.csv"');
    expect(await res.text()).toBe("day,key\n2026-10-08,ci\n");
    const [url, init] = f.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe("http://localhost:8080/admin/usage/export.csv?from=2026-10-01&to=2026-10-08&key_id=11111111-2222-3333-4444-555555555555");
    expect(init.headers).toMatchObject({ Authorization: "Bearer TOK" });
  });

  it("passes on only parameters it understands", async () => {
    const f = vi.fn(async () => new Response("", { headers: { "Content-Type": "text/csv" } }));
    vi.stubGlobal("fetch", f);
    await GET(req("from=../../etc&to=2026-10-08&key_id=not-a-uuid&evil=1&group_by=key"));
    expect((f.mock.calls[0] as unknown as [string])[0]).toBe("http://localhost:8080/admin/usage/export.csv?to=2026-10-08");
  });

  it("needs a session", async () => {
    const f = vi.fn();
    vi.stubGlobal("fetch", f);
    expect((await GET(req("", ""))).status).toBe(401);
    expect(f).not.toHaveBeenCalled();
  });

  it("relays the service's refusal and its absence", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => Response.json({ error: { code: "invalid_request", message: "from is after to" } }, { status: 400 })));
    const bad = await GET(req("from=2026-10-09&to=2026-10-01"));
    expect(bad.status).toBe(400);
    expect(await bad.json()).toEqual({ code: "invalid_request", message: "from is after to" });
    vi.stubGlobal("fetch", vi.fn(async () => { throw new TypeError("down"); }));
    expect((await GET(req(""))).status).toBe(502);
  });
});
