import { NextResponse, type NextRequest } from "next/server";

const SESSION_COOKIE = "spillway_session";

/**
 * Sends signed-out visitors to /login before any page renders. It only checks that a session cookie exists;
 * whether it is still valid is decided by the Go service when the page asks who the user is.
 */
export function proxy(request: NextRequest) {
  if (request.cookies.has(SESSION_COOKIE)) return NextResponse.next();
  const url = new URL("/login", request.url);
  return NextResponse.redirect(url);
}

export const config = {
  matcher: ["/((?!login|api/auth|_next/static|_next/image|favicon.ico|icon.svg).*)"],
};
