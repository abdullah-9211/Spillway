import { NextResponse } from "next/server";
import { adminFetch } from "@/lib/api";
import { sessionTokenOf } from "@/lib/proxy";

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export async function GET(request: Request, { params }: { params: Promise<{ id: string }> }) {
  const token = sessionTokenOf(request);
  if (!token) return NextResponse.json({ code: "unauthorized", message: "Sign in to continue." }, { status: 401 });
  const { id } = await params;
  if (!UUID.test(id)) return NextResponse.json({ code: "not_found", message: "No run with that id." }, { status: 404 });
  const res = await adminFetch<unknown>(`/admin/runs/${id}/graph`, { token });
  if (res.ok) return NextResponse.json(res.data, { headers: { "Cache-Control": "no-store" } });
  return NextResponse.json({ code: res.code, message: res.message }, { status: res.status === 0 ? 502 : res.status });
}
