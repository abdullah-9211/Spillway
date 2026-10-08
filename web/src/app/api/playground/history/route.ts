import { NextResponse } from "next/server";
import { adminFetch } from "@/lib/api";
import { SESSION_COOKIE } from "@/lib/cookies";

function token(request: Request): string | null {
  for (const part of (request.headers.get("cookie") ?? "").split(";")) {
    const [k, ...rest] = part.trim().split("=");
    if (k === SESSION_COOKIE) return rest.join("=");
  }
  return null;
}

/** One page of past playground requests. Only the paging parameters are passed on. */
export async function GET(request: Request) {
  const t = token(request);
  if (!t) return NextResponse.json({ code: "unauthorized", message: "Sign in to continue." }, { status: 401 });
  const src = new URL(request.url).searchParams;
  const q = new URLSearchParams();
  const limit = src.get("limit");
  if (limit && /^\d{1,3}$/.test(limit)) q.set("limit", limit);
  const cursor = src.get("cursor");
  if (cursor && /^[A-Za-z0-9_=-]{1,200}$/.test(cursor)) q.set("cursor", cursor);

  const res = await adminFetch<unknown>(`/admin/playground/history?${q}`, { token: t });
  if (res.ok) return NextResponse.json(res.data);
  return NextResponse.json({ code: res.code, message: res.message }, { status: res.status === 0 ? 502 : res.status });
}
