import type { Metadata } from "next";
import { RunsView } from "@/components/runs/RunsView";
import { parseFilters, parseHours } from "@/lib/runs";
import { loadSnapshot } from "@/lib/runs-server";
import { adminFetch } from "@/lib/api";
import type { ApiKey } from "@/lib/format";
import { requireSession } from "@/lib/session";

export const metadata: Metadata = { title: "Runs" };

export default async function RunsPage({ searchParams }: { searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  const { token } = await requireSession();
  const params = await searchParams;
  const hours = parseHours(params.hours);
  const filters = parseFilters(params);
  const [res, models, keys] = await Promise.all([
    loadSnapshot(token, hours, filters),
    adminFetch<{ policies: { name: string; description: string }[] }>("/admin/models", { token }),
    adminFetch<{ keys: ApiKey[] }>("/admin/keys", { token }),
  ]);
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
  const policies = models.ok ? models.data.policies.map((p) => ({ name: p.name, description: p.description })) : [{ name: "default", description: "" }];
  const keyChoices = keys.ok ? keys.data.keys.filter((k) => !k.revoked_at || k.builtin).map((k) => ({ id: k.id, name: k.name })) : [];
  return <RunsView key={hours} initial={res.data} initialFilters={filters} policies={policies} keys={keyChoices} />;
}
