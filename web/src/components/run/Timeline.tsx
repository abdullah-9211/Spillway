import { useState } from "react";
import { ICONS, type GraphNode, type RunGraph } from "@/lib/graph";
import { keepNode, type StepFilter } from "@/lib/waterfall";
import { shortKey } from "./Inspector";

const TITLE: Record<GraphNode["type"], string> = { model_call: "Model call", tool_call: "Tool call", wait_human: "Approval", sleep: "Sleep", compaction: "Compaction" };

function who(n: GraphNode): string {
  if (n.type === "tool_call") return n.tool || "tool";
  if (n.type === "wait_human") return n.gate && n.tool ? n.tool : "request_human_approval";
  if (n.type === "sleep") return "sleep";
  if (n.type === "model_call") return n.provider && n.model ? `${n.provider}/${n.model}` : n.model || "model";
  return TITLE[n.type].toLowerCase();
}

function sub(n: GraphNode): string {
  if (n.type === "model_call" && n.tokens) return `${n.tokens.in.toLocaleString("en-US")} in, ${n.tokens.out.toLocaleString("en-US")} out`;
  if (n.idempotency_key) return `idempotency key ${shortKey(n.idempotency_key)}`;
  return "";
}

function tags(n: GraphNode): string[] {
  const t: string[] = [];
  if (n.type === "model_call" && n.attempts.length > 1 && (n.attempts[0].error_kind || n.attempts[0].status)) {
    const a = n.attempts[0];
    t.push(`Fell back from ${a.provider}/${a.model} after a ${a.status ?? a.error_kind}`);
  }
  if (n.cache === "hit_exact") t.push("Exact cache hit");
  else if (n.cache === "hit_semantic") t.push("Semantic cache hit");
  else if (n.type === "model_call" && n.state === "finished") t.push("Cache miss");
  if (n.reissued) t.push(n.idempotency_key ? "Same key as the first attempt" : "Re-issued after recovery");
  if (n.state === "stopped") t.push("Stopped before it finished");
  if (n.state === "running") t.push("Waiting for a result");
  if (n.state === "waiting") t.push("Waiting for a person");
  if (n.state === "sleeping") t.push("Asleep until its wake time");
  if (n.type === "wait_human" && n.decision) t.push(n.decision === "approve" ? `Approved by ${n.by || "someone"}` : `Rejected by ${n.by || "someone"}`);
  return t;
}

function body(n: GraphNode): string {
  const parts: string[] = [];
  if (n.type === "model_call") {
    if (n.message) parts.push(`Assistant: ${n.message}`);
  } else if (n.type === "wait_human") {
    if (n.reason) parts.push(`Asked: ${n.reason}`);
    if (n.arguments) parts.push(`Arguments: ${n.arguments}`);
    if (n.note) parts.push(`Note: ${n.note}`);
  } else if (n.type === "sleep") {
    if (n.seconds) parts.push(`Sleeps ${n.seconds} seconds`);
  } else if (n.type === "compaction") {
    if (n.message) parts.push(`Summary: ${n.message}`);
  } else {
    if (n.arguments) parts.push(`Arguments: ${n.arguments}`);
    if (n.result) parts.push(`Result: ${n.result}`);
  }
  if (n.error) parts.push(`Error: ${n.error}`);
  if (parts.length === 0) parts.push(n.state === "running" ? "In progress…" : n.state === "waiting" ? "Waiting for a decision." : n.state === "sleeping" ? "Sleeping." : n.state === "stopped" ? "This attempt never wrote a result." : "Nothing recorded.");
  return parts.join("\n");
}

const dur = (n: GraphNode) => (n.state === "running" ? "running" : n.state === "waiting" ? "waiting" : n.state === "sleeping" ? "sleeping" : n.duration_ms === null ? "stopped" : n.duration_ms < 1000 ? `${n.duration_ms} ms` : `${(n.duration_ms / 1000).toFixed(1)}s`);

/** The run as a vertical list of steps, with the recovery shown where it happened. */
const FILTERS: { id: StepFilter; label: string }[] = [
  { id: "all", label: "All steps" },
  { id: "model", label: "Model calls" },
  { id: "tool", label: "Tool calls" },
  { id: "problems", label: "Problems and re-issues" },
];

export function Timeline({ graph, selected, onSelect }: { graph: RunGraph; selected: string | null; onSelect: (id: string) => void }) {
  const [filter, setFilter] = useState<StepFilter>("all");
  const [open, setOpen] = useState<Set<string>>(new Set());
  const items = graph.nodes
    .map((n, i) => {
      const rec = i > 0 && n.epoch !== graph.nodes[i - 1].epoch ? graph.recoveries.find((r) => r.epoch === n.epoch) : undefined;
      return { n, rec, i };
    })
    .filter(({ n }) => keepNode(n, filter));
  const toggle = (id: string) =>
    setOpen((cur) => {
      const next = new Set(cur);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  return (
    <section className="tl" aria-label="Run steps">
      <div className="chips2 tl__filters" role="group" aria-label="Show">
        {FILTERS.map((f) => (
          <button key={f.id} type="button" className={`chip2 ${filter === f.id ? "on" : ""}`.trim()} aria-pressed={filter === f.id} onClick={() => setFilter(f.id)}>
            {f.label}
            <span className="num">{graph.nodes.filter((n) => keepNode(n, f.id)).length}</span>
          </button>
        ))}
      </div>
      {items.length === 0 && <p className="empty panel">{graph.nodes.length === 0 ? "The run has not started a step yet." : "No steps match this filter."}</p>}
      {items.map(({ n, rec, i }) => {
        const id = `${n.step_no}:${n.epoch}`;
        const tone = n.state === "running" ? "run" : n.state === "waiting" ? "wait" : n.state === "sleeping" ? "sleep" : n.state === "stopped" || n.state === "failed" ? "bad" : n.reissued ? "redo" : "";
        const icon = n.state === "running" ? ICONS.run : n.state === "waiting" ? ICONS.wait : n.state === "sleeping" ? ICONS.sleep : n.state === "stopped" || n.state === "failed" ? ICONS.fail : n.reissued ? ICONS.redo : ICONS.ok;
        return (
          <div key={id}>
            {rec && (
              <div className="recov">
                <svg width="20" height="20" viewBox="0 0 20 20" fill="none" stroke="var(--run)" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                  <path d="M16 10a6 6 0 1 1-2-4.5M16 3.5V6h-2.5" />
                </svg>
                <div>
                  <strong>Run recovered after a worker stopped</strong>
                  <span className="mute">
                    Worker {rec.from_worker} stopped sending heartbeats and its lease expired. Worker {rec.to_worker} claimed the run at lease epoch {rec.epoch}
                    {n.reissued ? ` and re-issued step ${n.step_no}${n.idempotency_key ? " with the same idempotency key, so a tool honouring the key cannot repeat its side effect" : ""}` : ""}.
                  </span>
                </div>
              </div>
            )}
            <div className="step">
              <div className="trail">
                <span className={`node ${tone}`}>
                  <svg width="12" height="12" viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d={icon} /></svg>
                </span>
                <span className="line" />
              </div>
              <div className={`panel card ${n.state === "running" ? "live" : n.state === "waiting" ? "live wait" : ""} ${selected === id ? "sel" : ""}`.trim()} style={{ animationDelay: `${Math.min(i, 12) * 40}ms` }}>
                <button type="button" className="card__select" aria-pressed={selected === id} aria-label={`Select step ${n.step_no}, ${TITLE[n.type].toLowerCase()}, attempt on ${n.worker}`} onClick={() => onSelect(id)} />
                <span className="ch">
                  <strong>{TITLE[n.type]}</strong>
                  <span className="mute">Step {n.step_no}</span>
                  <span className="r num">
                    <span>{dur(n)}</span>
                    <span>{Number(n.cost_usd) === 0 ? "free" : `$${Number(n.cost_usd).toFixed(4)}`}</span>
                  </span>
                </span>
                <span className="kv">
                  <span className="mono" style={{ color: "var(--text)" }}>{who(n)}</span>
                  {sub(n) && <span className={n.type === "model_call" ? "num" : "mono"}>{sub(n)}</span>}
                  {tags(n).map((t) => (
                    <span key={t} className="tag">{t}</span>
                  ))}
                </span>
                <span className={`code mono ${open.has(id) ? "open" : "clamp"}`}>{body(n)}</span>
                {body(n).length > 220 && (
                  <button type="button" className="link-btn card__more" onClick={() => toggle(id)} aria-expanded={open.has(id)}>
                    {open.has(id) ? "Show less" : "Show all"}
                  </button>
                )}
              </div>
            </div>
          </div>
        );
      })}
    </section>
  );
}
