import type { Metadata } from "next";
import { notFound } from "next/navigation";
import { RunPage } from "@/components/run/RunPage";
import { adminFetch } from "@/lib/api";
import type { RunGraph } from "@/lib/graph";
import { requireSession } from "@/lib/session";

export const metadata: Metadata = { title: "Run" };

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export default async function RunDetailPage({ params, searchParams }: { params: Promise<{ id: string }>; searchParams: Promise<{ view?: string }> }) {
  const { token } = await requireSession();
  const { id } = await params;
  if (!UUID.test(id)) notFound();
  const res = await adminFetch<RunGraph>(`/admin/runs/${id}/graph`, { token });
  if (!res.ok && res.status === 404) notFound();
  if (!res.ok) {
    return (
      <>
        <h1 className="h1">Run</h1>
        <div className="err" role="alert">
          <span>Could not load the run: {res.message}</span>
        </div>
      </>
    );
  }
  const view = (await searchParams).view === "timeline" ? "timeline" : "graph";
  return <RunPage key={id} initial={res.data} view={view} />;
}
