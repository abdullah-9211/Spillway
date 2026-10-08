import type { Metadata } from "next";
import { CacheAndHealth } from "@/components/usage/CacheAndHealth";
import { Kpis } from "@/components/usage/Kpis";
import { ModelTable } from "@/components/usage/ModelTable";
import { UsageChart } from "@/components/usage/UsageChart";
import { UsageControls } from "@/components/usage/UsageControls";
import { adminFetch } from "@/lib/api";
import type { ApiKey } from "@/lib/format";
import { requireSession } from "@/lib/session";
import { keyChoices, parseCustom, parsePreset, rangeFor, type UsageSummary } from "@/lib/usage";

export const metadata: Metadata = { title: "Usage and cost" };

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export default async function UsagePage({ searchParams }: { searchParams: Promise<{ range?: string; key?: string; from?: string; to?: string }> }) {
  const { token } = await requireSession();
  const sp = await searchParams;
  const preset = parsePreset(sp.range);
  const keyId = sp.key && UUID.test(sp.key) ? sp.key : "";
  let rangeError = "";
  let { from, to } = rangeFor(preset === "custom" ? "14d" : preset);
  if (preset === "custom") {
    const c = parseCustom(sp.from, sp.to);
    if ("error" in c) {
      rangeError = c.error; // keep the custom fields open, showing the last 14 days underneath
    } else {
      ({ from, to } = c);
    }
  }
  const today = rangeFor("7d").to;
  const query = new URLSearchParams({ from, to });
  if (keyId) query.set("key_id", keyId);

  const [byDay, byDayKey, byModel, keys] = await Promise.all([
    adminFetch<UsageSummary>(`/admin/usage/summary?${query}&group_by=day`, { token }),
    adminFetch<UsageSummary>(`/admin/usage/summary?${query}&group_by=day&stack=key`, { token }),
    adminFetch<UsageSummary>(`/admin/usage/summary?${query}&group_by=model`, { token }),
    adminFetch<{ keys: ApiKey[] }>("/admin/keys", { token }),
  ]);

  const keyOptions = keys.ok ? keyChoices(keys.data.keys, keyId) : [];
  const keyName = keyOptions.find((k) => k.id === keyId)?.name;

  const failure = [byDay, byDayKey, byModel].find((r) => !r.ok);
  const ok = byDay.ok && byDayKey.ok && byModel.ok;

  return (
    <>
      <div className="head head--end">
        <div>
          <h1 className="h1">Usage and cost</h1>
          <p className="sub">
            Spend, speed and cache savings across {keyName ? <strong>{keyName}</strong> : "every API key"}, {from} to {to}. Days are UTC.
          </p>
        </div>
        <UsageControls preset={preset} from={from} to={to} today={today} keyId={keyId} keys={keyOptions} exportQuery={query.toString()} />
      </div>

      {rangeError && (
        <div className="err" role="alert">
          <span>{rangeError} Showing the last 14 days instead.</span>
        </div>
      )}
      {failure && !failure.ok ? (
        <div className="err" role="alert">
          <span>Could not load the usage report: {failure.message}</span>
        </div>
      ) : ok ? (
        <>
          <Kpis summary={byDay.data} />
          <div className="two">
            <UsageChart dayModel={byDay.data.groups} dayKey={byDayKey.ok ? byDayKey.data.groups : []} keyId={keyId} />
            <CacheAndHealth summary={byModel.data} />
          </div>
          <ModelTable models={byModel.data.groups} />
        </>
      ) : null}
    </>
  );
}
