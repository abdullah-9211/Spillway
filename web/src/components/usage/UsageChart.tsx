"use client";

import Link from "next/link";
import { useMemo, useRef, useState, type KeyboardEvent } from "react";
import { buildBars, defaultGran, detailRows, formatValue, IDLE_NAMES, METRICS, planSeries, toBuckets, uniqueNames, valueOf, zoomRange, type BarModel, type Bucket, type Gran, type Metric, type Stack } from "@/lib/chart";
import { compact, count, percent, type UsageGroup } from "@/lib/usage";

function Toggle<T extends string>({ label, value, options, onChange, disabled, hint }: {
  label: string;
  value: T;
  options: { value: T; label: string; disabled?: boolean }[];
  onChange: (v: T) => void;
  disabled?: boolean;
  hint?: string;
}) {
  return (
    <div className="seg seg--sm" role="group" aria-label={label} title={disabled ? hint : undefined}>
      {options.map((o) => (
        <button key={o.value} type="button" className={value === o.value ? "on" : undefined} aria-pressed={value === o.value} disabled={disabled || o.disabled} onClick={() => onChange(o.value)}>
          {o.label}
        </button>
      ))}
    </div>
  );
}

function Detail({ bucket, metric, keyId, onClose }: { bucket: Bucket; metric: Metric; keyId: string; onClose: () => void }) {
  const { rows, more, idle } = detailRows(bucket, metric);
  const z = zoomRange(bucket);
  const q = new URLSearchParams({ range: "custom", from: z.from, to: z.to });
  if (keyId) q.set("key", keyId);
  return (
    <div className="cdetail" role="region" aria-label={`Details for ${bucket.title}`}>
      <div className="cdetail__head">
        <h3>{bucket.title}</h3>
        <span className="cdetail__links">
          {bucket.start !== bucket.end || bucket.days > 1 ? (
            <Link href={`/usage?${q}`} scroll={false}>Zoom to {bucket.days > 1 ? "this week" : "this day"}</Link>
          ) : (
            <Link href={`/usage?${q}`} scroll={false}>Open only this day</Link>
          )}
          <button type="button" className="link-btn" onClick={onClose}>Close</button>
        </span>
      </div>
      <dl className="cdetail__stats">
        <div><dt>Spend</dt><dd>{formatValue(valueOf(bucket, "spend"), "spend")}</dd></div>
        <div><dt>Requests</dt><dd>{count(bucket.requests)}</dd></div>
        <div><dt>Tokens</dt><dd>{compact(bucket.input + bucket.output)}</dd></div>
        <div><dt>Saved by cache</dt><dd>{formatValue(valueOf(bucket, "saved"), "saved")}</dd></div>
        <div><dt>Cache hits</dt><dd>{percent(bucket.cacheHits, bucket.requests)}</dd></div>
        <div><dt>Errors</dt><dd>{percent(bucket.errors, bucket.requests, 1)}</dd></div>
      </dl>
      {(rows.length > 0 || idle > 0) && (
        <ul className="cdetail__list">
          {rows.map((r) => (
            <li key={r.label}>
              <span>
                {r.label}
                {r.n > 1 && <span className="mute"> ×{r.n}</span>}
              </span>
              <span className="num">{formatValue(r.value, metric)}</span>
            </li>
          ))}
          {more && (
            <li className="cdetail__more">
              <span>{count(more.count)} more</span>
              <span className="num">{formatValue(more.value, metric)}</span>
            </li>
          )}
          {idle > 0 && (
            <li className="cdetail__more">
              <span>{count(idle)} with none</span>
              <span className="num">{formatValue(0, metric)}</span>
            </li>
          )}
        </ul>
      )}
    </div>
  );
}

/**
 * Usage over time. Switch what it plots (spend, requests, tokens, savings), what each bar is split by (model or
 * API key) and the bar size (day or week). Click a legend entry to hide a series, hover or focus a bar for its
 * figures, and click a bar to pin its details. The colours are the design's documented accents, which are not
 * distinguishable on their own, so the chart always has a legend, spoken figures and a table view.
 */
export function UsageChart({ dayModel, dayKey, keyId }: { dayModel: UsageGroup[]; dayKey: UsageGroup[]; keyId: string }) {
  const [metric, setMetric] = useState<Metric>("spend");
  const [stack, setStack] = useState<Stack>("model");
  const [gran, setGran] = useState<Gran>(defaultGran(dayModel.length));
  const [hidden, setHidden] = useState<Set<string>>(new Set());
  const [hover, setHover] = useState<number | null>(null);
  const [pinned, setPinned] = useState<number | null>(null);
  const [focusIdx, setFocusIdx] = useState(0);
  const [table, setTable] = useState(false);
  const refs = useRef<(HTMLButtonElement | null)[]>([]);

  const days = stack === "key" ? dayKey : dayModel;
  const buckets = useMemo(() => toBuckets(days, gran), [days, gran]);
  const { series, idle } = useMemo(() => planSeries(buckets, metric, stack === "key" ? "keys" : "models"), [buckets, metric, stack]);
  const { bars, ticks } = useMemo(() => buildBars(buckets, metric, series, hidden), [buckets, metric, series, hidden]);
  const empty = bars.every((b) => b.total === 0);
  const dense = bars.length > 20;
  const every = Math.max(1, Math.ceil(bars.length / 12));
  const pinnedBucket = pinned != null ? buckets[pinned] : undefined;
  const shown = hover ?? (pinned != null ? pinned : null);

  // Series ids differ per metric and split, so a hidden series or a pinned bar from before would point at nothing.
  const reset = () => {
    setHidden(new Set());
    setPinned(null);
    setHover(null);
    setFocusIdx(0);
  };
  const toggle = (id: string) =>
    setHidden((h) => {
      const n = new Set(h);
      if (n.has(id)) n.delete(id);
      else n.add(id);
      return n;
    });

  function onKey(e: KeyboardEvent, i: number) {
    const move = (to: number) => {
      const t = Math.min(Math.max(to, 0), bars.length - 1);
      setFocusIdx(t);
      refs.current[t]?.focus();
      e.preventDefault();
    };
    if (e.key === "ArrowRight") move(i + 1);
    else if (e.key === "ArrowLeft") move(i - 1);
    else if (e.key === "Home") move(0);
    else if (e.key === "End") move(bars.length - 1);
    else if (e.key === "Escape") setPinned(null);
  }

  const grand = bars.reduce((a, b) => a + b.total, 0);
  const noun = METRICS.find((m) => m.value === metric)!.label.toLowerCase();

  return (
    <section className="panel pc" aria-label="Usage over time">
      <div className="ph">
        <h2>Usage over time</h2>
        <div className="cctl">
          <Toggle label="What to plot" value={metric} options={METRICS} onChange={(v) => { setMetric(v); reset(); }} />
          <Toggle label="Split bars by" value={stack} options={[{ value: "model", label: "By model" }, { value: "key", label: "By key" }]}
            onChange={(v) => { setStack(v); reset(); }} disabled={!!keyId} hint="Already filtered to one key" />
          <Toggle label="Bar size" value={gran} options={[{ value: "day", label: "Daily" }, { value: "week", label: "Weekly", disabled: dayModel.length < 14 }]}
            onChange={(v) => { setGran(v); reset(); }} />
        </div>
      </div>

      <div className="legend">
        {series.map((s) => (
          <button key={s.id} type="button" className={`lg${hidden.has(s.id) ? " lg--off" : ""}`} aria-pressed={!hidden.has(s.id)} onClick={() => toggle(s.id)}
            title={hidden.has(s.id) ? `Show ${s.label}` : `Hide ${s.label}`}>
            <i className="sw" style={{ background: hidden.has(s.id) ? "transparent" : s.color, borderColor: s.color }} />
            {s.label}
          </button>
        ))}
        {uniqueNames(idle).slice(0, IDLE_NAMES).map((f) => (
          <span key={f} className="lg lg--idle">
            <i className="sw sw--free" />
            {f} ({metric === "spend" ? (stack === "key" ? "no spend" : "free, no bar") : "none"})
          </span>
        ))}
        {uniqueNames(idle).length > IDLE_NAMES && (
          <span className="lg lg--idle" title={uniqueNames(idle).join(", ")}>
            and {uniqueNames(idle).length - IDLE_NAMES} more with no {metric === "spend" ? "spend" : "activity"}
          </span>
        )}
        {hidden.size > 0 && (
          <button type="button" className="link-btn" onClick={() => setHidden(new Set())}>Show all</button>
        )}
        <button type="button" className="link-btn lg__table" onClick={() => setTable((t) => !t)} aria-pressed={table}>
          {table ? "View as chart" : "View as table"}
        </button>
      </div>

      {table ? (
        <div className="scroll">
          <table className="t t--days">
            <caption className="sr-only">{noun} by {gran === "day" ? "day" : "week"}</caption>
            <thead>
              <tr>
                <th scope="col">{gran === "day" ? "Day" : "Week"}</th>
                {series.filter((s) => !hidden.has(s.id)).map((s) => <th scope="col" key={s.id}>{s.label}</th>)}
                <th scope="col">Total</th>
                <th scope="col">Requests</th>
              </tr>
            </thead>
            <tbody>
              {bars.map((b) => (
                <tr key={b.bucket.start}>
                  <th scope="row">{b.bucket.title}</th>
                  {series.filter((s) => !hidden.has(s.id)).map((s) => (
                    <td key={s.id}>{formatValue(b.segs.find((x) => x.id === s.id)?.value ?? 0, metric)}</td>
                  ))}
                  <td>{formatValue(b.total, metric)}</td>
                  <td>{count(b.bucket.requests)}</td>
                </tr>
              ))}
            </tbody>
            <tfoot>
              <tr>
                <th scope="row">Total</th>
                {series.filter((s) => !hidden.has(s.id)).map((s) => (
                  <td key={s.id}>{formatValue(bars.reduce((a, b) => a + (b.segs.find((x) => x.id === s.id)?.value ?? 0), 0), metric)}</td>
                ))}
                <td>{formatValue(grand, metric)}</td>
                <td>{count(buckets.reduce((a, b) => a + b.requests, 0))}</td>
              </tr>
            </tfoot>
          </table>
        </div>
      ) : (
        <div className="chart">
          <div className="yax" aria-hidden="true">
            {ticks.map((t, i) => <span key={i}>{t}</span>)}
          </div>
          <div className="plot" onMouseLeave={() => setHover(null)}>
            {[0, 25, 50, 75].map((p) => <div key={p} className="gl" style={{ top: `${p}%` }} />)}
            <div className="bars" style={{ gap: dense ? 3 : 8 }}>
              {bars.map((b, i) => (
                <button key={b.bucket.start} ref={(el) => { refs.current[i] = el; }} type="button" className={`col${pinned === i ? " col--pinned" : ""}`}
                  aria-label={b.tip} aria-pressed={pinned === i} tabIndex={i === Math.min(focusIdx, bars.length - 1) ? 0 : -1}
                  onMouseEnter={() => setHover(i)} onFocus={() => { setHover(i); setFocusIdx(i); }} onBlur={() => setHover((h) => (h === i ? null : h))}
                  onClick={() => setPinned((p) => (p === i ? null : i))} onKeyDown={(e) => onKey(e, i)}>
                  {[...b.segs].reverse().map((s) => <i key={s.id} style={{ height: `${s.height}%`, background: s.color }} />)}
                </button>
              ))}
            </div>
            <div className="xl" style={{ gap: dense ? 3 : 8 }} aria-hidden="true">
              {bars.map((b, i) => <span key={b.bucket.start} className={i % every !== 0 ? "xl--skip" : undefined}>{b.bucket.label}</span>)}
            </div>
            {shown != null && bars[shown] && <Tooltip bar={bars[shown]} count={bars.length} metric={metric} />}
            {empty && <p className="chart__empty">{hidden.size > 0 ? "Every series is hidden." : `No ${noun} in this range.`}</p>}
          </div>
        </div>
      )}

      {pinnedBucket && !table && <Detail bucket={pinnedBucket} metric={metric} keyId={keyId} onClose={() => setPinned(null)} />}
      <p className="mute chart__note">
        {table ? "The same figures as the chart, with totals for the whole period." : "Hover or focus a bar for its figures and click it to pin the details. Arrow keys move between bars. Click a name in the legend to hide it."}
      </p>
    </section>
  );
}

function Tooltip({ bar, count: n, metric }: { bar: BarModel; count: number; metric: Metric }) {
  const edge = bar.index < n * 0.18 ? "left" : bar.index > n * 0.82 ? "right" : "mid";
  return (
    <div className={`tip tip--${edge}`} style={{ left: `${((bar.index + 0.5) / n) * 100}%` }} role="tooltip">
      <strong>{bar.bucket.title}</strong>
      <span className="tip__total">{formatValue(bar.total, metric)}</span>
      {bar.segs.length > 1 && (
        <ul>
          {bar.segs.map((s) => (
            <li key={s.id}>
              <i className="sw" style={{ background: s.color }} />
              <span>{s.label}</span>
              <span className="num">{formatValue(s.value, metric)}</span>
            </li>
          ))}
        </ul>
      )}
      <span className="tip__sub">{count(bar.bucket.requests)} requests</span>
    </div>
  );
}

