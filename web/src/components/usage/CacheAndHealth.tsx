import { breakerPill, count, shares, type UsageSummary } from "@/lib/usage";

function Pill({ state, remaining, reason }: { state: string; remaining: number | null; reason?: string }) {
  const p = breakerPill({ state: state as "closed", open_remaining_seconds: remaining, reason });
  return (
    <span className={`st ${p.tone}`} title={reason}>
      <svg width="12" height="12" viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
        <path d={p.icon} />
      </svg>
      {p.label}
    </span>
  );
}

/** How requests were answered (cache outcomes) and how each provider is doing (breaker state and fallbacks). */
export function CacheAndHealth({ summary }: { summary: UsageSummary }) {
  const c = summary.totals.cache;
  // Not cacheable requests and misses both went to a provider, so they share the first segment.
  const counts = [c.miss + c.bypass, c.hit_exact, c.hit_semantic];
  const [miss, exact, semantic] = shares(counts);
  const total = counts.reduce((a, b) => a + b, 0);
  const rows = [
    { label: "Miss or not cached", pct: miss, n: counts[0], sw: <i className="sw" style={{ background: "var(--subtle)" }} /> },
    { label: "Exact hit", pct: exact, n: counts[1], sw: <i className="sw" style={{ background: "var(--ok)" }} /> },
    { label: "Semantic hit, striped", pct: semantic, n: counts[2], sw: <i className="sw hit2" /> },
  ];
  return (
    <section className="panel pc2" aria-label="Cache and provider health">
      <div className="ph">
        <h2>Cache outcomes</h2>
        <span className="mute">{summary.range.days} {summary.range.days === 1 ? "day" : "days"}</span>
      </div>
      {total === 0 ? (
        <p className="mute">No requests in this range.</p>
      ) : (
        <>
          <div className="stack" role="img" aria-label={`${miss} percent misses or not cached, ${exact} percent exact hits, ${semantic} percent semantic hits`}>
            <i style={{ width: `${miss}%`, background: "var(--subtle)" }} />
            <i style={{ width: `${exact}%`, background: "var(--ok)" }} />
            <i className="hit2" style={{ width: `${semantic}%` }} />
          </div>
          {rows.map((r) => (
            <div className="row2" key={r.label}>
              <span className="row2__l">
                {r.sw}
                {r.label}
              </span>
              <span className="num" title={`${count(r.n)} requests`}>{r.pct}%</span>
            </div>
          ))}
        </>
      )}

      <div className="ph ph--gap">
        <h2>Provider health</h2>
        <span className="mute">Fallbacks fired: {count(summary.fallbacks_fired)}</span>
      </div>
      {summary.providers.length === 0 ? (
        <p className="mute">No providers are configured.</p>
      ) : (
        summary.providers.map((p) => (
          <div className="brk" key={p.name}>
            <span>
              {p.name}
              {p.failed_attempts > 0 && <span className="mute brk__sub"> {count(p.failed_attempts)} failed {p.failed_attempts === 1 ? "call" : "calls"}</span>}
            </span>
            <Pill state={p.state} remaining={p.open_remaining_seconds} reason={p.reason} />
          </div>
        ))
      )}
    </section>
  );
}
