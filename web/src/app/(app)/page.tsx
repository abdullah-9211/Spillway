import type { Metadata } from "next";
import { RunsView } from "@/components/runs/RunsView";
import { parseHours } from "@/lib/runs";
import { loadSnapshot } from "@/lib/runs-server";
import { requireSession } from "@/lib/session";

export const metadata: Metadata = { title: "Runs" };

export default async function RunsPage({ searchParams }: { searchParams: Promise<{ hours?: string }> }) {
  const { token } = await requireSession();
  const hours = parseHours((await searchParams).hours);
  const res = await loadSnapshot(token, hours);
  if (!res.ok) {
    return (
      <>
        <h1 className="h1">Runs</h1>
        <div className="err" role="alert">
          <span>Could not load the runs: {res.message}</span>
        </div>
      </>
    );
  }
  return <RunsView key={hours} initial={res.data} />;
}
