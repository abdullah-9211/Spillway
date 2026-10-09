import type { Metadata } from "next";
import Link from "next/link";
import { notFound } from "next/navigation";
import { RunTag, StepStrip } from "@/components/runs/parts";
import { Panel } from "@/components/ui";
import { adminFetch } from "@/lib/api";
import { durationLabel, reasonLabel, runCost, type RunItem } from "@/lib/runs";
import { requireSession } from "@/lib/session";

export const metadata: Metadata = { title: "Run" };

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export default async function RunPage({ params }: { params: Promise<{ id: string }> }) {
  const { token } = await requireSession();
  const { id } = await params;
  if (!UUID.test(id)) notFound();
  const res = await adminFetch<RunItem>(`/admin/runs/${id}`, { token });
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
  const r = res.data;
  const now = new Date();
  return (
    <>
      <p className="small">
        <Link href="/">← All runs</Link>
      </p>
      <div className="head">
        <h1 className="h1 h1--run">{r.goal}</h1>
        <RunTag run={r} now={now} />
      </div>
      <Panel className="soon rundetail">
        <dl className="facts-list num">
          <div><dt>Steps</dt><dd>{r.step_count}</dd></div>
          <div><dt>Cost</dt><dd>{runCost(r.cost_usd)}</dd></div>
          <div><dt>{r.finished_at ? "Took" : "Running for"}</dt><dd>{durationLabel(r.finished_at ? r.duration_ms : now.getTime() - new Date(r.created_at).getTime())}</dd></div>
          <div><dt>API key</dt><dd>{r.key}</dd></div>
          <div><dt>Model</dt><dd><span className="mono">{r.model}</span></dd></div>
          {r.failure_reason && <div><dt>Ended because</dt><dd>{reasonLabel(r.failure_reason)}</dd></div>}
        </dl>
        <StepStrip strip={r.strip} />
        <p className="mute">The live run graph and the step inspector arrive in phase 11.</p>
      </Panel>
    </>
  );
}
