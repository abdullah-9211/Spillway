import { NextResponse } from "next/server";
import { sessionTokenOf } from "@/lib/proxy";
import { parseHours } from "@/lib/runs";
import { loadSnapshot } from "@/lib/runs-server";

/** The Runs page asks for this every couple of seconds. The session token is added here, on the server. */
export async function GET(request: Request) {
  const token = sessionTokenOf(request);
  if (!token) return NextResponse.json({ code: "unauthorized", message: "Sign in to continue." }, { status: 401 });
  const res = await loadSnapshot(token, parseHours(new URL(request.url).searchParams.get("hours")));
  if (res.ok) return NextResponse.json(res.data, { headers: { "Cache-Control": "no-store" } });
  return NextResponse.json({ code: res.code, message: res.message }, { status: res.status === 0 ? 502 : res.status });
}
