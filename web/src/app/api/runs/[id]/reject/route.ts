import { NextResponse } from "next/server";
import { proxyAdmin } from "@/lib/proxy";

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/** Records a person's decision on a run that waits for one. The Go service decides who may (admins only). */
export async function POST(request: Request, { params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  if (!UUID.test(id)) return NextResponse.json({ code: "not_found", message: "No run with that id." }, { status: 404 });
  return proxyAdmin(request, "POST", `/admin/runs/${id}/reject`);
}
