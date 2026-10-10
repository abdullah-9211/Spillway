import { NextResponse } from "next/server";
import { adminFetch } from "@/lib/api";
import { sessionTokenOf } from "@/lib/proxy";
import { EARLIER_PAGE } from "@/lib/runs-server";
import { parseFilters, parseHours, type RunItem } from "@/lib/runs";

/** The next page of finished runs, after a cursor. Only the known parameters are passed on. */
export async function GET(request: Request) {
  const token = sessionTokenOf(request);
  if (!token) return NextResponse.json({ code: "unauthorized", message: "Sign in to continue." }, { status: 401 });
  const q = new URL(request.url).searchParams;
  const hours = parseHours(q.get("hours"));
  const cursor = q.get("cursor");
  const params = new URLSearchParams({ state: "finished", hours: String(hours), limit: String(EARLIER_PAGE) });
  if (cursor && /^[A-Za-z0-9_=-]{1,200}$/.test(cursor)) params.set("cursor", cursor);
  const f = parseFilters(q);
  if (f.q) params.set("q", f.q);
  if (f.key) params.set("key_id", f.key);
  if (f.status === "succeeded" || f.status === "failed" || f.status === "cancelled") params.set("status", f.status);
  const res = await adminFetch<{ runs: RunItem[]; next_cursor: string | null }>(`/admin/runs?${params}`, { token });
  if (res.ok) return NextResponse.json(res.data, { headers: { "Cache-Control": "no-store" } });
  return NextResponse.json({ code: res.code, message: res.message }, { status: res.status === 0 ? 502 : res.status });
}
