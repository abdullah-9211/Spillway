"use client";

import type { Layout, LayoutNode } from "@/lib/graph";

const edgeColor = { done: "var(--subtle)", run: "var(--run)", redo: "var(--run)" } as const;

/**
 * The run graph, drawn from a Layout: bands for each worker, a dashed cut where one took over, nodes as buttons and
 * edges as thin elements. Plain absolutely-positioned boxes, so every node is a real focusable control.
 */
export function GraphCanvas({ layout, selected, onSelect }: { layout: Layout; selected: string | null; onSelect: (id: string) => void }) {
  return (
    <div className="scroll">
      <div className="cv" style={{ width: layout.width, height: layout.height }}>
        {layout.bands.map((b) => (
          <div key={b.id} className="band" style={{ left: b.x, top: 0, width: b.w, height: layout.height, background: b.tint === "base" ? "color-mix(in srgb, var(--raised) 55%, transparent)" : "color-mix(in srgb, var(--run) 5%, transparent)" }} aria-hidden="true" />
        ))}
        {layout.cuts.map((c) => (
          <div key={c.id}>
            <div className="band" style={{ left: c.x, top: 0, width: 0, height: layout.height, borderLeft: "1.5px dashed var(--line2)" }} aria-hidden="true" />
            <span className="pillc" style={{ left: c.x, top: 12, transform: "translateX(-50%)" }}>
              <svg width="12" height="12" viewBox="0 0 12 12" fill="none" stroke="var(--fail)" strokeWidth="1.6" strokeLinecap="round" aria-hidden="true"><path d="M3 3l6 6M9 3l-6 6" /></svg>
              {c.label}
            </span>
          </div>
        ))}
        <div className="lbl" style={{ left: 12, top: 150 }} aria-hidden="true">Model</div>
        <div className="lbl" style={{ left: 12, top: 300 }} aria-hidden="true">Tools</div>
        {layout.bands.map((b) => (
          <div key={`l${b.id}`} className="lbl" style={{ left: b.labelX, top: 372 }} aria-hidden="true">{b.label}</div>
        ))}

        {layout.edges.map((e) => (
          <div key={e.id} aria-hidden="true" style={{ position: "absolute", left: e.x, top: e.y, width: e.w, height: e.h, ...(e.dashed ? { borderTop: `2px dashed ${edgeColor.redo}` } : { background: edgeColor[e.kind] }) }} />
        ))}
        {layout.stubs.map((s) => (
          <div key={s.id} aria-hidden="true" style={{ position: "absolute", left: s.x, top: s.y, width: 0, height: s.h, borderLeft: "2px dashed var(--fail)" }} />
        ))}
        {layout.chips.map((c) => (
          <div key={c.id} className="chip" style={{ left: c.x, top: c.y, width: c.w, height: c.h }}>
            <svg width="10" height="10" viewBox="0 0 12 12" fill="none" stroke="var(--fail)" strokeWidth="1.8" strokeLinecap="round" aria-hidden="true"><path d="M3 3l6 6M9 3l-6 6" /></svg>
            <span>{c.text}</span>
          </div>
        ))}
        {layout.nodes.map((n) => (
          <GraphNodeButton key={n.id} n={n} selected={selected === n.id} onSelect={onSelect} />
        ))}
      </div>
    </div>
  );
}

function GraphNodeButton({ n, selected, onSelect }: { n: LayoutNode; selected: boolean; onSelect: (id: string) => void }) {
  const cls = ["gn", n.kind, n.state === "running" ? "run" : "", n.state === "stopped" ? "crash" : "", n.reissued ? "redo" : "", n.state === "failed" ? "bad" : "", selected ? "sel" : ""].filter(Boolean).join(" ");
  const markColor = n.state === "running" ? "var(--run)" : n.state === "stopped" || n.state === "failed" ? "var(--fail)" : n.reissued ? "var(--run)" : n.state === "goal" ? "var(--subtle)" : "var(--ok)";
  const content = (
    <>
      <span className="gt">
        <svg width="12" height="12" viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d={n.icon} /></svg>
        <span className="gs">{n.step ?? ""}</span>
      </span>
      <span className={`gname ${n.kind === "goal" ? "" : "mono"}`}>{n.label}</span>
      <span className="gm" style={{ ["--c" as string]: markColor }}>
        <svg className={n.state === "running" ? "sp" : ""} width="12" height="12" viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d={n.mark} /></svg>
        {n.meta}
      </span>
    </>
  );
  const style = { left: n.x, top: n.y, width: n.w, height: n.h };
  if (n.kind === "goal") {
    return (
      <div className={cls} style={style} title={n.tip}>
        {content}
      </div>
    );
  }
  return (
    <button type="button" className={cls} style={style} title={n.tip} aria-pressed={selected} aria-label={n.tip} onClick={() => onSelect(n.id)}>
      {content}
    </button>
  );
}
