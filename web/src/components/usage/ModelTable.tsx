"use client";

import { useMemo, useState } from "react";
import { usd } from "@/lib/format";
import { COLUMNS, defaultDir, filterModels, sortModels, type Dir, type ModelSort } from "@/lib/sort";
import { compact, count, modelColors, ms, percent, sumUsd, type UsageGroup } from "@/lib/usage";

/**
 * The by-model breakdown. Click a column heading to sort by it, and again to reverse; type to narrow the list. The totals
 * row adds up what is shown and matches the headline figures when nothing is filtered out.
 */
export function ModelTable({ models }: { models: UsageGroup[] }) {
  const [sort, setSort] = useState<{ key: ModelSort; dir: Dir }>({ key: "cost", dir: "desc" });
  const [query, setQuery] = useState("");
  const colorOf = useMemo(() => modelColors(models), [models]);
  const shown = useMemo(() => sortModels(filterModels(models, query), sort.key, sort.dir), [models, query, sort]);
  const sum = (f: (m: UsageGroup) => number) => shown.reduce((a, m) => a + f(m), 0);
  const reqs = sum((m) => m.requests);
  const filtered = shown.length !== models.length;

  const click = (key: ModelSort) => setSort((s) => (s.key === key ? { key, dir: s.dir === "asc" ? "desc" : "asc" } : { key, dir: defaultDir(key) }));

  return (
    <section className="panel pad" aria-label="Breakdown by model">
      <div className="ph">
        <h2>By model</h2>
        <div className="mtool">
          {models.length > 1 && (
            <>
              <label className="sr-only" htmlFor="model-filter">Filter models</label>
              <input id="model-filter" className="inp mtool__q" type="search" placeholder="Filter models" value={query} onChange={(e) => setQuery(e.target.value)} />
            </>
          )}
          <span className="mute" role="status">
            {filtered ? `${shown.length} of ${models.length} models, ` : ""}sorted by {COLUMNS.find((c) => c.key === sort.key)!.label.toLowerCase()}, {sort.dir === "asc" ? (sort.key === "model" ? "A to Z" : "lowest first") : sort.key === "model" ? "Z to A" : "highest first"}
          </span>
        </div>
      </div>
      {models.length === 0 ? (
        <p className="mute">No requests in this range.</p>
      ) : shown.length === 0 ? (
        <p className="mute">No model matches “{query}”.</p>
      ) : (
        <div className="scroll">
          <table className="t t--sortable">
            <thead>
              <tr>
                {COLUMNS.map((c) => {
                  const active = sort.key === c.key;
                  return (
                    <th key={c.key} scope="col" aria-sort={active ? (sort.dir === "asc" ? "ascending" : "descending") : "none"} title={c.hint}>
                      <button type="button" className={`sortbtn${active ? " sortbtn--on" : ""}`} onClick={() => click(c.key)}>
                        {c.label}
                        <svg className="sortbtn__i" width="10" height="12" viewBox="0 0 10 12" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                          <path d={active ? (sort.dir === "asc" ? "M5 10V2M2 5l3-3 3 3" : "M5 2v8M2 7l3 3 3-3") : "M3 4.5l2-2.5 2 2.5M3 7.5l2 2.5 2-2.5"} opacity={active ? 1 : 0.55} />
                        </svg>
                      </button>
                    </th>
                  );
                })}
              </tr>
            </thead>
            <tbody>
              {shown.map((m) => (
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
                  <td>{ms(m.timed_requests > 0 ? m.p50_ms : null)}</td>
                  <td>{ms(m.timed_requests > 0 ? m.p95_ms : null)}</td>
                  <td>{percent(m.cache_hits, m.requests)}</td>
                  <td>{percent(m.errors, m.requests, 1)}</td>
                </tr>
              ))}
            </tbody>
            <tfoot>
              <tr>
                <th scope="row">{filtered ? "Total of those shown" : "Total"}</th>
                <td>{count(reqs)}</td>
                <td>{compact(sum((m) => m.input_tokens))}</td>
                <td>{compact(sum((m) => m.output_tokens))}</td>
                <td>{usd(sumUsd(shown.map((m) => m.cost_usd)))}</td>
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
