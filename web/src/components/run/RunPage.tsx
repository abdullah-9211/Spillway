"use client";

import Link from "next/link";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { layoutGraph, meters, recoveredLabel, type RunGraph } from "@/lib/graph";
import { EVENT_NAMES, TERMINAL, initialLive, liveLabel, nextLive, statusOf, type LiveEvent, type LiveState } from "@/lib/live";
import { useRole } from "@/lib/role";
import { runCost, shortSpan } from "@/lib/runs";
import { Button } from "../ui";
import { RunTag } from "../runs/parts";
import { GraphCanvas } from "./GraphCanvas";
import { Inspector, shortKey } from "./Inspector";
import { Timeline } from "./Timeline";

const POLL_MS = 3000;
const LIVE_STATUSES = new Set(["queued", "running", "waiting_tool", "waiting_human", "sleeping"]);

type View = "graph" | "timeline";

/** The default selection: the re-issued attempt (the point of the demo), else the one running, else the last. */
function defaultSelection(g: RunGraph): string | null {
  const pick = g.nodes.find((n) => n.reissued) ?? g.nodes.find((n) => n.state === "running") ?? g.nodes[g.nodes.length - 1];
  return pick ? `${pick.step_no}:${pick.epoch}` : null;
}

function useNow(every: number, on: boolean): Date {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    if (!on) return;
    const id = window.setInterval(() => setNow(new Date()), every);
    return () => window.clearInterval(id);
  }, [every, on]);
  return now;
}

export function RunPage({ initial, view: initialView }: { initial: RunGraph; view: View }) {
  const isAdmin = useRole() === "admin";
  const [graph, setGraph] = useState(initial);
  const [view, setView] = useState<View>(initialView);
  const [live, setLive] = useState<LiveState>(() => initialLive(initial.last_event_id, initial.run.status));
  const [selected, setSelected] = useState<string | null>(() => defaultSelection(initial));
  const [confirm, setConfirm] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const picked = useRef(false); // once the person picks a node, new data does not move the selection
  const id = initial.run.id;
  const isLive = LIVE_STATUSES.has(graph.run.status);
  const now = useNow(1000, isLive);

  const refetch = useCallback(async () => {
    try {
      const res = await fetch(`/api/runs/${id}/graph`, { cache: "no-store" });
      if (!res.ok) return;
      const data = (await res.json()) as RunGraph;
      setGraph(data);
      setSelected((cur) => (picked.current && cur ? cur : defaultSelection(data)));
      setLive((s) => nextLive(s, { type: "graph", status: data.run.status, lastEvent: data.last_event_id }).state);
    } catch {
      // the next event or poll tries again
    }
  }, [id]);

  // The event stream: each new row means the graph changed, so it is read again (a few at once are merged into one read).
  useEffect(() => {
    if (TERMINAL.has(initial.run.status)) return;
    let timer: number | undefined;
    let poll: number | undefined;
    const soon = () => {
      window.clearTimeout(timer);
      timer = window.setTimeout(() => void refetch(), 120);
    };
    const es = new EventSource(`/api/runs/${id}/events?after=${initial.last_event_id}`);
    const apply = (e: LiveEvent) => {
      setLive((s) => {
        const step = nextLive(s, e);
        if (step.refetch) soon();
        if (step.close) es.close();
        if (step.state.phase === "polling" && poll === undefined) poll = window.setInterval(() => void refetch(), POLL_MS);
        return step.state;
      });
    };
    es.onopen = () => apply({ type: "open" });
    es.onerror = () => apply({ type: "error", closed: es.readyState === EventSource.CLOSED });
    for (const name of EVENT_NAMES) {
      es.addEventListener(name, (ev) => {
        const m = ev as MessageEvent<string>;
        apply({ type: "step", id: Number(m.lastEventId), name, status: name === "run.status" ? statusOf(m.data) : undefined });
      });
    }
    return () => {
      es.close();
      window.clearTimeout(timer);
      window.clearInterval(poll);
    };
  }, [id, initial.run.status, initial.last_event_id, refetch]);

  const layout = useMemo(() => layoutGraph(graph), [graph]);
  const sel = graph.nodes.find((n) => `${n.step_no}:${n.epoch}` === selected) ?? null;
  const ms = meters(graph.run, now);
  const lastWorker = graph.workers.length ? graph.workers[graph.workers.length - 1].id : graph.run.lease_owner ?? "none yet";
  const status = { status: graph.run.status, wake_at: null };

  function choose(nodeId: string) {
    picked.current = true;
    setSelected(nodeId);
  }

  function switchView(v: View) {
    setView(v);
    window.history.replaceState(null, "", v === "graph" ? `/runs/${id}` : `/runs/${id}?view=timeline`);
  }

  async function cancel() {
    setBusy(true);
    setError(null);
    try {
      const res = await fetch(`/api/runs/${id}/cancel`, { method: "POST" });
      const data = (await res.json().catch(() => ({}))) as { message?: string };
      if (!res.ok) {
        setError(data.message ?? "Could not cancel the run.");
        return;
      }
      setConfirm(false);
      void refetch();
    } catch {
      setError("Could not reach the dashboard. Check your connection and try again.");
    } finally {
      setBusy(false);
    }
  }

  const deadline = new Date(graph.run.deadline_at).getTime() - now.getTime();
  const started = shortSpan(now.getTime() - new Date(graph.run.created_at).getTime());

  return (
    <>
      <nav className="crumb" aria-label="Breadcrumb">
        <Link href="/">Runs</Link>
        <span aria-hidden="true">/</span>
        <span className="mono">{id.slice(0, 8)}</span>
      </nav>
      <div className="head head--top">
        <div>
          <h1 className="h1 h1--run">{graph.run.goal}</h1>
          <div className="meta">
            <span>API key <b>{graph.run.key}</b></span>
            <span>Policy <b>{graph.run.model}</b></span>
            <span>Started <b>{started} ago</b></span>
            {isLive && deadline > 0 && <span>Deadline <b>in {shortSpan(deadline)}</b></span>}
            {graph.run.failure_reason && <span>Ended because <b>{graph.run.failure_reason.replace(/_/g, " ")}</b></span>}
          </div>
        </div>
        <div className="head__acts">
          <RunTag run={status} now={now} />
          {isLive && isAdmin && !confirm && <Button type="button" className="danger" onClick={() => setConfirm(true)} disabled={graph.run.cancel_requested}>{graph.run.cancel_requested ? "Cancelling…" : "Cancel run"}</Button>}
          {isLive && !isAdmin && <span className="small">Only admins can cancel</span>}
        </div>
      </div>
      {confirm && (
        <div className="peek__confirm" role="group" aria-label="Confirm cancelling">
          <span>Cancel this run? Steps already finished are kept.</span>
          <Button type="button" size="sm" disabled={busy} onClick={() => void cancel()}>{busy ? "Cancelling…" : "Yes, cancel it"}</Button>
          <Button type="button" size="sm" onClick={() => setConfirm(false)}>Keep running</Button>
        </div>
      )}
      {error && (
        <div className="err" role="alert">
          <span>{error}</span>
        </div>
      )}

      <section className="panel rkpis" aria-label="Run totals">
        {ms.map((m) => (
          <div key={m.label} className="rkpi">
            <div className="l">{m.label}</div>
            <div className="v num">{m.value} <small>{m.of}</small></div>
            <div className="rtrk"><i style={{ width: `${m.share * 100}%` }} /></div>
          </div>
        ))}
        <div className="rkpi">
          <div className="l">Worker</div>
          <div className="v mono-v">{lastWorker} <small>{recoveredLabel(graph.recoveries.length)}</small></div>
          <div className="rtrk"><i style={{ width: "0" }} /></div>
        </div>
      </section>

      <section className="panel" aria-label={view === "graph" ? "Run graph" : "Run timeline"}>
        <div className="gh">
          <h2>{view === "graph" ? "Run graph" : "Timeline"}</h2>
          <div className="gr">
            <span className={`live ${live.phase === "live" ? "" : live.phase === "ended" ? "paused" : "offline"}`} role="status">
              <i aria-hidden="true" />
              {liveLabel(live)}
            </span>
            <div className="seg" role="group" aria-label="View">
              <button type="button" className={view === "graph" ? "on" : ""} aria-pressed={view === "graph"} onClick={() => switchView("graph")}>Graph</button>
              <button type="button" className={view === "timeline" ? "on" : ""} aria-pressed={view === "timeline"} onClick={() => switchView("timeline")}>Timeline</button>
            </div>
          </div>
        </div>
        {view === "graph" ? (
          <>
            <GraphCanvas layout={layout} selected={selected} onSelect={choose} />
            <div className="leg">
              <span><i className="lk" style={{ borderRadius: 7 }} />Model call</span>
              <span><i className="lk" style={{ borderRadius: 3 }} />Tool call</span>
              <span><i className="lk" style={{ border: "1.5px dashed var(--fail)", borderRadius: 3 }} />Attempt that stopped</span>
              <span><i className="lk" style={{ border: "1.5px dashed var(--run)", borderRadius: 3 }} />Re-issued after recovery</span>
              <span><i className="lk" style={{ borderColor: "var(--run)", borderRadius: 3 }} />Running now</span>
            </div>
          </>
        ) : (
          <div className="tlwrap">
            <Timeline graph={graph} selected={selected} onSelect={choose} />
            <aside className="side2" aria-label="Run details">
              <section className="panel sp">
                <h3>Worker lease</h3>
                <dl className="rdl">
                  <dt>Held by</dt><dd className="mono">{isLive ? graph.run.lease_owner ?? "nobody" : "released"}</dd>
                  <dt>Lease epoch</dt><dd className="num">{graph.run.lease_epoch}</dd>
                  <dt>Lease expires in</dt><dd className="num">{isLive && graph.run.lease_expires_at ? shortSpan(new Date(graph.run.lease_expires_at).getTime() - now.getTime()) : "-"}</dd>
                  <dt>Recoveries</dt><dd className="num">{graph.recoveries.length}</dd>
                </dl>
              </section>
              <section className="panel sp">
                <h3>Tools allowed</h3>
                {graph.run.tools.length === 0 ? <p className="mute">None. This run only calls the model.</p> : <div className="rchips">{graph.run.tools.map((t) => <span key={t} className="tag mono">{t}</span>)}</div>}
              </section>
              <section className="panel sp">
                <h3>Cost</h3>
                <p className="num" style={{ margin: 0 }}>{runCost(graph.run.cost_usd)} of {runCost(graph.run.max_cost_usd)}</p>
              </section>
            </aside>
          </div>
        )}
      </section>

      {view === "graph" && <Inspector node={sel} all={graph.nodes} />}
      {view === "timeline" && sel && sel.idempotency_key && (
        <p className="small">Selected step {sel.step_no} sent idempotency key <span className="mono">{shortKey(sel.idempotency_key)}</span>.</p>
      )}
    </>
  );
}
