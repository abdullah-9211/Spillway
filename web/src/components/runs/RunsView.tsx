"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { applySnapshot, appendFinished, bars, counters, firstState, rangeOf, RANGES, type Hours, type Live, type RunItem, type Snapshot } from "@/lib/runs";
import { Button, StatTile } from "../ui";
import { ActivityBars, NeedsYou, RunCard, RunRow } from "./parts";

const POLL_MS = 2000;
const TONE = { running: "run", needs: "wait", ok: "ok", fail: "fail" } as const;
const ICONS = {
  running: "M6 1.5a4.5 4.5 0 1 0 4.5 4.5",
  needs: "M4 2.5v7M8 2.5v7",
  ok: "M2.5 6.5l2.2 2.2L9.5 3.5",
  fail: "M3 3l6 6M9 3l-6 6",
};

type Link = "live" | "paused" | "offline";

/** A clock that ticks, so "6m" on a running card keeps counting between updates. */
function useNow(every: number): Date {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const id = window.setInterval(() => setNow(new Date()), every);
    return () => window.clearInterval(id);
  }, [every]);
  return now;
}

export function RunsView({ initial }: { initial: Snapshot }) {
  const [live, setLive] = useState<Live>(() => firstState(initial));
  const [hours, setHours] = useState<Hours>(initial.hours);
  const [link, setLink] = useState<Link>("live");
  const [more, setMore] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const want = useRef<Hours>(initial.hours);
  const misses = useRef(0);
  const now = useNow(1000);

  const refresh = useCallback(async () => {
    const h = want.current;
    try {
      const res = await fetch(`/api/runs/snapshot?hours=${h}`, { cache: "no-store" });
      if (!res.ok) throw new Error(String(res.status));
      const data = (await res.json()) as Snapshot;
      if (want.current !== h) return; // the range changed while this was on its way
      misses.current = 0;
      setLink(document.hidden ? "paused" : "live");
      setError(null);
      setLive((prev) => applySnapshot(prev, data));
    } catch {
      if (want.current !== h) return;
      misses.current += 1;
      if (misses.current >= 2) setLink("offline");
    }
  }, []);

  useEffect(() => {
    const tick = () => {
      if (document.hidden) {
        setLink((l) => (l === "offline" ? l : "paused"));
        return;
      }
      void refresh();
    };
    const id = window.setInterval(tick, POLL_MS);
    document.addEventListener("visibilitychange", tick);
    return () => {
      window.clearInterval(id);
      document.removeEventListener("visibilitychange", tick);
    };
  }, [refresh]);

  function choose(h: Hours) {
    if (h === want.current) return;
    want.current = h;
    setHours(h);
    window.history.replaceState(null, "", h === 24 ? "/" : `/?hours=${h}`);
    void refresh();
  }

  async function loadMore() {
    const cursor = live.snap.nextCursor;
    if (!cursor) return;
    setMore(true);
    setError(null);
    try {
      const res = await fetch(`/api/runs/earlier?hours=${want.current}&cursor=${encodeURIComponent(cursor)}`, { cache: "no-store" });
      const data = (await res.json().catch(() => ({}))) as { runs?: RunItem[]; next_cursor?: string | null; message?: string };
      if (!res.ok || !data.runs) {
        setError(data.message ?? "Could not load older runs.");
        return;
      }
      setLive((prev) => appendFinished(prev, data.runs ?? [], data.next_cursor ?? null));
    } catch {
      setError("Could not reach the dashboard. Check your connection and try again.");
    } finally {
      setMore(false);
    }
  }

  const { snap, fresh, ended } = live;
  const running = snap.active.filter((r) => r.status !== "waiting_human");
  const chart = bars(snap.buckets, snap.bucketSeconds, snap.hours);
  const range = rangeOf(snap.hours);
  const totalFinished = snap.counts.succeeded + snap.counts.failed + snap.counts.cancelled;
  const linkWord = link === "live" ? "Live" : link === "paused" ? "Paused while this tab is hidden" : "Offline: trying again";

  return (
    <>
      <div className="head">
        <h1 className="h1">Runs</h1>
        <div className="right">
          <div className="seg" role="group" aria-label="Time range">
            {RANGES.map((r) => (
              <button key={r.hours} type="button" className={hours === r.hours ? "on" : ""} aria-pressed={hours === r.hours} onClick={() => choose(r.hours)}>
                {r.label}
              </button>
            ))}
          </div>
          <span className={`live ${link}`} role="status">
            <i aria-hidden="true" />
            {linkWord}
          </span>
        </div>
      </div>

      <section className="panel pulse pulse--wide" aria-label={`Activity in the last ${range.label}`}>
        <div className="stats">
          {counters(snap.counts).map((c) => (
            <StatTile
              key={c.id}
              label={c.label}
              value={c.value}
              tone={TONE[c.id]}
              hint={c.hint}
              icon={
                <svg width="14" height="14" viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                  <path d={ICONS[c.id]} />
                </svg>
              }
            />
          ))}
        </div>
        <div className="pulse__chart">
          <ActivityBars bars={chart.bars} summary={chart.summary} labels={chart.bars.length > 1} />
        </div>
      </section>

      {snap.waiting.length > 0 && <NeedsYou waiting={snap.waiting} now={now} />}

      <section className="sec" aria-label="Runs in progress">
        <h2>
          Running now <span>{running.length}</span>
        </h2>
        {running.length === 0 ? (
          <p className="empty panel">Nothing is running. Start a run with the API, or with <span className="mono">spillway runs create</span>.</p>
        ) : (
          <div className="grid3">
            {running.map((r) => (
              <RunCard key={r.id} run={r} now={now} fresh={fresh.has(r.id)} />
            ))}
          </div>
        )}
      </section>

      <section className="sec" aria-label="Earlier runs">
        <h2>
          {range.finishedHeading} <span>{totalFinished}</span>
        </h2>
        {snap.finished.length === 0 ? (
          <p className="empty panel">No runs finished in the last {range.label}.</p>
        ) : (
          <div className="panel scroll">
            <div className="list" role="list">
              {snap.finished.map((r) => (
                <RunRow key={r.id} run={r} now={now} tone={ended.has(r.id) ? "ended" : fresh.has(r.id) ? "fresh" : ""} />
              ))}
            </div>
          </div>
        )}
        {error && (
          <div className="err" role="alert">
            <span>{error}</span>
          </div>
        )}
        {snap.nextCursor && (
          <div className="loadmore">
            <Button type="button" onClick={() => void loadMore()} disabled={more}>
              {more ? "Loading…" : "Load older runs"}
            </Button>
          </div>
        )}
      </section>
    </>
  );
}
