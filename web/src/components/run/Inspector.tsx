import { durationLabel } from "@/lib/runs";
import type { GraphNode } from "@/lib/graph";

const KIND: Record<GraphNode["type"], string> = { model_call: "model call", tool_call: "tool call", wait_human: "approval", sleep: "sleep", compaction: "compaction" };

export function shortKey(k: string): string {
  return k.length > 12 ? `${k.slice(0, 6)}…${k.slice(-2)}` : k;
}

/** What to call an attempt's state: the word the tag shows. */
export function nodeStateWord(n: GraphNode): string {
  if (n.state === "stopped") return "Stopped before it finished";
  if (n.state === "running") return "Running now";
  if (n.state === "failed") return "Failed";
  return n.reissued ? "Re-issued after recovery" : "Finished";
}

const mmss = (ms: number | null) => (ms === null ? "never ended" : ms < 1000 ? `${ms} ms` : durationLabel(ms));

/** The lines under a node: what happened in this attempt, in plain words. */
export function narrative(n: GraphNode, all: GraphNode[]): [string, string][] {
  const lines: [string, string][] = [];
  const same = all.filter((x) => x.step_no === n.step_no);
  if (same.length > 1) {
    same.forEach((x, i) => {
      const ordinal = ["first", "second", "third", "fourth", "fifth"][i] ?? `attempt ${i + 1}`;
      const how = x.state === "stopped" ? "stopped before it finished" : x.state === "failed" ? "failed" : x.state === "running" ? "is running" : "finished";
      lines.push([`${ordinal} attempt`, `${x.reissued ? "started" : "started"} on ${x.worker} (epoch ${x.epoch})${x.reissued && i > 0 && x.idempotency_key ? " with the same key" : ""}, ${how}`]);
    });
  }
  if (n.type === "tool_call" && n.arguments) lines.push(["arguments", n.arguments]);
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

export function Inspector({ node, all }: { node: GraphNode | null; all: GraphNode[] }) {
  if (!node) {
    return (
      <section className="panel insp" aria-label="Selected step">
        <div className="ic">
          <p className="mute">Select any node in the graph to inspect that step.</p>
        </div>
      </section>
    );
  }
  const tone = node.state === "stopped" || node.state === "failed" ? "fail" : node.state === "running" || node.reissued ? "run" : "ok";
  const lines = narrative(node, all);
  return (
    <section className="panel insp" aria-label="Selected step">
      <div className="ip">
        <h3>
          Step {node.step_no}, {KIND[node.type]}
        </h3>
        <span className="tag" style={{ ["--c" as string]: `var(--${tone})` }}>
          <svg width="12" height="12" viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
            <path d={node.state === "stopped" || node.state === "failed" ? "M3 3l6 6M9 3l-6 6" : node.state === "running" ? "M6 1.5a4.5 4.5 0 1 0 4.5 4.5" : node.reissued ? "M10 6a4 4 0 1 1-1.2-2.8M10 2.5v2.3H7.7" : "M2.5 6.5l2.2 2.2L9.5 3.5"} />
          </svg>
          {nodeStateWord(node)}
        </span>
        <dl className="rdl">
          {node.tool && (<><dt>Tool</dt><dd className="mono">{node.tool}</dd></>)}
          {node.type === "model_call" && node.model && (<><dt>Model</dt><dd className="mono">{node.provider ? `${node.provider}/${node.model}` : node.model}</dd></>)}
          <dt>Worker</dt><dd className="mono">{node.worker}</dd>
          <dt>Lease epoch</dt><dd className="num">{node.epoch}</dd>
          {node.idempotency_key && (<><dt>Idempotency key</dt><dd className="mono" title={node.idempotency_key}>{shortKey(node.idempotency_key)}</dd></>)}
          <dt>Duration</dt><dd className="num">{mmss(node.duration_ms)}</dd>
          <dt>Cost</dt><dd className="num">{Number(node.cost_usd) === 0 ? "free" : `$${Number(node.cost_usd).toFixed(4)}`}</dd>
          {node.tokens && (<><dt>Tokens</dt><dd className="num">{node.tokens.in} in, {node.tokens.out} out</dd></>)}
          {node.cache && node.cache !== "miss" && node.cache !== "bypass" && (<><dt>Cache</dt><dd>{node.cache.replace("hit_", "hit, ")}</dd></>)}
        </dl>
      </div>
      <div className="ic">
        {lines.length > 0 ? (
          <div className="code mono">
            {lines.map(([k, v], i) => (
              <div key={i}>
                <b>{k.padEnd(16, " ")}</b>
                {v}
              </div>
            ))}
          </div>
        ) : (
          <p className="mute">Nothing more was recorded for this step.</p>
        )}
        <p className="mute" style={{ margin: "16px 0 0" }}>Select any node in the graph to inspect that step.</p>
      </div>
    </section>
  );
}
