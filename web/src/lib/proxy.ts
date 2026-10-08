import { NextResponse } from "next/server";
import { adminFetch } from "./api";
import { SESSION_COOKIE } from "./cookies";

function cookieValue(header: string | null, name: string): string | null {
  for (const part of (header ?? "").split(";")) {
    const [k, ...rest] = part.trim().split("=");
    if (k === name) return rest.join("=");
  }
  return null;
}

/**
 * Forwards a browser call to the Go admin API with the session token attached on the server. The browser's own
 * JavaScript never holds the token. The Go service decides what the role may do; this adds no rules of its own.
 */
export async function proxyAdmin(request: Request, method: "POST" | "PATCH" | "DELETE", path: string) {
  const token = cookieValue(request.headers.get("cookie"), SESSION_COOKIE);
  if (!token) return NextResponse.json({ code: "unauthorized", message: "Sign in to continue." }, { status: 401 });

  let body: unknown;
  if (method !== "DELETE") {
    try {
      body = await request.json();
    } catch {
      return NextResponse.json({ code: "invalid_request", message: "The request body is not valid JSON." }, { status: 400 });
    }
  }
  const res = await adminFetch<unknown>(path, { method, token, body });
  if (res.ok) return NextResponse.json(res.data, { status: res.status });
  return NextResponse.json({ code: res.code, message: res.message }, { status: res.status === 0 ? 502 : res.status });
}
