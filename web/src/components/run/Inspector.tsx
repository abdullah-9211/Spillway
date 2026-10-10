"use client";

import { useState } from "react";
import { durationLabel } from "@/lib/runs";
import type { GraphNode, RunGraph } from "@/lib/graph";
import { neighbours, pretty, tokens } from "@/lib/waterfall";

const KIND: Record<GraphNode["type"], string> = { model_call: "model call", tool_call: "tool call", wait_human: "approval", sleep: "sleep", compaction: "compaction" };

export function shortKey(k: string): string {
  return k.length > 12 ? `${k.slice(0, 6)}…${k.slice(-2)}` : k;
}

/** What to call an attempt's state: the word the tag shows. */
export function nodeStateWord(n: GraphNode): string {
  if (n.state === "waiting") return "Waiting for approval";
  if (n.state === "sleeping") return "Sleeping";
  if (n.type === "wait_human" && n.decision) return n.decision === "approve" ? "Approved" : "Rejected";
  if (n.state === "stopped") return "Stopped before it finished";
  if (n.state === "running") return "Running now";
  if (n.state === "failed") return "Failed";
  return n.reissued ? "Re-issued after recovery" : "Finished";
}

const mmss = (ms: number | null) => (ms === null ? "never ended" : ms < 1000 ? `${ms} ms` : durationLabel(ms));
const id = (n: GraphNode) => `${n.step_no}:${n.epoch}`;

/** The lines under a node: what happened in this attempt, in plain words. */
export function narrative(n: GraphNode, all: GraphNode[]): [string, string][] {
  const lines: [string, string][] = [];
  const same = all.filter((x) => x.step_no === n.step_no);
  if (same.length > 1) {
    same.forEach((x, i) => {
      const ordinal = ["first", "second", "third", "fourth", "fifth"][i] ?? `attempt ${i + 1}`;
      const how = x.state === "stopped" ? "stopped before it finished" : x.state === "failed" ? "failed" : x.state === "running" ? "is running" : "finished";
      lines.push([`${ordinal} attempt`, `started on ${x.worker} (epoch ${x.epoch})${x.reissued && i > 0 && x.idempotency_key ? " with the same key" : ""}, ${how}`]);
    });
  }
  if (n.type === "tool_call" && n.arguments) lines.push(["arguments", n.arguments]);
  if (n.type === "wait_human") {
    if (n.reason) lines.push(["asked", n.reason]);
    if (n.gate && n.tool) lines.push(["held tool", n.tool]);
    if (n.arguments) lines.push(["arguments", n.arguments]);
    if (n.decision) lines.push(["decision", `${n.decision === "approve" ? "approved" : "rejected"} by ${n.by || "someone"}${n.note ? `: ${n.note}` : ""}`]);
  }
  if (n.type === "sleep" && n.seconds) lines.push(["slept", `${n.seconds} seconds${n.wake_at ? `, until ${n.wake_at}` : ""}`]);
  if (n.type === "compaction" && n.message) lines.push(["summary", n.message]);
  if (n.type === "model_call") {
    n.attempts.forEach((a, i) => {
      const bad = a.error_kind || a.status;
      lines.push([`provider ${i + 1}`, `${a.provider}/${a.model} ${a.kind}${bad ? `, failed ${a.status ?? a.error_kind}` : ", answered"} in ${a.latency_ms} ms`]);
    });
    if (n.message) lines.push(["answer", n.message]);
  }
  if (n.result) lines.push(["result", n.result]);
  if (n.error) lines.push(["error", n.error]);
  if (n.state === "stopped" && lines.length === 0) lines.push(["note", "This attempt never wrote a result. Another worker carried on."]);
  return lines;
}

/** Text with light colouring when it is JSON, and a copy button. */
function Code({ text, label }: { text: string; label: string }) {
  const [copied, setCopied] = useState(false);
  const p = pretty(text);
  async function copy() {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1400);
    } catch {
      // clipboard blocked: the text is selectable
    }
  }
  return (
    <div className="codebox">
      <button type="button" className="codebox__copy" onClick={() => void copy()} aria-label={`Copy ${label}`}>{copied ? "Copied" : "Copy"}</button>
      <pre className="code mono" tabIndex={0} aria-label={label}>
        {p.json ? tokens(p.text).map((t, i) => <span key={i} className={`tk tk-${t.t}`}>{t.v}</span>) : p.text}
      </pre>
    </div>
  );
}

function Share({ label, value, of, text }: { label: string; value: number; of: number; text: string }) {
  const pct = of > 0 ? Math.min(100, Math.max(value > 0 ? 3 : 0, (value / of) * 100)) : 0;
  return (
    <div className="share">
      <div className="share__t"><span>{label}</span><span className="num">{text}</span></div>
      <div className="share__bar"><i style={{ width: `${pct}%` }} /></div>
    </div>
  );
}

type Tab = "overview" | "input" | "output" | "providers" | "attempts";

export function Inspector({ node, all, run, now, onSelect }: { node: GraphNode | null; all: GraphNode[]; run?: RunGraph["run"]; now?: Date; onSelect?: (id: string) => void }) {
  const [tab, setTab] = useState<Tab>("overview");
  if (!node) {
    return (
      <section className="panel insp2" aria-label="Selected step">
        <p className="mute insp2__empty">Select a node in the stage, the graph or the waterfall to inspect that step.</p>
      </section>
    );
  }
  const ids = all.map(id);
  const nb = neighbours(ids, id(node));
  const siblings = all.filter((x) => x.step_no === node.step_no);
  const tone =
    node.state === "stopped" || node.state === "failed" || node.decision === "reject" ? "fail" : node.state === "waiting" ? "wait" : node.state === "sleeping" ? "accent" : node.state === "running" || node.reissued ? "run" : "ok";
  const tabs: { id: Tab; label: string }[] = [{ id: "overview", label: "Overview" }];
  if (node.type === "tool_call" || (node.type === "wait_human" && node.arguments)) tabs.push({ id: "input", label: "Input" });
  tabs.push({ id: "output", label: node.error ? "Error" : "Output" });
  if (node.type === "model_call" && node.attempts.length > 0) tabs.push({ id: "providers", label: `Providers (${node.attempts.length})` });
  if (siblings.length > 1) tabs.push({ id: "attempts", label: `Attempts (${siblings.length})` });
  const active = tabs.some((t) => t.id === tab) ? tab : "overview";
  const maxLat = Math.max(1, ...node.attempts.map((a) => a.latency_ms));
  const totalMs = run ? Math.max(1, (run.finished_at ? new Date(run.finished_at).getTime() : (now ?? new Date(run.created_at)).getTime()) - new Date(run.created_at).getTime()) : 0;
  const cost = Number(node.cost_usd);

  return (
    <section className="panel insp2" aria-label="Selected step">
      <header className="insp2__head">
        <button type="button" className="iconbtn" onClick={() => nb.prev && onSelect?.(nb.prev)} disabled={!nb.prev} aria-label="Previous step">‹</button>
        <div className="insp2__title">
          <h3>Step {node.step_no}, {KIND[node.type]}</h3>
          <span className="mono small">{node.type === "tool_call" || node.type === "wait_human" ? node.tool || node.reason : node.type === "sleep" ? (node.seconds ? `${node.seconds} s` : "") : node.provider && node.model ? `${node.provider}/${node.model}` : node.model}</span>
        </div>
        <span className="tag" style={{ ["--c" as string]: `var(--${tone})` }}>
          <svg width="12" height="12" viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
            <path d={node.state === "stopped" || node.state === "failed" || node.decision === "reject" ? "M3 3l6 6M9 3l-6 6" : node.state === "waiting" ? "M4 2.5v7M8 2.5v7" : node.state === "sleeping" ? "M9.5 7A4 4 0 0 1 5 2.5a4 4 0 1 0 4.5 4.5z" : node.state === "running" ? "M6 1.5a4.5 4.5 0 1 0 4.5 4.5" : node.reissued ? "M10 6a4 4 0 1 1-1.2-2.8M10 2.5v2.3H7.7" : "M2.5 6.5l2.2 2.2L9.5 3.5"} />
          </svg>
          {nodeStateWord(node)}
        </span>
        <button type="button" className="iconbtn" onClick={() => nb.next && onSelect?.(nb.next)} disabled={!nb.next} aria-label="Next step">›</button>
      </header>
      <div className="insp2__tabs" role="tablist" aria-label="Step details">
        {tabs.map((t) => (
          <button key={t.id} type="button" role="tab" id={`tab-${t.id}`} aria-selected={active === t.id} aria-controls="insp-panel" className={active === t.id ? "on" : ""} onClick={() => setTab(t.id)}>
            {t.label}
          </button>
        ))}
      </div>
      <div className="insp2__body" role="tabpanel" id="insp-panel" aria-labelledby={`tab-${active}`} key={`${id(node)}:${active}`}>
        {active === "overview" && (
          <div className="insp2__cols">
            <dl className="rdl">
              {node.tool && (<><dt>Tool</dt><dd className="mono">{node.tool}</dd></>)}
              {node.type === "model_call" && node.model && (<><dt>Model</dt><dd className="mono">{node.provider ? `${node.provider}/${node.model}` : node.model}</dd></>)}
              <dt>Worker</dt><dd className="mono">{node.worker}</dd>
              <dt>Lease epoch</dt><dd className="num">{node.epoch}</dd>
              {node.idempotency_key && (<><dt>Idempotency key</dt><dd className="mono" title={node.idempotency_key}>{shortKey(node.idempotency_key)}</dd></>)}
              <dt>Duration</dt><dd className="num">{mmss(node.duration_ms)}</dd>
              <dt>Cost</dt><dd className="num">{cost === 0 ? "free" : `$${cost.toFixed(4)}`}</dd>
              {node.cache && node.cache !== "miss" && node.cache !== "bypass" && (<><dt>Cache</dt><dd>{node.cache.replace("hit_", "hit, ")}</dd></>)}
            </dl>
            <div className="insp2__shares">
              {node.duration_ms !== null && run && <Share label="Share of the run's time" value={node.duration_ms} of={totalMs} text={`${Math.round((node.duration_ms / totalMs) * 100)}%`} />}
              {run && Number(run.cost_usd) > 0 && <Share label="Share of the run's cost" value={cost} of={Number(run.cost_usd)} text={`${Math.round((cost / Number(run.cost_usd)) * 100)}%`} />}
              {node.tokens && (
                <div className="share">
                  <div className="share__t"><span>Tokens</span><span className="num">{node.tokens.in} in, {node.tokens.out} out</span></div>
                  <div className="share__bar split"><i style={{ width: `${(node.tokens.in / Math.max(1, node.tokens.in + node.tokens.out)) * 100}%` }} /></div>
                </div>
              )}
              {node.reissued && (
                <p className="insp2__note">
                  Re-issued after {node.previous_worker || "another worker"} stopped at epoch {node.previous_epoch}. {node.idempotency_key ? "It sent the same idempotency key, so a receiver that honours the key applied the effect once." : ""}
                </p>
              )}
            </div>
          </div>
        )}
        {active === "input" && (node.arguments ? <Code text={node.arguments} label="Tool arguments" /> : <p className="mute">No arguments were recorded.</p>)}
        {active === "output" &&
          (node.error ? (
            <Code text={node.error} label="Error" />
          ) : node.result ? (
            <Code text={node.result} label="Tool result" />
          ) : node.message ? (
            <Code text={node.message} label="Model answer" />
          ) : (
            <p className="mute">{node.state === "waiting" ? "Waiting for a person to decide." : node.state === "sleeping" ? "Asleep until the wake time." : node.state === "running" ? "Still running." : node.state === "stopped" ? "This attempt never wrote a result. Another worker carried on." : "Nothing was recorded."}</p>
          ))}
        {active === "providers" && (
          <ol className="prov">
            {node.attempts.map((a, i) => {
              const bad = !!(a.status || a.error_kind);
              return (
                <li key={i} className={bad ? "bad" : "good"}>
                  <span className="prov__who mono">{a.provider}/{a.model}</span>
                  <span className="prov__kind">{a.kind}{a.injected ? ", on purpose" : ""}</span>
                  <span className="prov__bar"><i style={{ width: `${Math.max(3, (a.latency_ms / maxLat) * 100)}%` }} /></span>
                  <span className="prov__ms num">{a.latency_ms} ms</span>
                  <span className="prov__res">{bad ? `Failed ${a.status ?? a.error_kind}` : "Answered"}</span>
                </li>
              );
            })}
          </ol>
        )}
        {active === "attempts" && (
          <ol className="att">
            {siblings.map((s, i) => (
              <li key={id(s)} className={id(s) === id(node) ? "cur" : ""}>
                <button type="button" onClick={() => onSelect?.(id(s))} aria-label={`Attempt ${i + 1} on ${s.worker}, ${s.state}`}>
                  <span className="att__n">{["First", "Second", "Third", "Fourth", "Fifth"][i] ?? `#${i + 1}`} attempt</span>
                  <span className="mono">{s.worker}, epoch {s.epoch}</span>
                  <span className={`att__s ${s.state}`}>{s.state === "stopped" ? "stopped before it finished" : s.state}{s.reissued ? ", re-issued" : ""}</span>
                  <span className="num">{mmss(s.duration_ms)}</span>
                </button>
              </li>
            ))}
          </ol>
        )}
      </div>
    </section>
  );
}
