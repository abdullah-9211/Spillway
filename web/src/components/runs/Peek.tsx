"use client";

import Link from "next/link";
import { useState } from "react";
import { reasonLabel, runCost, runDuration, stripLabel, type RunItem } from "@/lib/runs";
import { useRole } from "@/lib/role";
import { Button } from "../ui";
import { RunTag, StepStrip } from "./parts";

const LIVE = new Set(["queued", "running", "waiting_tool", "waiting_human", "sleeping"]);

/** The selected run's details, beside the list, with the things an admin can do to it. */
export function Peek({ run, now, onClose, onCancelled }: { run: RunItem | null; now: Date; onClose: () => void; onCancelled: () => void }) {
  const isAdmin = useRole() === "admin";
  const [confirm, setConfirm] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);

  if (!run) {
    return (
      <aside className="panel peek peek--empty" aria-label="Run details">
        <p className="mute">Select a run to see its steps, cost and what you can do with it.</p>
        <p className="small">Double-click a run, or use Open run, to see all of it. Press <kbd className="kbd">j</kbd> and <kbd className="kbd">k</kbd> to move between runs.</p>
      </aside>
    );
  }

  const live = LIVE.has(run.status);

  async function cancel() {
    if (!run) return;
    setBusy(true);
    setError(null);
    try {
      const res = await fetch(`/api/runs/${run.id}/cancel`, { method: "POST" });
      const data = (await res.json().catch(() => ({}))) as { message?: string };
      if (!res.ok) {
        setError(data.message ?? "Could not cancel the run.");
        return;
      }
      setConfirm(false);
      onCancelled();
    } catch {
      setError("Could not reach the dashboard. Check your connection and try again.");
    } finally {
      setBusy(false);
    }
  }

  async function copy() {
    try {
      await navigator.clipboard.writeText(run?.id ?? "");
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      // clipboard blocked: the id is shown in full on the run page
    }
  }

  return (
    <aside className="panel peek" aria-label="Run details" key={run.id}>
      <div className="peek__head">
        <RunTag run={run} now={now} />
        <button type="button" className="link-btn" onClick={onClose} aria-label="Close the details">Close</button>
      </div>
      <h2 className="peek__goal">{run.goal}</h2>
      <dl className="peek__facts num">
        <div><dt>API key</dt><dd>{run.key}</dd></div>
        <div><dt>Model or policy</dt><dd><span className="mono">{run.model}</span></dd></div>
        <div><dt>Steps</dt><dd>{run.step_count}</dd></div>
        <div><dt>Cost</dt><dd>{runCost(run.cost_usd)}</dd></div>
        <div><dt>{run.finished_at ? "Took" : "Running for"}</dt><dd>{runDuration(run, now)}</dd></div>
        <div><dt>Started</dt><dd>{new Date(run.created_at).toLocaleString("en-GB", { day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" })}</dd></div>
        {run.failure_reason && <div><dt>Ended because</dt><dd>{reasonLabel(run.failure_reason)}</dd></div>}
      </dl>
      <div>
        <h3 className="peek__h">Steps</h3>
        <StepStrip strip={run.strip} />
        <p className="small peek__legend">{stripLabel(run.strip)}. Round dots are model calls, square ones are tool calls.</p>
      </div>
      {error && (
        <div className="err" role="alert">
          <span>{error}</span>
        </div>
      )}
      <div className="peek__acts">
        <Link className="btn pri" href={`/runs/${run.id}`}>Open run</Link>
        <Button type="button" onClick={() => void copy()}>{copied ? "Copied" : "Copy id"}</Button>
        {live && isAdmin && !confirm && <Button type="button" onClick={() => setConfirm(true)}>Cancel run</Button>}
      </div>
      {live && isAdmin && confirm && (
        <div className="peek__confirm" role="group" aria-label="Confirm cancelling">
          <span>Cancel this run? Work already done is kept.</span>
          <Button type="button" size="sm" disabled={busy} onClick={() => void cancel()}>{busy ? "Cancelling…" : "Yes, cancel it"}</Button>
          <Button type="button" size="sm" onClick={() => setConfirm(false)}>Keep running</Button>
        </div>
      )}
      {live && !isAdmin && <p className="small">Only admins can cancel runs.</p>}
    </aside>
  );
}
