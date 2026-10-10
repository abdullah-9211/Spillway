import { proxyAdmin } from "@/lib/proxy";

/** Starts a run from the dashboard. The Go service decides who may (admins) and which limits apply. */
export async function POST(request: Request) {
  return proxyAdmin(request, "POST", "/admin/runs");
}
