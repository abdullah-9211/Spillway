"use client";

import { useEffect, useRef, useState } from "react";
import { tweenValue, type FeedItem, type WorkerCard } from "@/lib/stage";

/** A number that settles to its new value instead of jumping. With reduced motion it simply changes. */
export function AnimatedNumber({ value, format, ms = 600 }: { value: number; format: (n: number) => string; ms?: number }) {
  const [shown, setShown] = useState(value);
  const from = useRef(value);
  useEffect(() => {
    if (window.matchMedia?.("(prefers-reduced-motion: reduce)").matches) {
      from.current = value;
      return;
    }
    const start = performance.now();
    const a = from.current;
    let raf = 0;
    const tick = (now: number) => {
      const t = (now - start) / ms;
      from.current = tweenValue(a, value, t);
      setShown(from.current);
      if (t < 1) raf = requestAnimationFrame(tick);
    };
    raf = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(raf);
  }, [value, ms]);
  const reduced = typeof window !== "undefined" && window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;
  return <>{format(reduced ? value : shown)}</>;
}

const when = (at: number, base: number) => {
  const s = Math.round((at - base) / 1000);
  return s < 60 ? `+${s}s` : `+${Math.floor(s / 60)}m ${String(s % 60).padStart(2, "0")}s`;
};

/** What happened, newest first. Each line has its time since the run began, a shape and a word for its tone. */
export function Feed({ items, startedAt }: { items: FeedItem[]; startedAt: number }) {
  // Lines that happened after this page opened slide in; the history that was already there does not.
  const [openedAt] = useState(() => Date.now());
  const fresh = items.filter((i) => i.at > openedAt).map((i) => i.key);
  const ICON = { ok: "M2.5 6.5l2.2 2.2L9.5 3.5", fail: "M3 3l6 6M9 3l-6 6", run: "M6 1.5a4.5 4.5 0 1 0 4.5 4.5", wait: "M3 6h6" };
  const WORD = { ok: "Done", fail: "Problem", run: "Working", wait: "Started" };
  return (
    <ol className="feed" aria-label="What has happened">
      {items.slice(0, 40).map((i) => (
        <li key={i.key} className={`feed__i ${i.tone} ${fresh.includes(i.key) ? "new" : ""}`.trim()}>
          <svg width="12" height="12" viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
            <path d={ICON[i.tone]} />
          </svg>
          <span className="sr-only">{WORD[i.tone]}: </span>
          <span className="feed__t">{i.text}</span>
          <span className="feed__w num">{when(i.at, startedAt)}</span>
        </li>
      ))}
    </ol>
  );
}

/** The workers that have touched the run, with who holds it now. A lost worker is dimmed and says so. */
export function Workers({ cards }: { cards: WorkerCard[] }) {
  if (cards.length === 0) return <p className="mute">No worker has picked this run up yet.</p>;
  return (
    <ul className="workers">
      {cards.map((c) => (
        <li key={c.id} className={`worker ${c.state}`}>
          <span className="worker__dot" aria-hidden="true" />
          <span className="mono worker__id">{c.id}</span>
          <span className="worker__state">{c.state === "holding" ? "Holding the run" : c.state === "parked" ? "Released" : c.state === "stopped" ? "Lost" : "Done"}</span>
          <span className="worker__note">{c.note}{c.epochs.length > 0 ? `, epoch ${c.epochs.join(", ")}` : ""}</span>
        </li>
      ))}
    </ul>
  );
}
