import { ICONS, type GraphNode, type RunGraph } from "@/lib/graph";
import { shortKey } from "./Inspector";

const TITLE: Record<GraphNode["type"], string> = { model_call: "Model call", tool_call: "Tool call", wait_human: "Approval", sleep: "Sleep", compaction: "Compaction" };

function who(n: GraphNode): string {
  if (n.type === "tool_call") return n.tool || "tool";
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
  return t;
}

function body(n: GraphNode): string {
  const parts: string[] = [];
  if (n.type === "model_call") {
    if (n.message) parts.push(`Assistant: ${n.message}`);
  } else {
    if (n.arguments) parts.push(`Arguments: ${n.arguments}`);
    if (n.result) parts.push(`Result: ${n.result}`);
  }
  if (n.error) parts.push(`Error: ${n.error}`);
  if (parts.length === 0) parts.push(n.state === "running" ? "In progress…" : n.state === "stopped" ? "This attempt never wrote a result." : "Nothing recorded.");
  return parts.join("\n");
}

const dur = (n: GraphNode) => (n.state === "running" ? "running" : n.duration_ms === null ? "stopped" : n.duration_ms < 1000 ? `${n.duration_ms} ms` : `${(n.duration_ms / 1000).toFixed(1)}s`);

/** The run as a vertical list of steps, with the recovery shown where it happened. */
export function Timeline({ graph, selected, onSelect }: { graph: RunGraph; selected: string | null; onSelect: (id: string) => void }) {
  const items = graph.nodes.map((n, i) => {
    const rec = i > 0 && n.epoch !== graph.nodes[i - 1].epoch ? graph.recoveries.find((r) => r.epoch === n.epoch) : undefined;
    return { n, rec, i };
  });
  return (
    <section className="tl" aria-label="Run steps">
      {items.length === 0 && <p className="empty panel">The run has not started a step yet.</p>}
      {items.map(({ n, rec }) => {
        const id = `${n.step_no}:${n.epoch}`;
        const tone = n.state === "running" ? "run" : n.state === "stopped" || n.state === "failed" ? "bad" : n.reissued ? "redo" : "";
        const icon = n.state === "running" ? ICONS.run : n.state === "stopped" || n.state === "failed" ? ICONS.fail : n.reissued ? ICONS.redo : ICONS.ok;
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
              <button type="button" className={`panel card ${n.state === "running" ? "live" : ""} ${selected === id ? "sel" : ""}`.trim()} aria-pressed={selected === id} onClick={() => onSelect(id)}>
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
                <span className="code mono">{body(n)}</span>
              </button>
            </div>
          </div>
        );
      })}
    </section>
  );
}
