import type { Metadata } from "next";
import { CacheAndHealth } from "@/components/usage/CacheAndHealth";
import { Kpis } from "@/components/usage/Kpis";
import { ModelTable } from "@/components/usage/ModelTable";
import { SpendChart } from "@/components/usage/SpendChart";
import { UsageControls } from "@/components/usage/UsageControls";
import { adminFetch } from "@/lib/api";
import type { ApiKey } from "@/lib/format";
import { requireSession } from "@/lib/session";
import { parsePreset, rangeFor, type UsageSummary } from "@/lib/usage";

export const metadata: Metadata = { title: "Usage and cost" };

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export default async function UsagePage({ searchParams }: { searchParams: Promise<{ range?: string; key?: string }> }) {
  const { token } = await requireSession();
  const sp = await searchParams;
  const preset = parsePreset(sp.range);
  const keyId = sp.key && UUID.test(sp.key) ? sp.key : "";
  const { from, to } = rangeFor(preset);
  const query = new URLSearchParams({ from, to });
  if (keyId) query.set("key_id", keyId);

  const [byDay, byModel, keys] = await Promise.all([
    adminFetch<UsageSummary>(`/admin/usage/summary?${query}&group_by=day`, { token }),
    adminFetch<UsageSummary>(`/admin/usage/summary?${query}&group_by=model`, { token }),
    adminFetch<{ keys: ApiKey[] }>("/admin/keys", { token }),
  ]);

  const keyOptions = keys.ok ? keys.data.keys.filter((k) => !k.builtin || k.id === keyId).map((k) => ({ id: k.id, name: k.revoked_at ? `${k.name} (revoked)` : k.name })) : [];
  const keyName = keyOptions.find((k) => k.id === keyId)?.name;

  const failure = [byDay, byModel].find((r) => !r.ok);
  const ok = byDay.ok && byModel.ok;

  return (
    <>
      <div className="head head--end">
        <div>
          <h1 className="h1">Usage and cost</h1>
          <p className="sub">
            Spend, speed and cache savings across {keyName ? <strong>{keyName}</strong> : "every API key"}. Days are UTC.
          </p>
        </div>
        <UsageControls preset={preset} keyId={keyId} keys={keyOptions} exportQuery={query.toString()} />
      </div>

      {failure && !failure.ok ? (
        <div className="err" role="alert">
          <span>Could not load the usage report: {failure.message}</span>
        </div>
      ) : ok ? (
        <>
          <Kpis summary={byDay.data} />
          <div className="two">
            <SpendChart days={byDay.data.groups} byModel={byModel.data.groups} />
            <CacheAndHealth summary={byModel.data} />
          </div>
          <ModelTable models={byModel.data.groups} days={byDay.data.groups} />
        </>
      ) : null}
    </>
  );
}
