import { usd } from "@/lib/format";
import { chartSeries, compact, count, ms, percent, sumUsd, type UsageGroup } from "@/lib/usage";

/** The by-model breakdown, highest spend first, with a totals row that adds up to the chart and the headline figures. */
export function ModelTable({ models, days }: { models: UsageGroup[]; days: UsageGroup[] }) {
  const { colorOf } = chartSeries(days, models);
  const sum = (f: (m: UsageGroup) => number) => models.reduce((a, m) => a + f(m), 0);
  const reqs = sum((m) => m.requests);
  return (
    <section className="panel pad" aria-label="Breakdown by model">
      <div className="ph">
        <h2>By model</h2>
        <span className="mute">Sorted by spend</span>
      </div>
      {models.length === 0 ? (
        <p className="mute">No requests in this range.</p>
      ) : (
        <div className="scroll">
          <table className="t">
            <thead>
              <tr>
                <th scope="col">Model</th>
                <th scope="col">Requests</th>
                <th scope="col">Tokens in</th>
                <th scope="col">Tokens out</th>
                <th scope="col">Cost</th>
                <th scope="col" title="Successful requests answered by a provider, not by a cache">p50</th>
                <th scope="col">p95</th>
                <th scope="col">Cache hit</th>
                <th scope="col">Errors</th>
              </tr>
            </thead>
            <tbody>
              {models.map((m) => (
                <tr key={m.key || "none"}>
                  <th scope="row">
                    <span className="mname">
                      <i className="sw" style={{ background: colorOf(m.key) }} />
                      {m.label}
                    </span>
                  </th>
                  <td>{count(m.requests)}</td>
                  <td>{compact(m.input_tokens)}</td>
                  <td>{compact(m.output_tokens)}</td>
                  <td>{usd(m.cost_usd)}</td>
                  <td>{ms(m.p50_ms)}</td>
                  <td>{ms(m.p95_ms)}</td>
                  <td>{percent(m.cache_hits, m.requests)}</td>
                  <td>{percent(m.errors, m.requests, 1)}</td>
                </tr>
              ))}
            </tbody>
            <tfoot>
              <tr>
                <th scope="row">Total</th>
                <td>{count(reqs)}</td>
                <td>{compact(sum((m) => m.input_tokens))}</td>
                <td>{compact(sum((m) => m.output_tokens))}</td>
                <td>{usd(sumUsd(models.map((m) => m.cost_usd)))}</td>
                <td />
                <td />
                <td>{percent(sum((m) => m.cache_hits), reqs)}</td>
                <td>{percent(sum((m) => m.errors), reqs, 1)}</td>
              </tr>
            </tfoot>
          </table>
        </div>
      )}
    </section>
  );
}
