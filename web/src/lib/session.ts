import "server-only";
import { cookies } from "next/headers";
import { redirect } from "next/navigation";
import { adminFetch, type User } from "./api";

import { SESSION_COOKIE } from "./cookies";

export async function sessionToken(): Promise<string | null> {
  return (await cookies()).get(SESSION_COOKIE)?.value ?? null;
}

export type Session = { token: string; user: User };

/** The signed-in user, asked of the Go service so the role is never taken on trust from the cookie alone. */
export async function getSession(): Promise<Session | null> {
  const token = await sessionToken();
  if (!token) return null;
  const me = await adminFetch<User>("/admin/me", { token });
  return me.ok ? { token, user: me.data } : null;
}

/** For pages behind sign-in. A stale or invalid cookie is cleared by the logout route on the way to /login. */
export async function requireSession(): Promise<Session> {
  const s = await getSession();
  if (!s) redirect("/api/auth/logout");
  return s;
}
