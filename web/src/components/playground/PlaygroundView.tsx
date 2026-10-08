"use client";

import { useEffect, useRef, useState } from "react";
import { usd } from "@/lib/format";
import {
  SUGGESTIONS, MAX_PROMPT, answerFor, chipsFor, emptyOptions, keepFaults, providerLabel, recentMeta, requestBody, toggleFault, validate,
  type Fault, type HistoryItem, type Options, type PlaygroundResult, type PlaygroundState,
} from "@/lib/playground";
import { useRole } from "@/lib/role";
import { dollarsOf, microsOf } from "@/lib/usage";
import { Button } from "../ui";
import { RouteTrace } from "./RouteTrace";

type Shown = { prompt: string; result: PlaygroundResult };

export function PlaygroundView({ state, history, nextCursor }: { state: PlaygroundState; history: HistoryItem[]; nextCursor: string | null }) {
  const isAdmin = useRole() === "admin";
  // The default policy first, the rest in the order the service lists them.
  const policies = [...state.policies].sort((a, b) => Number(b.name === "default") - Number(a.name === "default"));
  const [policy, setPolicy] = useState(policies.find((p) => p.name === "default")?.name ?? policies[0]?.name ?? "");
  const [prompt, setPrompt] = useState("");
  const [faults, setFaults] = useState<Fault[]>([]);
  const [opts, setOpts] = useState<Options>(emptyOptions);
  const [showOpts, setShowOpts] = useState(false);
  const [shown, setShown] = useState<Shown | null>(null);
  const [asking, setAsking] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [recent, setRecent] = useState<HistoryItem[]>(history);
  const [cursor, setCursor] = useState(nextCursor);
  const [more, setMore] = useState(false);
  const [spend, setSpend] = useState(state.key.spend_usd);
  const resultRef = useRef<HTMLElement>(null);
  const current = policies.find((p) => p.name === policy);
  const sending = asking !== null;
  const chips = state.fault_injection && current ? chipsFor(current.providers) : [];
  const fieldErrors = validate(prompt, opts);

  useEffect(() => {
    if (shown) resultRef.current?.focus({ preventScroll: false });
  }, [shown]);

  function choose(name: string) {
    setPolicy(name);
    const p = policies.find((x) => x.name === name);
    if (p) setFaults((f) => keepFaults(f, p.providers));
  }

  async function send() {
    setError(null);
    if (Object.keys(fieldErrors).length > 0) {
      setError(Object.values(fieldErrors)[0] ?? null);
      return;
    }
    const text = prompt.trim();
    setAsking(text);
    try {
      const res = await fetch("/api/playground/chat", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(requestBody(policy, text, opts, faults)) });
      const data = (await res.json().catch(() => ({}))) as PlaygroundResult & { message?: string };
      if (!res.ok) {
        setError(data.message ?? "Something went wrong. Try again.");
        return;
      }
      setShown({ prompt: text, result: data });
      setRecent((r) => [{ ...data, prompt: text, system: opts.system.trim() }, ...r]);
      setSpend((s) => dollarsOf(microsOf(s) + microsOf(data.cost_usd)));
    } catch {
      setError("Could not reach the dashboard. Check your connection and try again.");
    } finally {
      setAsking(null);
    }
  }

  async function loadMore() {
    if (!cursor) return;
    setMore(true);
    try {
      const res = await fetch(`/api/playground/history?cursor=${encodeURIComponent(cursor)}`);
      const data = (await res.json().catch(() => ({}))) as { requests?: HistoryItem[]; next_cursor?: string | null; message?: string };
      if (!res.ok || !data.requests) {
        setError(data.message ?? "Could not load older requests.");
        return;
      }
      const older = data.requests;
      setRecent((r) => [...r, ...older.filter((o) => !r.some((x) => x.id === o.id))]);
      setCursor(data.next_cursor ?? null);
    } catch {
      setError("Could not reach the dashboard. Check your connection and try again.");
    } finally {
      setMore(false);
    }
  }

  const budget = state.key.monthly_budget_usd;
  const answer = shown ? answerFor(shown.result) : null;

  return (
    <>
      <div className="head">
        <h1 className="h1">Playground</h1>
        <span className="tag num">
          Playground key, {usd(spend)}
          {budget ? ` of ${usd(budget).replace(/\.00$/, "")}` : ""} used this month
        </span>
      </div>

      <section className="pols" aria-label="Routing policy">
        {policies.map((p) => (
          <button key={p.name} type="button" className={`pol ${p.name === policy ? "on" : ""}`.trim()} aria-pressed={p.name === policy} onClick={() => choose(p.name)}>
            <span className="nm">
              {p.name}
              <svg width="14" height="14" viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" style={{ visibility: p.name === policy ? "visible" : "hidden" }}>
                <path d="M2.5 6.5l2.2 2.2L9.5 3.5" />
              </svg>
            </span>
            <span className="chain" aria-hidden="true">
              {(p.type === "fixed" || p.type === "cheapest" ? [p.models[0]] : p.models).map((m, i) => (
                <i key={`${m?.id}${i}`} className={`cd ${p.type === "weighted" || (p.hedge_after_ms > 0 && i > 0) ? "sq" : ""} ${p.type === "cheapest" ? "hi" : ""}`.trim()} />
              ))}
            </span>
            <span className="ds">{p.description}</span>
          </button>
        ))}
      </section>

      <div className="row">
        <div className="stage">
          {(asking ?? shown?.prompt) && <div className="me">{asking ?? shown?.prompt}</div>}

          {sending && (
            <section className="panel ai" aria-live="polite">
              <p className="ans mute">Waiting for the answer…</p>
            </section>
          )}

          {!sending && shown && answer && (
            <section className="panel ai" aria-label="Answer and route" tabIndex={-1} ref={resultRef}>
              <RouteTrace result={shown.result} />
              {answer.failed && (
                <div className="err" role="alert">
                  <span>
                    {answer.partial ? "The answer was cut off: " : "No provider could answer: "}
                    {shown.result.error?.message}
                  </span>
                </div>
              )}
              {answer.text && !(answer.failed && !answer.partial) && <p className="ans">{answer.text}</p>}
              <div className="facts num">
                {shown.result.model && (
                  <span>
                    <b className="mono-id">{shown.result.model}</b> on {shown.result.provider}
                  </span>
                )}
                <span>
                  <b>{shown.result.input_tokens}</b> in, <b>{shown.result.output_tokens}</b> out
                </span>
                <span>
                  <b>{usd(shown.result.cost_usd)}</b>
                </span>
                <span>
                  Cache <b>{shown.result.cache === "miss" ? "miss" : shown.result.cache === "bypass" ? "bypassed" : "hit"}</b>
                </span>
              </div>
              {shown.result.unused_faults.length > 0 && (
                <p className="small">
                  Not used, because this policy never tries them: {shown.result.unused_faults.map((f) => providerLabel(f.provider)).join(", ")}.
                </p>
              )}
            </section>
          )}

          {!sending && !shown && (
            <section className="panel ai">
              <p className="ans mute">Pick a policy, break a provider if you like, and send a prompt. The route it takes shows up here.</p>
            </section>
          )}

          <section className="panel comp" aria-label="Send a prompt">
            <label className="sr-only" htmlFor="pg-prompt">Prompt</label>
            <textarea
              id="pg-prompt" className="pr" value={prompt} disabled={!isAdmin || sending} maxLength={MAX_PROMPT + 1}
              placeholder="Ask anything, then break a provider to watch the fallback"
              aria-invalid={!!error && !!fieldErrors.prompt}
              onChange={(e) => setPrompt(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && (e.metaKey || e.ctrlKey) && !sending) void send();
              }}
            />
            {chips.length > 0 && (
              <div className="chips" role="group" aria-label="Make a provider fail">
                {chips.map((c) => {
                  const on = faults.some((f) => f.provider === c.fault.provider && f.kind === c.fault.kind);
                  return (
                    <button key={c.id} type="button" className={`cz ${on ? "on" : ""}`.trim()} aria-pressed={on} disabled={!isAdmin || sending} onClick={() => setFaults((f) => toggleFault(f, c.fault))}>
                      <svg width="12" height="12" viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                        <path d="M7 1L2.5 7H6l-1 4L9.5 5H6z" />
                      </svg>
                      {c.label}
                    </button>
                  );
                })}
              </div>
            )}
            {!state.fault_injection && <p className="small">Fault injection is switched off in the configuration.</p>}
            {showOpts && isAdmin && (
              <div className="opts">
                <div>
                  <label className="lab" htmlFor="pg-system">System prompt</label>
                  <textarea id="pg-system" className="inp" rows={2} value={opts.system} onChange={(e) => setOpts({ ...opts, system: e.target.value })} />
                </div>
                <div>
                  <label className="lab" htmlFor="pg-temp">Temperature</label>
                  <input id="pg-temp" className="inp num" inputMode="decimal" placeholder="Provider default" value={opts.temperature} onChange={(e) => setOpts({ ...opts, temperature: e.target.value })} aria-invalid={!!fieldErrors.temperature} />
                  {fieldErrors.temperature && <p className="field-err">{fieldErrors.temperature}</p>}
                </div>
                <div>
                  <label className="lab" htmlFor="pg-max">Max tokens</label>
                  <input id="pg-max" className="inp num" inputMode="numeric" placeholder="Provider default" value={opts.maxTokens} onChange={(e) => setOpts({ ...opts, maxTokens: e.target.value })} aria-invalid={!!fieldErrors.maxTokens} />
                  {fieldErrors.maxTokens && <p className="field-err">{fieldErrors.maxTokens}</p>}
                </div>
              </div>
            )}
            <div className="ctl">
              <p className="small" aria-live="polite">
                {faults.length > 0 ? `${faults.length} ${faults.length === 1 ? "fault" : "faults"} will be injected` : "No faults"}
              </p>
              <div className="ctl__btns">
                <Button type="button" disabled={!isAdmin} aria-expanded={showOpts} onClick={() => setShowOpts((s) => !s)}>Options</Button>
                <Button type="button" variant="primary" disabled={!isAdmin || sending} onClick={() => void send()}>{sending ? "Sending…" : "Send"}</Button>
              </div>
            </div>
            {error && <div className="err" role="alert"><span>{error}</span></div>}
            {isAdmin && (
              <div className="try">
                <span>Try</span>
                {SUGGESTIONS.map((s) => (
                  <button key={s} type="button" disabled={sending} onClick={() => setPrompt(s)}>{s}</button>
                ))}
              </div>
            )}
            {!isAdmin && (
              <div className="lock">
                <svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" style={{ flex: "none", marginTop: 2 }}>
                  <rect x="3" y="7" width="10" height="7" rx="1.5" />
                  <path d="M5.5 7V5a2.5 2.5 0 0 1 5 0v2" />
                </svg>
                <span>Sending prompts spends money, so only admins can. You can read past requests on the right.</span>
              </div>
            )}
          </section>
        </div>

        <aside className="panel rail" aria-label="Recent requests">
          <h2 className="pg__h2">Recent</h2>
          {recent.length === 0 && <p className="small">Nothing yet. Requests you send here are kept.</p>}
          <ul className="hist">
            {recent.map((h) => (
              <li key={h.id}>
                <button type="button" className={`hr ${shown?.result.id === h.id ? "on" : ""}`.trim()} onClick={() => { setShown({ prompt: h.prompt, result: h }); setError(null); }}>
                  <span className="q">{h.prompt}</span>
                  <span className="m num">
                    <span>{recentMeta(h)}</span>
                    <span>{usd(h.cost_usd)}</span>
                  </span>
                </button>
              </li>
            ))}
          </ul>
          {cursor && <Button size="sm" type="button" onClick={() => void loadMore()} disabled={more}>{more ? "Loading…" : "Older requests"}</Button>}
        </aside>
      </div>
    </>
  );
}
