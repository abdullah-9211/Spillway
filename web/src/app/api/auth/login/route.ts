import { NextResponse } from "next/server";
import { adminFetch, type LoginResponse } from "@/lib/api";
import { SESSION_COOKIE, cookieSecure } from "@/lib/cookies";

export async function POST(request: Request) {
  let body: { username?: unknown; password?: unknown };
  try {
    body = await request.json();
  } catch {
    return NextResponse.json({ code: "invalid_request", message: "Send a username and a password." }, { status: 400 });
  }
  if (typeof body.username !== "string" || typeof body.password !== "string" || !body.username || !body.password) {
    return NextResponse.json({ code: "invalid_request", message: "Enter a username and a password." }, { status: 400 });
  }

  const forwardedFor = request.headers.get("x-forwarded-for")?.split(",")[0]?.trim() || undefined;
  const res = await adminFetch<LoginResponse>("/admin/login", {
    method: "POST",
    body: { username: body.username, password: body.password },
    forwardedFor,
  });
  if (!res.ok) {
    const status = res.status === 0 ? 502 : res.status;
    return NextResponse.json(
      { code: res.code, message: res.message },
      { status, headers: res.retryAfter ? { "Retry-After": String(res.retryAfter) } : undefined },
    );
  }

  // The token goes into an httpOnly cookie and nowhere else: it is not in the response body.
  const out = NextResponse.json({ user: res.data.user });
  out.cookies.set(SESSION_COOKIE, res.data.token, {
    httpOnly: true,
    sameSite: "strict",
    secure: cookieSecure(),
    path: "/",
    expires: new Date(res.data.expires_at),
  });
  return out;
}
