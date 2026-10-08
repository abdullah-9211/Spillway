import { compact, count, ms, percent, type UsageSummary } from "@/lib/usage";
import { usd } from "@/lib/format";

function perDay(n: number, days: number) {
  return days > 0 ? n / days : 0;
}

/** The five headline figures for the range. */
export function Kpis({ summary }: { summary: UsageSummary }) {
  const t = summary.totals;
  const days = summary.range.days;
  const lat = summary.added_latency;
  return (
    <section className="panel kpis" aria-label={`Totals for the last ${days} ${days === 1 ? "day" : "days"}`}>
      <div className="kpi">
        <div className="kpi__l">Requests</div>
        <div className="kpi__v">{count(t.requests)}</div>
        <div className="kpi__d">{count(Math.round(perDay(t.requests, days)))} a day on average</div>
      </div>
      <div className="kpi">
        <div className="kpi__l">Tokens in and out</div>
        <div className="kpi__v">{compact(t.input_tokens + t.output_tokens)}</div>
        <div className="kpi__d">
          {compact(t.input_tokens)} in, {compact(t.output_tokens)} out
        </div>
      </div>
      <div className="kpi">
        <div className="kpi__l">Spend</div>
        <div className="kpi__v">{usd(t.cost_usd)}</div>
        <div className="kpi__d">{usd((Number(t.cost_usd) / Math.max(days, 1)).toFixed(6))} a day on average</div>
      </div>
      <div className="kpi">
        <div className="kpi__l">Saved by cache</div>
        <div className="kpi__v">{usd(t.saved_usd)}</div>
        <div className="kpi__d">{Math.round(t.saved_share * 100)}% of what you would have spent</div>
      </div>
      <div className="kpi" title="Time spent in Spillway itself, not the provider. Covers all keys since the service started.">
        <div className="kpi__l">Added latency, p95</div>
        <div className="kpi__v">{lat ? ms(lat.p95_ms) : "—"}</div>
        <div className="kpi__d">
          {lat ? `p50 ${ms(lat.p50_ms)}, p99 ${ms(lat.p99_ms)}, since the service started` : "No requests since the service started"}
        </div>
      </div>
    </section>
  );
}

export { percent };
