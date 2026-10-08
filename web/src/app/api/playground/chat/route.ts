import { proxyAdmin } from "@/lib/proxy";

export async function POST(request: Request) {
  return proxyAdmin(request, "POST", "/admin/playground/chat");
}
