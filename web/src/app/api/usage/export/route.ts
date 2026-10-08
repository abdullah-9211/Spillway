import { NextResponse } from "next/server";
import { adminUrl } from "@/lib/api";
import { SESSION_COOKIE } from "@/lib/cookies";

const DATE = /^\d{4}-\d{2}-\d{2}$/;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

function token(request: Request): string | null {
  for (const part of (request.headers.get("cookie") ?? "").split(";")) {
    const [k, ...rest] = part.trim().split("=");
    if (k === SESSION_COOKIE) return rest.join("=");
  }
  return null;
}

/** Streams the CSV from the Go service with the session token added on the server. Only known parameters are passed on. */
export async function GET(request: Request) {
  const t = token(request);
  if (!t) return NextResponse.json({ code: "unauthorized", message: "Sign in to continue." }, { status: 401 });

  const src = new URL(request.url).searchParams;
  const q = new URLSearchParams();
  for (const k of ["from", "to"]) {
    const v = src.get(k);
    if (v && DATE.test(v)) q.set(k, v);
  }
  const key = src.get("key_id");
  if (key && UUID.test(key)) q.set("key_id", key);

  let res: Response;
  try {
    res = await fetch(`${adminUrl()}/admin/usage/export.csv?${q}`, { headers: { Authorization: `Bearer ${t}` }, cache: "no-store" });
  } catch {
    return NextResponse.json({ code: "unreachable", message: "Cannot reach the Spillway service." }, { status: 502 });
  }
  if (!res.ok) {
    const body = (await res.json().catch(() => null)) as { error?: { code?: string; message?: string } } | null;
    return NextResponse.json({ code: body?.error?.code ?? "error", message: body?.error?.message ?? "The export failed." }, { status: res.status });
  }
  return new Response(res.body, {
    headers: {
      "Content-Type": "text/csv; charset=utf-8",
      "Content-Disposition": res.headers.get("Content-Disposition") ?? 'attachment; filename="spillway-usage.csv"',
      "Cache-Control": "no-store",
    },
  });
}
