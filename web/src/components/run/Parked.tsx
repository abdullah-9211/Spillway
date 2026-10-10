"use client";

import { useId, useRef, useState } from "react";
import { approvalView, countdown, waitedLabel, type Approval } from "@/lib/approval";
import { Button } from "../ui";

export type Decided = { verb: "approve" | "reject"; note: string };

/** What the person just did, kept on the page after the card itself is gone because the run moved on. */
export function DecisionNote({ done }: { done: Decided }) {
  return (
    <section className={`apv apv--done ${done.verb}`} aria-label="Decision recorded" role="status">
      <svg width="18" height="18" viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
        <path d={done.verb === "approve" ? "M2.5 6.5l2.2 2.2L9.5 3.5" : "M3 3l6 6M9 3l-6 6"} />
      </svg>
      <strong>{done.verb === "approve" ? "Approved. The run is picking up where it stopped." : "Rejected. The run ends as failed, with your reason on record."}</strong>
    </section>
  );
}

/**
 * A run that waits for a person. It shows what is about to happen (an email as an email, anything else as its fields),
 * why the model asked, and two decisions. A viewer sees all of it with the decisions disabled and told why. The run
 * itself is not held by any worker while it waits, so nothing here depends on a process staying up.
 */
export function ApprovalCard({ runId, approval, isAdmin, now, onDecided }: { runId: string; approval: Approval; isAdmin: boolean; now: Date; onDecided: (d?: Decided) => void }) {
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState<"approve" | "reject" | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [needReason, setNeedReason] = useState(false);
  const [done, setDone] = useState<Decided | null>(null);
  const noteRef = useRef<HTMLTextAreaElement>(null);
  const view = approvalView(approval);
  const noteId = useId();
  const reasonId = useId();

  async function decide(verb: "approve" | "reject") {
    if (verb === "reject" && note.trim() === "") {
      setNeedReason(true);
      noteRef.current?.focus();
      return;
    }
    setBusy(verb);
    setError(null);
    try {
      const res = await fetch(`/api/runs/${runId}/${verb}`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ note: note.trim() }) });
      const data = (await res.json().catch(() => ({}))) as { message?: string };
      if (!res.ok) {
        setError(data.message ?? "Could not record the decision.");
        if (res.status === 409) onDecided(); // someone else decided: show what happened
        return;
      }
      const d = { verb, note: note.trim() };
      setDone(d);
      onDecided(d);
    } catch {
      setError("Could not reach the dashboard. Check your connection and try again.");
    } finally {
      setBusy(null);
    }
  }

  if (done) return <DecisionNote done={done} />;

  const what = approval.gate ? (approval.tool ? `Wants to call ${approval.tool}` : "Wants to continue") : "The model is asking before it goes on";
  return (
    <section className="apv" aria-label="Approval needed">
      <header className="apv__head">
        <span className="apv__ic" aria-hidden="true">
          <svg width="16" height="16" viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" aria-hidden="true">
            <path d="M4 2.5v7M8 2.5v7" />
          </svg>
        </span>
        <div>
          <h2 className="apv__title">Waiting for your approval</h2>
          <p className="apv__sub">
            {what}. Waiting <b className="num">{waitedLabel(approval.since, now)}</b>. Nothing runs until someone decides, and no worker is holding this run.
          </p>
        </div>
      </header>

      <blockquote className="apv__why">{approval.reason}</blockquote>

      {view.kind === "email" && (
        <figure className="mail" aria-label="Preview of the email">
          <figcaption>Preview of what will be sent. Nothing has been sent yet.</figcaption>
          <dl className="mail__h">
            <dt>To</dt>
            <dd>{view.to}</dd>
            {view.cc && (
              <>
                <dt>Cc</dt>
                <dd>{view.cc}</dd>
              </>
            )}
            <dt>Subject</dt>
            <dd>{view.subject || "(no subject)"}</dd>
          </dl>
          <div className="mail__b">{view.body || "(empty message)"}</div>
        </figure>
      )}
      {view.kind === "fields" && (
        <figure className="mail mail--fields" aria-label="What the tool will receive">
          <figcaption>What {approval.tool ? <span className="mono">{approval.tool}</span> : "the next step"} will receive</figcaption>
          <dl className="mail__h">
            {view.rows.map(([k, v]) => (
              <div key={k} className="mail__row">
                <dt className="mono">{k}</dt>
                <dd>{v}</dd>
              </div>
            ))}
          </dl>
        </figure>
      )}

      <div className="apv__note">
        <label className="lab" htmlFor={noteId}>
          Note {isAdmin ? <span className="mute">(the model reads it when the run goes on; a reason is needed to reject)</span> : null}
        </label>
        <textarea
          id={noteId}
          ref={noteRef}
          className="inp apv__ta"
          rows={2}
          maxLength={2000}
          value={note}
          disabled={!isAdmin || busy !== null}
          placeholder={isAdmin ? "Optional for approve. Say why when you reject." : "Only admins can decide."}
          aria-invalid={needReason && note.trim() === "" ? true : undefined}
          aria-describedby={needReason ? reasonId : undefined}
          onChange={(e) => {
            setNote(e.target.value);
            if (e.target.value.trim() !== "") setNeedReason(false);
          }}
        />
        {needReason && (
          <p id={reasonId} className="apv__need" role="alert">
            Add a reason so whoever started the run knows why it was stopped.
          </p>
        )}
      </div>

      <div className="apv__acts">
        <Button type="button" variant="primary" disabled={!isAdmin || busy !== null} onClick={() => void decide("approve")}>
          {busy === "approve" ? "Approving…" : "Approve and continue"}
        </Button>
        <Button type="button" className="danger" disabled={!isAdmin || busy !== null} onClick={() => void decide("reject")}>
          {busy === "reject" ? "Rejecting…" : "Reject"}
        </Button>
        {!isAdmin && <span className="small apv__viewer">You can see what is waiting. Only admins can approve or reject.</span>}
      </div>
      {error && (
        <div className="err" role="alert">
          <span>{error}</span>
        </div>
      )}
    </section>
  );
}

/** A run that is sleeping: a calm panel with the time left. The run is not held by any worker meanwhile. */
export function SleepCard({ wakeAt, seconds, now }: { wakeAt: string; seconds?: number; now: Date }) {
  const left = countdown(wakeAt, now);
  const at = new Date(wakeAt).toLocaleTimeString("en-US", { hour: "numeric", minute: "2-digit" });
  return (
    <section className="zz" aria-label="Sleeping">
      <svg className="zz__moon" width="30" height="30" viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
        <path d="M9.5 7A4 4 0 0 1 5 2.5a4 4 0 1 0 4.5 4.5z" />
      </svg>
      <div>
        <h2 className="zz__title">
          Sleeping, wakes in <span className="num">{left}</span>
        </h2>
        <p className="zz__sub">
          The model asked to wait{seconds ? ` ${seconds >= 60 ? `${Math.round(seconds / 60)} minutes` : `${seconds} seconds`}` : ""}. It wakes at {at}. No worker holds the run while it sleeps, and any worker can pick it up then.
        </p>
      </div>
    </section>
  );
}
