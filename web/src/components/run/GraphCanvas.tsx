"use client";

import { useEffect, useRef, useState } from "react";
import type { Layout, LayoutNode } from "@/lib/graph";

const ZOOMS = [0.6, 0.75, 0.9, 1, 1.15, 1.3, 1.5];

/**
 * The run graph, drawn from a Layout: bands for each worker, a dashed cut where one took over, nodes as buttons and
 * edges as thin elements. It can be zoomed and dragged. The edge into the running step flows, the edges around the
 * selected step light up, and each node carries a bar for how long it took next to the longest step.
 */
export function GraphCanvas({ layout, selected, onSelect }: { layout: Layout; selected: string | null; onSelect: (id: string) => void }) {
  const view = useRef<HTMLDivElement>(null);
  const [zoomIdx, setZoomIdx] = useState(3);
  const [drag, setDrag] = useState<{ x: number; y: number; sl: number; st: number } | null>(null);
  const zoom = ZOOMS[zoomIdx];
  const longest = Math.max(1, ...layout.nodes.map((n) => n.node?.duration_ms ?? 0));

  // the edges that touch the selected node
  const hot = new Set<string>();
  if (selected) {
    const i = layout.nodes.findIndex((n) => n.id === selected);
    layout.edges.forEach((e) => {
      const m = /^e(\d+)/.exec(e.id);
      if (m && (Number(m[1]) === i || Number(m[1]) === i + 1)) hot.add(e.id);
    });
  }

  function fit() {
    const w = view.current?.clientWidth ?? layout.width;
    const want = Math.min(1.5, Math.max(0.6, (w - 24) / layout.width));
    const best = ZOOMS.reduce((b, z, i) => (Math.abs(z - want) < Math.abs(ZOOMS[b] - want) ? i : b), 0);
    setZoomIdx(best);
    if (view.current) view.current.scrollLeft = 0;
  }

  // Bring the selected node into view when it changes.
  useEffect(() => {
    const el = view.current;
    const n = layout.nodes.find((x) => x.id === selected);
    if (!el || !n) return;
    const x = n.x * zoom;
    if (x < el.scrollLeft + 40 || x + n.w * zoom > el.scrollLeft + el.clientWidth - 40) el.scrollTo?.({ left: Math.max(0, x - el.clientWidth / 2), behavior: "smooth" });
  }, [selected, layout.nodes, zoom]);

  function onKey(e: React.KeyboardEvent) {
    if (e.key === "+" || e.key === "=") setZoomIdx((z) => Math.min(ZOOMS.length - 1, z + 1));
    if (e.key === "-") setZoomIdx((z) => Math.max(0, z - 1));
    if (e.key === "0") setZoomIdx(3);
  }

  return (
    <div className="gframe" onKeyDown={onKey}>
      <div className="gtools" role="group" aria-label="Zoom">
        <button type="button" onClick={() => setZoomIdx((z) => Math.max(0, z - 1))} disabled={zoomIdx === 0} aria-label="Zoom out">−</button>
        <span className="num" aria-live="polite">{Math.round(zoom * 100)}%</span>
        <button type="button" onClick={() => setZoomIdx((z) => Math.min(ZOOMS.length - 1, z + 1))} disabled={zoomIdx === ZOOMS.length - 1} aria-label="Zoom in">+</button>
        <button type="button" onClick={fit}>Fit</button>
      </div>
      <div
        ref={view}
        className={`gview ${drag ? "drag" : ""}`}
        onPointerDown={(e) => {
          if ((e.target as HTMLElement).closest("button")) return;
          const el = view.current;
          if (!el) return;
          setDrag({ x: e.clientX, y: e.clientY, sl: el.scrollLeft, st: el.scrollTop });
        }}
        onPointerMove={(e) => {
          if (!drag || !view.current) return;
          view.current.scrollLeft = drag.sl - (e.clientX - drag.x);
          view.current.scrollTop = drag.st - (e.clientY - drag.y);
        }}
        onPointerUp={() => setDrag(null)}
        onPointerLeave={() => setDrag(null)}
      >
        <div style={{ width: layout.width * zoom, height: layout.height * zoom }}>
          <div className="cv" style={{ width: layout.width, height: layout.height, transform: `scale(${zoom})`, transformOrigin: "0 0", position: "relative" }}>
            {layout.bands.map((b) => (
              <div key={b.id} className={`band band--${b.tint}`} style={{ left: b.x, top: 0, width: b.w, height: layout.height }} aria-hidden="true" />
            ))}
            {layout.cuts.map((c) => (
              <div key={c.id}>
                <div className="band cutline" style={{ left: c.x, top: 0, height: layout.height }} aria-hidden="true" />
                <span className="pillc cutpill" style={{ left: c.x, top: 12, transform: "translateX(-50%)" }}>
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

            {layout.edges.map((e, i) => (
              <div
                key={e.id}
                aria-hidden="true"
                className={`ge ge--${e.kind} ${e.dashed ? "ge--dashed" : ""} ${hot.has(e.id) ? "ge--hot" : ""}`.trim()}
                style={{ position: "absolute", left: e.x, top: e.y, width: e.w, height: e.h, animationDelay: `${Math.min(i, 24) * 40}ms` }}
              />
            ))}
            {layout.stubs.map((s) => (
              <div key={s.id} aria-hidden="true" className="gstub" style={{ left: s.x, top: s.y, height: s.h }} />
            ))}
            {layout.chips.map((c) => (
              <div key={c.id} className="chip" style={{ left: c.x, top: c.y, width: c.w, height: c.h }}>
                <svg width="10" height="10" viewBox="0 0 12 12" fill="none" stroke="var(--fail)" strokeWidth="1.8" strokeLinecap="round" aria-hidden="true"><path d="M3 3l6 6M9 3l-6 6" /></svg>
                <span>{c.text}</span>
              </div>
            ))}
            {layout.end && (
              <div className={`gn end ${layout.end.state}`} style={{ left: layout.end.x, top: layout.end.y, width: layout.end.w, height: layout.end.h }} role="note" aria-label={layout.end.tip} title={layout.end.tip}>
                <span className="gt">
                  <svg width="12" height="12" viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" aria-hidden="true"><path d={layout.end.state === "failed" ? "M3 3l6 6M9 3l-6 6" : "M3 6h6"} /></svg>
                </span>
                <span className="gname">{layout.end.label}</span>
                <span className="gm" style={{ ["--c" as string]: "var(--fail)" }}>{layout.end.meta}</span>
              </div>
            )}
            {layout.nodes.map((n, i) => (
              <GraphNodeButton key={n.id} n={n} index={i} longest={longest} selected={selected === n.id} onSelect={onSelect} />
            ))}
          </div>
        </div>
      </div>
      <p className="gtip small">Drag to pan. Zoom with the buttons, or + and − while the graph has focus.</p>
    </div>
  );
}

function GraphNodeButton({ n, index, longest, selected, onSelect }: { n: LayoutNode; index: number; longest: number; selected: boolean; onSelect: (id: string) => void }) {
  const cls = ["gn", n.kind, n.state === "running" ? "run" : "", n.state === "waiting" ? "wait" : "", n.state === "sleeping" ? "sleep" : "", n.state === "stopped" ? "crash" : "", n.reissued ? "redo" : "", n.state === "failed" ? "bad" : "", n.state === "finished" && !n.reissued ? "done" : "", selected ? "sel" : ""].filter(Boolean).join(" ");
  const markColor = n.state === "running" ? "var(--run)" : n.state === "waiting" ? "var(--wait)" : n.state === "sleeping" ? "var(--accent)" : n.state === "stopped" || n.state === "failed" ? "var(--fail)" : n.reissued ? "var(--run)" : n.state === "goal" ? "var(--subtle)" : "var(--ok)";
  const share = n.node && n.node.duration_ms !== null ? Math.max(4, Math.round((n.node.duration_ms / longest) * 100)) : 0;
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
      {n.kind !== "goal" && (
        <span className="gbar" aria-hidden="true">
          <i style={{ width: `${share}%`, background: markColor }} />
        </span>
      )}
    </>
  );
  const style = { left: n.x, top: n.y, width: n.w, height: n.h, animationDelay: `${Math.min(index, 20) * 45}ms` };
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
