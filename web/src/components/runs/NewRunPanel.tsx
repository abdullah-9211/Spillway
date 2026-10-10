"use client";

import { useEffect, useRef, useState } from "react";
import { MAX_TASK, TASK_IDEAS, emptyNewRun, newRunBody, validateNewRun, type NewRunForm } from "@/lib/runs";
import { Button } from "../ui";

type Policy = { name: string; description: string };

/** Starts a run. It is made under the playground key, so it counts against that key's budget and rate limit. */
export function NewRunPanel({ policies, onStarted, onClose }: { policies: Policy[]; onStarted: (id: string) => void; onClose: () => void }) {
  const [f, setF] = useState<NewRunForm>({ ...emptyNewRun, model: policies.find((p) => p.name === "default")?.name ?? policies[0]?.name ?? "default" });
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [tried, setTried] = useState(false);
  const ref = useRef<HTMLTextAreaElement>(null);
  const errors = validateNewRun(f);
  const set = (k: keyof NewRunForm, v: string) => setF((x) => ({ ...x, [k]: v }));

  useEffect(() => {
    ref.current?.focus();
  }, []);

  async function submit() {
    setTried(true);
    setError(null);
    if (Object.keys(errors).length > 0) return;
    setPending(true);
    try {
      const res = await fetch("/api/runs", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(newRunBody(f)) });
      const data = (await res.json().catch(() => ({}))) as { id?: string; message?: string };
      if (!res.ok || !data.id) {
        setError(data.message ?? "Could not start the run. Try again.");
        return;
      }
      onStarted(data.id);
    } catch {
      setError("Could not reach the dashboard. Check your connection and try again.");
    } finally {
      setPending(false);
    }
  }

  const shown = (k: keyof NewRunForm) => (tried ? errors[k] : undefined);
  const policy = policies.find((p) => p.name === f.model);

  return (
    <section className="panel newrun" aria-label="Start a run">
      <div className="newrun__head">
        <h2 className="ttl">Start a run</h2>
        <button type="button" className="link-btn" onClick={onClose}>Close</button>
      </div>
      <div>
        <label className="lab" htmlFor="nr-task">What should the run do?</label>
        <textarea
          id="nr-task" ref={ref} className="pr" rows={3} value={f.task} maxLength={MAX_TASK + 1} aria-invalid={!!shown("task")} aria-describedby={shown("task") ? "nr-task-err" : undefined}
          placeholder="Describe the task in plain words. The run keeps calling the model until it is done."
          onChange={(e) => set("task", e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) void submit();
          }}
        />
        {shown("task") && <p id="nr-task-err" className="field-err">{shown("task")}</p>}
        <div className="try">
          <span>Try</span>
          {TASK_IDEAS.map((t) => (
            <button key={t} type="button" onClick={() => set("task", t)}>{t}</button>
          ))}
        </div>
      </div>
      <div className="newrun__grid">
        <div>
          <label className="lab" htmlFor="nr-policy">Routing policy</label>
          <select id="nr-policy" className="inp" value={f.model} onChange={(e) => set("model", e.target.value)}>
            {policies.map((p) => (
              <option key={p.name} value={p.name}>{p.name}</option>
            ))}
          </select>
          {policy?.description && <p className="small">{policy.description}</p>}
        </div>
        <div>
          <label className="lab" htmlFor="nr-system">System prompt (optional)</label>
          <input id="nr-system" className="inp" value={f.system} onChange={(e) => set("system", e.target.value)} placeholder="For example: answer in one paragraph" />
        </div>
      </div>
      <details className="newrun__limits">
        <summary>Limits</summary>
        <p className="small">Leave a field empty to use the server&apos;s own limit. A run can ask for less than the server allows, never more.</p>
        <div className="newrun__grid3">
          <div>
            <label className="lab" htmlFor="nr-steps">Most steps</label>
            <input id="nr-steps" className="inp num" inputMode="numeric" value={f.maxSteps} onChange={(e) => set("maxSteps", e.target.value)} aria-invalid={!!shown("maxSteps")} />
            {shown("maxSteps") && <p className="field-err">{shown("maxSteps")}</p>}
          </div>
          <div>
            <label className="lab" htmlFor="nr-cost">Most it may cost (USD)</label>
            <input id="nr-cost" className="inp num" inputMode="decimal" value={f.maxCost} onChange={(e) => set("maxCost", e.target.value)} aria-invalid={!!shown("maxCost")} />
            {shown("maxCost") && <p className="field-err">{shown("maxCost")}</p>}
          </div>
          <div>
            <label className="lab" htmlFor="nr-deadline">Give up after (minutes)</label>
            <input id="nr-deadline" className="inp num" inputMode="decimal" value={f.deadlineMinutes} onChange={(e) => set("deadlineMinutes", e.target.value)} aria-invalid={!!shown("deadlineMinutes")} />
            {shown("deadlineMinutes") && <p className="field-err">{shown("deadlineMinutes")}</p>}
          </div>
        </div>
      </details>
      {error && (
        <div className="err" role="alert">
          <span>{error}</span>
        </div>
      )}
      <div className="newrun__foot">
        <p className="small">Runs started here are charged to the playground key. Tools are not available from the dashboard yet.</p>
        <div className="newrun__btns">
          <Button type="button" onClick={onClose}>Cancel</Button>
          <Button type="button" variant="primary" disabled={pending} onClick={() => void submit()}>{pending ? "Starting…" : "Start run"}</Button>
        </div>
      </div>
    </section>
  );
}
