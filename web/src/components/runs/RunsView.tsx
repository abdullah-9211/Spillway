"use client";

import { useRouter } from "next/navigation";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  applySnapshot, appendFinished, bars, counters, firstState, groupByDay, isFiltered, noFilters, rangeOf, RANGES, showsActive, showsFinished, snapshotQuery,
  viewQuery, visibleActive, visibleFinished, type Filters, type Hours, type Live, type RunItem, type Snapshot, type StatusFilter,
} from "@/lib/runs";
import { useRole } from "@/lib/role";
import { Button } from "../ui";
import { NewRunPanel } from "./NewRunPanel";
import { ActivityBars, NeedsYou, RunCard, RunRow } from "./parts";
import { Peek } from "./Peek";
import { Toolbar } from "./Toolbar";

const POLL_MS = 2000;
const SEARCH_DELAY_MS = 300;
const ICONS = {
  running: "M6 1.5a4.5 4.5 0 1 0 4.5 4.5",
  needs: "M4 2.5v7M8 2.5v7",
  ok: "M2.5 6.5l2.2 2.2L9.5 3.5",
  fail: "M3 3l6 6M9 3l-6 6",
};
const COUNTER_FILTER = { running: "active", needs: "needs", ok: "succeeded", fail: "failed" } as const;
const TONE = { running: "run", needs: "wait", ok: "ok", fail: "fail" } as const;

type LinkState = "live" | "paused" | "offline";
type Policy = { name: string; description: string };

/** A clock that ticks, so "6m" on a running card keeps counting between updates. */
function useNow(every: number): Date {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const id = window.setInterval(() => setNow(new Date()), every);
    return () => window.clearInterval(id);
  }, [every]);
  return now;
}

export function RunsView({ initial, initialFilters = noFilters, policies, keys }: { initial: Snapshot; initialFilters?: Filters; policies: Policy[]; keys: { id: string; name: string }[] }) {
  const isAdmin = useRole() === "admin";
  const router = useRouter();
  const [live, setLive] = useState<Live>(() => firstState(initial));
  const [hours, setHours] = useState<Hours>(initial.hours);
  const [filters, setFilters] = useState<Filters>(initialFilters);
  const [q, setQ] = useState(initialFilters.q);
  const [link, setLink] = useState<LinkState>("live");
  const [more, setMore] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [panel, setPanel] = useState(false);
  const [notice, setNotice] = useState<{ id: string } | null>(null);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [copy, setCopy] = useState<RunItem | null>(null); // the selected run as last clicked, for when it leaves the lists
  const want = useRef({ hours: initial.hours, filters: initialFilters });
  const shown = useRef(snapshotQuery(initial.hours, initialFilters));
  const misses = useRef(0);
  const search = useRef<HTMLInputElement>(null);
  const now = useNow(1000);

  const refresh = useCallback(async () => {
    const { hours: h, filters: f } = want.current;
    const query = snapshotQuery(h, f);
    try {
      const res = await fetch(`/api/runs/snapshot?${query}`, { cache: "no-store" });
      if (!res.ok) throw new Error(String(res.status));
      const data = (await res.json()) as Snapshot;
      if (snapshotQuery(want.current.hours, want.current.filters) !== query) return; // the view changed while this was on its way
      misses.current = 0;
      setLink(document.hidden ? "paused" : "live");
      setError(null);
      const fresh = shown.current !== query; // a different view: nothing in it is "new"
      shown.current = query;
      setLive((prev) => (fresh ? firstState(data) : applySnapshot(prev, data)));
    } catch {
      if (snapshotQuery(want.current.hours, want.current.filters) !== query) return;
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

  /** Changes the view: the range or any filter. The address follows, so a view can be shared. */
  const change = useCallback(
    (next: { hours?: Hours; filters?: Partial<Filters> }) => {
      const h = next.hours ?? want.current.hours;
      const f = { ...want.current.filters, ...next.filters };
      want.current = { hours: h, filters: f };
      setHours(h);
      setFilters(f);
      window.history.replaceState(null, "", `/${viewQuery(h, f)}`);
      void refresh();
    },
    [refresh],
  );

  // The search box changes the view a moment after typing stops.
  useEffect(() => {
    if (q.trim() === want.current.filters.q) return;
    const id = window.setTimeout(() => change({ filters: { q: q.trim() } }), SEARCH_DELAY_MS);
    return () => window.clearTimeout(id);
  }, [q, change]);

  const { snap, fresh, ended } = live;
  const active = useMemo(() => visibleActive(snap, filters), [snap, filters]);
  const finished = useMemo(() => visibleFinished(snap, filters), [snap, filters]);
  const order = useMemo(() => [...active.waiting, ...active.running, ...finished], [active, finished]);

  // The selected run stays on screen even after the list changes under it: the live copy if it is still listed.
  const selected = useMemo(() => (selectedId ? ([...snap.active, ...snap.finished].find((r) => r.id === selectedId) ?? (copy?.id === selectedId ? copy : null)) : null), [selectedId, snap, copy]);

  const select = (r: RunItem) => {
    setSelectedId((cur) => (cur === r.id ? null : r.id));
    setCopy(r);
  };
  const open = (id: string) => router.push(`/runs/${id}`);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement | null;
      const typing = !!t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "SELECT" || t.isContentEditable);
      if (e.key === "Escape") {
        if (panel) setPanel(false);
        else setSelectedId(null);
        return;
      }
      if (typing || e.metaKey || e.ctrlKey || e.altKey) return;
      if (e.key === "/") {
        e.preventDefault();
        search.current?.focus();
      } else if (e.key === "j" || e.key === "k") {
        if (order.length === 0) return;
        const i = order.findIndex((r) => r.id === selectedId);
        const target = order[i === -1 ? 0 : e.key === "j" ? Math.min(i + 1, order.length - 1) : Math.max(i - 1, 0)];
        setSelectedId(target.id);
        setCopy(target);
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [order, selectedId, panel]);

  async function loadMore() {
    const cursor = live.snap.nextCursor;
    if (!cursor) return;
    setMore(true);
    setError(null);
    const { hours: h, filters: f } = want.current;
    try {
      const res = await fetch(`/api/runs/earlier?${snapshotQuery(h, f)}&cursor=${encodeURIComponent(cursor)}`, { cache: "no-store" });
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

  function started(id: string) {
    setPanel(false);
    setNotice({ id });
    setSelectedId(id);
    void refresh();
    window.setTimeout(() => void refresh(), 700);
  }

  const range = rangeOf(snap.hours);
  const chart = bars(snap.buckets, snap.bucketSeconds, snap.hours);
  const filtered = isFiltered(filters);
  const totalFinished = snap.counts.succeeded + snap.counts.failed + snap.counts.cancelled;
  const linkWord = link === "live" ? "Live" : link === "paused" ? "Paused while this tab is hidden" : "Offline: trying again";
  const days = filters.sort === "newest" ? groupByDay(finished, now) : [{ label: "", runs: finished }];
  const nothingShown = active.running.length + active.waiting.length + finished.length === 0;
  const rowProps = (r: RunItem) => ({ selected: selectedId === r.id, onSelect: () => select(r), onOpen: () => open(r.id) });
  const clear = () => {
    setQ("");
    change({ filters: { q: "", status: "all", key: "" } });
  };

  return (
    <>
      <div className="head">
        <h1 className="h1">Runs</h1>
        <div className="right">
          <div className="seg" role="group" aria-label="Time range">
            {RANGES.map((r) => (
              <button key={r.hours} type="button" className={hours === r.hours ? "on" : ""} aria-pressed={hours === r.hours} onClick={() => change({ hours: r.hours })}>
                {r.label}
              </button>
            ))}
          </div>
          <span className={`live ${link}`} role="status">
            <i aria-hidden="true" />
            {linkWord}
          </span>
          <Button type="button" variant="primary" disabled={!isAdmin} aria-expanded={panel} title={isAdmin ? undefined : "Only admins can start runs"} onClick={() => setPanel((p) => !p)}>
            New run
          </Button>
        </div>
      </div>

      {notice && (
        <div className="notice" role="status">
          <span>Run started.</span>
          <button type="button" className="link-btn" onClick={() => open(notice.id)}>Open it</button>
          <button type="button" className="link-btn" onClick={() => setNotice(null)} aria-label="Dismiss">Dismiss</button>
        </div>
      )}
      {panel && isAdmin && <NewRunPanel policies={policies} onStarted={started} onClose={() => setPanel(false)} />}

      <section className="panel pulse pulse--wide" aria-label={`Activity in the last ${range.label}`}>
        <div className="stats">
          {counters(snap.counts).map((c) => {
            const target: StatusFilter = COUNTER_FILTER[c.id];
            const on = filters.status === target;
            return (
              <button key={c.id} type="button" className={`stat stat--icon stat--btn ${on ? "on" : ""}`.trim()} style={{ ["--c" as string]: `var(--${TONE[c.id]})` }} aria-pressed={on} onClick={() => change({ filters: { status: on ? "all" : target } })}>
                <span className="stat__label">
                  <svg width="14" height="14" viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                    <path d={ICONS[c.id]} />
                  </svg>
                  {c.label}
                </span>
                <span className="stat__value">{c.value}</span>
                {c.hint && <span className="stat__hint">{c.hint}</span>}
              </button>
            );
          })}
        </div>
        <div className="pulse__chart">
          <ActivityBars bars={chart.bars} summary={chart.summary} labels={chart.bars.length > 1} />
        </div>
      </section>

      <Toolbar
        ref={search} filters={filters} q={q} onQ={setQ} counts={snap.counts} keys={keys} filtered={filtered}
        onStatus={(s) => change({ filters: { status: s } })} onKey={(k) => change({ filters: { key: k } })} onSort={(s) => change({ filters: { sort: s } })} onClear={clear}
      />

      <div className="rlayout">
        <div className="rmain">
          {nothingShown && (
            <p className="empty panel">
              {filtered ? "No runs match. " : "No runs yet. "}
              {filtered ? (
                <button type="button" className="link-btn" onClick={clear}>Clear filters</button>
              ) : isAdmin ? (
                <>Press <strong>New run</strong> to start one, or use the API or <span className="mono">spillway runs create</span>.</>
              ) : (
                <>Start one with the API, or with <span className="mono">spillway runs create</span>.</>
              )}
            </p>
          )}

          {active.waiting.length > 0 && <NeedsYou waiting={snap.waiting.filter((w) => active.waiting.some((r) => r.id === w.id))} now={now} />}

          {showsActive(filters.status) && filters.status !== "needs" && active.running.length > 0 && (
            <section className="sec" aria-label="Runs in progress">
              <h2>
                Running now <span>{active.running.length}</span>
              </h2>
              <div className="grid3">
                {active.running.map((r) => (
                  <RunCard key={r.id} run={r} now={now} fresh={fresh.has(r.id)} {...rowProps(r)} />
                ))}
              </div>
            </section>
          )}

          {showsFinished(filters.status) && finished.length > 0 && (
            <section className="sec" aria-label="Earlier runs">
              <h2>
                {filtered || filters.status !== "all" ? "Finished runs" : range.finishedHeading} <span>{filtered ? finished.length : totalFinished}</span>
                {filters.sort !== "newest" && <span className="note"> · ordering the {finished.length} loaded</span>}
              </h2>
              <div className="panel scroll">
                {days.map((g) => (
                  <div key={g.label || "all"}>
                    {g.label && <h3 className="dayh">{g.label}</h3>}
                    <div className="list" role="list" aria-label={g.label || "Finished runs"}>
                      {g.runs.map((r) => (
                        <RunRow key={r.id} run={r} now={now} tone={ended.has(r.id) ? "ended" : fresh.has(r.id) ? "fresh" : ""} {...rowProps(r)} />
                      ))}
                    </div>
                  </div>
                ))}
              </div>
            </section>
          )}
          {error && (
            <div className="err" role="alert">
              <span>{error}</span>
            </div>
          )}
          {showsFinished(filters.status) && snap.nextCursor && (
            <div className="loadmore">
              <Button type="button" onClick={() => void loadMore()} disabled={more}>
                {more ? "Loading…" : "Load older runs"}
              </Button>
            </div>
          )}
        </div>
        <Peek run={selected} now={now} onClose={() => setSelectedId(null)} onCancelled={() => void refresh()} />
      </div>
    </>
  );
}
