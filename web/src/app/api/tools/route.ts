import { NextResponse } from "next/server";
import { adminFetch } from "@/lib/api";
import { sessionTokenOf } from "@/lib/proxy";

/** The tool registry, for the New run form. Header values never leave the Go service; only their names do. */
export async function GET(request: Request) {
  const token = sessionTokenOf(request);
  if (!token) return NextResponse.json({ code: "unauthorized", message: "Sign in to continue." }, { status: 401 });
  const res = await adminFetch<unknown>("/admin/tools", { token });
  if (res.ok) return NextResponse.json(res.data, { headers: { "Cache-Control": "no-store" } });
  return NextResponse.json({ code: res.code, message: res.message }, { status: res.status === 0 ? 502 : res.status });
}
