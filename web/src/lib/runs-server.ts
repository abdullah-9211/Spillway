import "server-only";
import { adminFetch, type ApiResult } from "./api";
import { showsActive, showsFinished, type ActivityBucket, type Filters, type Hours, type RunCounts, type RunItem, type Snapshot, type WaitingRun } from "./runs";

export const EARLIER_PAGE = 20;

type RunsPage = { runs: RunItem[]; next_cursor: string | null };

/** Everything the Runs page shows, from four reads of the Go service done at once. */
export async function loadSnapshot(token: string, hours: Hours, f: Filters): Promise<ApiResult<Snapshot>> {
  const narrow = new URLSearchParams();
  if (f.q) narrow.set("q", f.q);
  if (f.key) narrow.set("key_id", f.key);
  const fin = new URLSearchParams(narrow);
  if (f.status === "succeeded" || f.status === "failed" || f.status === "cancelled") fin.set("status", f.status);
  const empty: ApiResult<RunsPage> = { ok: true, status: 200, data: { runs: [], next_cursor: null } };
  const [summary, activity, active, finished] = await Promise.all([
    adminFetch<{ counts: RunCounts; waiting: WaitingRun[] }>(`/admin/runs/summary?hours=${hours}`, { token }),
    adminFetch<{ bucket_seconds: number; buckets: ActivityBucket[] }>(`/admin/runs/activity?hours=${hours}`, { token }),
    showsActive(f.status) ? adminFetch<RunsPage>(`/admin/runs?state=active&limit=50&${narrow}`, { token }) : empty,
    showsFinished(f.status) ? adminFetch<RunsPage>(`/admin/runs?state=finished&hours=${hours}&limit=${EARLIER_PAGE}&${fin}`, { token }) : empty,
  ]);
  for (const r of [summary, activity, active, finished]) if (!r.ok) return r;
  if (!summary.ok || !activity.ok || !active.ok || !finished.ok) throw new Error("unreachable");
  return {
    ok: true,
    status: 200,
    data: {
      hours,
      counts: summary.data.counts,
      waiting: summary.data.waiting,
      bucketSeconds: activity.data.bucket_seconds,
      buckets: activity.data.buckets,
      active: active.data.runs,
      finished: finished.data.runs,
      nextCursor: finished.data.next_cursor,
    },
  };
}
