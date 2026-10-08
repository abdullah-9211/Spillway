import { proxyAdmin } from "@/lib/proxy";

type Ctx = { params: Promise<{ id: string }> };

export async function PATCH(request: Request, { params }: Ctx) {
  const { id } = await params;
  return proxyAdmin(request, "PATCH", `/admin/keys/${encodeURIComponent(id)}`);
}

export async function DELETE(request: Request, { params }: Ctx) {
  const { id } = await params;
  return proxyAdmin(request, "DELETE", `/admin/keys/${encodeURIComponent(id)}`);
}
