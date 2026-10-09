import Link from "next/link";
import { agoLabel, reasonLabel, runCost, runDuration, statusView, stripClass, stripLabel, type RunItem, type Strip, type WaitingRun, type Bar } from "@/lib/runs";

/** A run's steps as a row of dots: round for a model call, square for a tool, with the state in words for screen readers. */
export function StepStrip({ strip }: { strip: Strip }) {
  return (
    <span className="strip" role="img" aria-label={stripLabel(strip)}>
      {strip.more > 0 && <i className="nd more">+{strip.more}</i>}
      {strip.steps.map((n, i) => (
        <i key={i} className={stripClass(n)} />
      ))}
    </span>
  );
}

/** A status as a neutral pill with a coloured shape and a word. */
export function RunTag({ run, now }: { run: Pick<RunItem, "status" | "wake_at">; now: Date }) {
  const v = statusView(run, now);
  const cls = v.tone === "run" ? "run" : v.tone === "sleep" ? "sleep" : v.tone === "wait" ? "wait" : "";
  return (
    <span className={`tag ${cls}`.trim()} style={{ ["--c" as string]: `var(--${v.tone === "cancel" ? "sleep" : v.tone})` }}>
      <svg width="12" height="12" viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
        <path d={v.icon} />
      </svg>
      {v.word}
    </span>
  );
}

export function RunCard({ run, now, fresh }: { run: RunItem; now: Date; fresh: boolean }) {
  return (
    <Link className={`panel rc ${fresh ? "fresh" : ""}`.trim()} href={`/runs/${run.id}`}>
      <div className="top">
        <RunTag run={run} now={now} />
        <span>{run.key}</span>
      </div>
      <div className="g">{run.goal}</div>
      <StepStrip strip={run.strip} />
      <div className="ft num">
        <span>{run.step_count === 0 ? "Not started" : `Step ${run.step_count}`}</span>
        <span>{runCost(run.cost_usd)}</span>
        <span>{runDuration(run, now)}</span>
      </div>
    </Link>
  );
}

export function RunRow({ run, now, tone }: { run: RunItem; now: Date; tone: "fresh" | "ended" | "" }) {
  const v = statusView(run, now);
  const why = run.status === "succeeded" ? "" : reasonLabel(run.failure_reason) || v.word;
  return (
    <div role="listitem">
      <Link className={`lr ${tone}`.trim()} href={`/runs/${run.id}`}>
      <span className={`sti ${v.tone}`} role="img" aria-label={v.word}>
        <svg width="16" height="16" viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <path d={v.icon} />
        </svg>
      </span>
      <span className="goal">{run.goal}</span>
      <span className="why">{why}</span>
      <StepStrip strip={run.strip} />
      <span className="right-t num">{runCost(run.cost_usd)}</span>
      <span className="right-t num">{runDuration(run, now)}</span>
      <span className="right-t">{agoLabel(run.finished_at ?? run.created_at, now)}</span>
      </Link>
    </div>
  );
}

/** The activity chart: stacked bars per slice of time. The legend names each colour, and every bar has its numbers in a tooltip. */
export function ActivityBars({ bars, summary, labels }: { bars: Bar[]; summary: string; labels: boolean }) {
  return (
    <>
      <div className="alegend mute">
        <span><i className="sw" style={{ background: "var(--ok)" }} />Succeeded</span>
        <span><i className="sw" style={{ background: "var(--fail)" }} />Failed</span>
        <span><i className="sw" style={{ background: "var(--sleep)" }} />Cancelled</span>
        <span><i className="sw" style={{ background: "var(--run)" }} />In progress</span>
      </div>
      <div className="abars" role="img" aria-label={summary}>
        {bars.map((b) => (
          <div key={b.key} className="abar" title={b.tip}>
            <i style={{ height: `${b.run}%`, background: "var(--run)" }} />
            <i style={{ height: `${b.cancel}%`, background: "var(--sleep)" }} />
            <i style={{ height: `${b.fail}%`, background: "var(--fail)" }} />
            <i style={{ height: `${b.ok}%`, background: "var(--ok)" }} />
          </div>
        ))}
      </div>
      {labels && (
        <div className="abx" aria-hidden="true">
          {bars.map((b) => (
            <span key={b.key}>{b.label}</span>
          ))}
        </div>
      )}
    </>
  );
}

export function NeedsYou({ waiting, now }: { waiting: WaitingRun[]; now: Date }) {
  return (
    <section className="sec" aria-label="Runs waiting for you">
      <h2>
        Needs you <span>{waiting.length}</span>
      </h2>
      <div className="need">
        {waiting.map((w) => (
          <div key={w.id} className="nr">
            <span className="ic" aria-hidden="true">
              <svg width="14" height="14" viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" aria-hidden="true">
                <path d="M4 2.5v7M8 2.5v7" />
              </svg>
            </span>
            <div className="tx">
              <span className="tt">{w.goal}</span>
              <span className="sb">
                Waiting {agoLabel(w.waiting_since, now).replace(" ago", "")}, {w.key}
              </span>
            </div>
            <span className="tag mono">{w.tool}</span>
            <Link className="btn sm pri" href={`/runs/${w.id}`}>
              Review
            </Link>
          </div>
        ))}
      </div>
    </section>
  );
}
