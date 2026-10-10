import { NextResponse } from "next/server";
import { adminUrl } from "@/lib/api";
import { sessionTokenOf } from "@/lib/proxy";

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/**
 * The run's event stream, passed through with the session token added on the server. The browser's EventSource sends
 * Last-Event-ID when it reconnects; it is forwarded so the Go service resumes from that row.
 */
export async function GET(request: Request, { params }: { params: Promise<{ id: string }> }) {
  const token = sessionTokenOf(request);
  if (!token) return NextResponse.json({ code: "unauthorized", message: "Sign in to continue." }, { status: 401 });
  const { id } = await params;
  if (!UUID.test(id)) return NextResponse.json({ code: "not_found", message: "No run with that id." }, { status: 404 });

  const headers: Record<string, string> = { Authorization: `Bearer ${token}`, Accept: "text/event-stream" };
  const last = request.headers.get("last-event-id");
  if (last && /^\d{1,18}$/.test(last)) headers["Last-Event-ID"] = last;
  const after = new URL(request.url).searchParams.get("after");
  const q = after && /^\d{1,18}$/.test(after) ? `?after=${after}` : "";

  let upstream: Response;
  try {
    upstream = await fetch(`${adminUrl()}/admin/runs/${id}/events${q}`, { headers, cache: "no-store", signal: request.signal });
  } catch {
    return NextResponse.json({ code: "unreachable", message: "Cannot reach the Spillway service." }, { status: 502 });
  }
  if (!upstream.ok || !upstream.body) {
    const body = (await upstream.json().catch(() => null)) as { error?: { code?: string; message?: string } } | null;
    return NextResponse.json({ code: body?.error?.code ?? "error", message: body?.error?.message ?? "The event stream is not available." }, { status: upstream.status });
  }
  return new Response(upstream.body, {
    headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache, no-transform", "X-Accel-Buffering": "no", Connection: "keep-alive" },
  });
}
