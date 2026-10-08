export const SESSION_COOKIE = "spillway_session";

/** The cookie is `Secure` unless SESSION_COOKIE_SECURE=false, which local development over http needs. */
export function cookieSecure(): boolean {
  return process.env.SESSION_COOKIE_SECURE !== "false";
}
