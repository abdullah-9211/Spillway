"use client";

import { useState } from "react";
import { waterfall } from "@/lib/waterfall";
import type { RunGraph } from "@/lib/graph";

const WORD = { finished: "finished", failed: "failed", running: "running", stopped: "stopped" } as const;

/** The run along time: one bar per attempt, so a slow step, a re-issue and the idle gap in a recovery are all visible. */
export function Waterfall({ graph, now, selected, onSelect }: { graph: RunGraph; now: Date; selected: string | null; onSelect: (id: string) => void }) {
  const w = waterfall(graph, now);
  const [hover, setHover] = useState<string | null>(null);
  if (w.bars.length === 0) return null;
  const ROW = 30;
  return (
    <section className="panel wf" aria-label="Where the time went">
      <div className="gh">
        <h2>Where the time went</h2>
        <span className="small num">{(w.totalMs / 1000).toFixed(1)}s from the first step to the last</span>
      </div>
      <div className="wf__body">
        <div className="wf__axis" aria-hidden="true">
          {w.ticks.map((t) => (
            <span key={t.at} style={{ left: `${t.at}%` }}>{t.label}</span>
          ))}
        </div>
        <ol className="wf__rows" style={{ height: w.bars.length * ROW }}>
          {w.ticks.map((t) => (
            <i key={t.at} className="wf__tick" style={{ left: `${t.at}%` }} aria-hidden="true" />
          ))}
          {w.gaps.map((g, i) => (
            <i key={i} className="wf__gap" style={{ left: `${g.left}%`, width: `${g.width}%` }} title={g.label} />
          ))}
          {w.bars.map((b, i) => (
            <li key={b.id} className="wf__row" style={{ top: i * ROW, height: ROW }}>
              <span className="wf__name mono" aria-hidden="true">{b.step} {b.label}</span>
              <button
                type="button"
                className={`wf__bar ${b.state} ${b.reissued ? "redo" : ""} ${selected === b.id ? "sel" : ""}`.trim()}
                style={{ left: `${b.left}%`, width: `${b.width}%`, animationDelay: `${Math.min(i, 24) * 35}ms` }}
                aria-pressed={selected === b.id}
                aria-label={`Step ${b.step}, ${b.label}, ${WORD[b.state]}${b.reissued ? ", re-issued" : ""}, on ${b.worker}, ${((b.endMs - b.startMs) / 1000).toFixed(1)} seconds from ${(b.startMs / 1000).toFixed(1)}`}
                onClick={() => onSelect(b.id)}
                onPointerEnter={() => setHover(b.id)}
                onPointerLeave={() => setHover(null)}
              />
              {hover === b.id && (
                <span className="wf__tip" style={{ left: `${Math.min(b.left + b.width / 2, 88)}%` }}>
                  {b.worker}, epoch {b.epoch}: {(b.startMs / 1000).toFixed(1)}s to {(b.endMs / 1000).toFixed(1)}s{b.open ? " (never ended)" : ""}
                </span>
              )}
            </li>
          ))}
        </ol>
      </div>
      <div className="leg">
        <span><i className="lk wfk ok" />Finished</span>
        <span><i className="lk wfk run" />Running</span>
        <span><i className="lk wfk stop" />Stopped before it finished</span>
        <span><i className="lk wfk redo" />Re-issued</span>
        <span><i className="lk wfk gap" />No worker was holding the run</span>
      </div>
    </section>
  );
}
