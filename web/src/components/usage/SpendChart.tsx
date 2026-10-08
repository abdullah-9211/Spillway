"use client";

import { useState } from "react";
import { usd } from "@/lib/format";
import { buildBars, chartSeries, count, sumUsd, type UsageGroup } from "@/lib/usage";

/**
 * Spend per day, one stacked bar per day coloured by model. The series colours are the design's documented accents,
 * which do not reach contrast guidelines on their own, so the chart always has a legend, a hover figure for each bar
 * and a table view that says the same thing in words.
 */
export function SpendChart({ days, byModel }: { days: UsageGroup[]; byModel: UsageGroup[] }) {
  const [table, setTable] = useState(false);
  const { series, free } = chartSeries(days, byModel);
  const { bars, ticks } = buildBars(days, series);
  const total = sumUsd(days.map((d) => d.cost_usd));
  const empty = Number(total) === 0;
  const dense = bars.length > 20;

  return (
    <section className="panel pc" aria-label="Spend per day by model">
      <div className="ph">
        <h2>Spend per day</h2>
        <div className="legend">
          {series.map((s) => (
            <span key={s.model}>
              <i className="sw" style={{ background: s.color }} />
              {s.label}
            </span>
          ))}
          {free.map((f) => (
            <span key={f}>
              <i className="sw sw--free" />
              {f} (free, no bar)
            </span>
          ))}
          <button type="button" className="link-btn" onClick={() => setTable((t) => !t)} aria-pressed={table}>
            {table ? "View as chart" : "View as table"}
          </button>
        </div>
      </div>

      {table ? (
        <div className="scroll">
          <table className="t t--days">
            <caption className="sr-only">Spend per day by model</caption>
            <thead>
              <tr>
                <th scope="col">Day</th>
                {series.map((s) => (
                  <th scope="col" key={s.model}>{s.label}</th>
                ))}
                <th scope="col">Total</th>
                <th scope="col">Requests</th>
              </tr>
            </thead>
            <tbody>
              {days.map((d, i) => (
                <tr key={d.key}>
                  <th scope="row">{d.key}</th>
                  {series.map((s) => (
                    <td key={s.model}>{usd(bars[i].segments.find((x) => x.model === s.model)?.cost ?? "0")}</td>
                  ))}
                  <td>{usd(d.cost_usd)}</td>
                  <td>{count(d.requests)}</td>
                </tr>
              ))}
            </tbody>
            <tfoot>
              <tr>
                <th scope="row">Total</th>
                {series.map((s) => (
                  <td key={s.model}>{usd(sumUsd(bars.map((b) => b.segments.find((x) => x.model === s.model)?.cost ?? "0")))}</td>
                ))}
                <td>{usd(total)}</td>
                <td>{count(days.reduce((a, d) => a + d.requests, 0))}</td>
              </tr>
            </tfoot>
          </table>
        </div>
      ) : (
        <div className="chart">
          <div className="yax" aria-hidden="true">
            {ticks.map((t, i) => (
              <span key={i}>{t}</span>
            ))}
          </div>
          <div className="plot">
            {[0, 25, 50, 75].map((p) => (
              <div key={p} className="gl" style={{ top: `${p}%` }} />
            ))}
            <div className="bars" style={{ gap: dense ? 3 : 8 }}>
              {bars.map((b) => (
                <div key={b.day} className="col" title={b.tip} tabIndex={0} role="img" aria-label={b.tip}>
                  {[...b.segments].reverse().map((s) => (
                    <i key={s.model} style={{ height: `${s.height}%`, background: s.color }} />
                  ))}
                </div>
              ))}
            </div>
            <div className="xl" style={{ gap: dense ? 3 : 8 }} aria-hidden="true">
              {bars.map((b, i) => (
                <span key={b.day} className={dense && i % 5 !== 0 ? "xl--skip" : undefined}>{b.label}</span>
              ))}
            </div>
            {empty && <p className="chart__empty">No spend in this range.</p>}
          </div>
        </div>
      )}
      <p className="mute chart__note">
        {table ? "The same figures as the chart, with totals for the whole period." : "Hover or focus a bar for that day's figures. The table view has the totals for the whole period."}
      </p>
    </section>
  );
}
