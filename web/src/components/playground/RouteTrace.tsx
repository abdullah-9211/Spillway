import { hopsFor, segmentsFor, timingLabel, type PlaygroundResult } from "@/lib/playground";

/**
 * How the request got to its answer: one stop per attempt, a shape and a word on each (never colour alone), and a
 * bar showing where the time went.
 */
export function RouteTrace({ result }: { result: Pick<PlaygroundResult, "policy" | "attempts" | "latency_ms" | "overhead_ms"> }) {
  const hops = hopsFor(result);
  const segs = segmentsFor(result.attempts);
  const t = timingLabel(result);
  return (
    <div>
      <h2 className="pg__h2">How it got there</h2>
      <ol className="trace" aria-label="Route">
        {hops.map((h, i) => (
          <li key={h.key} className={`h ${h.tone} ${i === 0 ? "first" : ""}`.trim()} aria-label={h.label}>
            <span className="hc">
              <svg width="16" height="16" viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                <path d={h.icon} />
              </svg>
            </span>
            <span className="hn" aria-hidden="true">{h.name}</span>
            <span className="hm num" aria-hidden="true">{h.meta}</span>
          </li>
        ))}
      </ol>
      {segs.length > 0 && (
        <div className="tb" role="img" aria-label={t.aria}>
          {segs.map((s) => (
            <i key={s.key} style={{ width: `${s.share}%`, background: s.tone === "fail" ? "var(--fail)" : "var(--ok)" }} />
          ))}
        </div>
      )}
      <div className="tt num">
        <span>{t.total}</span>
        <span>{t.overhead}</span>
      </div>
    </div>
  );
}
