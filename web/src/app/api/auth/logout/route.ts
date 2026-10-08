import { NextResponse } from "next/server";
import { SESSION_COOKIE } from "@/lib/cookies";

function clear(request: Request) {
  const out = NextResponse.redirect(new URL("/login", request.url), 303);
  out.cookies.delete(SESSION_COOKIE);
  return out;
}

// GET lets a page with a stale cookie redirect here; POST is what the Sign out button uses.
export const GET = clear;
export const POST = clear;
