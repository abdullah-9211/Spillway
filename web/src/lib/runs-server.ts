import "server-only";
import { adminFetch, type ApiResult } from "./api";
import type { ActivityBucket, Hours, RunCounts, RunItem, Snapshot, WaitingRun } from "./runs";

export const EARLIER_PAGE = 20;

type RunsPage = { runs: RunItem[]; next_cursor: string | null };

/** Everything the Runs page shows, from four reads of the Go service done at once. */
export async function loadSnapshot(token: string, hours: Hours): Promise<ApiResult<Snapshot>> {
  const [summary, activity, active, finished] = await Promise.all([
    adminFetch<{ counts: RunCounts; waiting: WaitingRun[] }>(`/admin/runs/summary?hours=${hours}`, { token }),
    adminFetch<{ bucket_seconds: number; buckets: ActivityBucket[] }>(`/admin/runs/activity?hours=${hours}`, { token }),
    adminFetch<RunsPage>(`/admin/runs?state=active&limit=50`, { token }),
    adminFetch<RunsPage>(`/admin/runs?state=finished&hours=${hours}&limit=${EARLIER_PAGE}`, { token }),
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
