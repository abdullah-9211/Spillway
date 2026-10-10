"use client";

import { useEffect, useRef, useState } from "react";
import { MAX_TASK, TASK_IDEAS, emptyNewRun, newRunBody, validateNewRun, type NewRunForm } from "@/lib/runs";
import { Button } from "../ui";

type Policy = { name: string; description: string };
type ToolOption = { name: string; description: string; approval: boolean };

type RegistryTool = { name: string; kind: "http" | "mcp"; description: string; requires_approval: boolean; mcp_tools?: { name: string; description: string }[] };

/** What a run may be given: each HTTP tool, and each tool an MCP server was discovered to have. */
export function toolOptions(list: RegistryTool[]): ToolOption[] {
  const out: ToolOption[] = [];
  for (const t of list) {
    if (t.kind === "mcp") for (const m of t.mcp_tools ?? []) out.push({ name: m.name, description: m.description, approval: t.requires_approval });
    else out.push({ name: t.name, description: t.description, approval: t.requires_approval });
  }
  return out;
}

/** Starts a run. It is made under the playground key, so it counts against that key's budget and rate limit. */
export function NewRunPanel({ policies, onStarted, onClose }: { policies: Policy[]; onStarted: (id: string) => void; onClose: () => void }) {
  const [f, setF] = useState<NewRunForm>({ ...emptyNewRun, model: policies.find((p) => p.name === "default")?.name ?? policies[0]?.name ?? "default" });
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [tried, setTried] = useState(false);
  const [options, setOptions] = useState<ToolOption[]>([]);
  const ref = useRef<HTMLTextAreaElement>(null);
  const errors = validateNewRun(f);
  const set = (k: keyof NewRunForm, v: string) => setF((x) => ({ ...x, [k]: v }));

  useEffect(() => {
    ref.current?.focus();
  }, []);

  useEffect(() => {
    let dead = false;
    fetch("/api/tools", { cache: "no-store" })
      .then((r) => (r.ok ? r.json() : null))
      .then((d: { tools?: RegistryTool[] } | null) => {
        if (!dead && d?.tools) setOptions(toolOptions(d.tools));
      })
      .catch(() => undefined); // no tool picker if the registry cannot be read; the run still starts
    return () => {
      dead = true;
    };
  }, []);

  const toggle = (k: "tools" | "askFirst", name: string) =>
    setF((x) => {
      const cur = x[k] ?? [];
      const next = cur.includes(name) ? cur.filter((n) => n !== name) : [...cur, name];
      return k === "tools" ? { ...x, tools: next, askFirst: (x.askFirst ?? []).filter((n) => next.includes(n)) } : { ...x, askFirst: next };
    });

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
      {options.length > 0 && (
        <fieldset className="newrun__tools">
          <legend className="lab">Tools the run may call</legend>
          <p className="small">A call to a tool marked &quot;asks first&quot; waits for a person to approve it. The run also always has sleep and a way to ask for approval.</p>
          <ul>
            {options.map((o) => {
              const on = (f.tools ?? []).includes(o.name);
              const ask = o.approval || (f.askFirst ?? []).includes(o.name);
              return (
                <li key={o.name} className={on ? "on" : ""}>
                  <label>
                    <input type="checkbox" checked={on} onChange={() => toggle("tools", o.name)} />
                    <span className="mono">{o.name}</span>
                  </label>
                  {o.description && <span className="small mute">{o.description}</span>}
                  {on && (
                    <label className="newrun__ask">
                      <input type="checkbox" checked={ask} disabled={o.approval} onChange={() => toggle("askFirst", o.name)} />
                      <span>{o.approval ? "Asks first (set on the tool)" : "Ask me first"}</span>
                    </label>
                  )}
                </li>
              );
            })}
          </ul>
        </fieldset>
      )}
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
        <p className="small">Runs started here are charged to the playground key and count against its budget.</p>
        <div className="newrun__btns">
          <Button type="button" onClick={onClose}>Cancel</Button>
          <Button type="button" variant="primary" disabled={pending} onClick={() => void submit()}>{pending ? "Starting…" : "Start run"}</Button>
        </div>
      </div>
    </section>
  );
}
