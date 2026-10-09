"use client";

import { useEffect, useState } from "react";

/**
 * Shows an answer word by word, the way a model would type it. The full text is always in the page for screen
 * readers, a click or a key press shows the rest at once, and reduced motion skips the effect.
 */
export function Reveal({ text, animate }: { text: string; animate: boolean }) {
  const parts = text.split(/(\s+)/);
  const [n, setN] = useState(() => (animate && !window.matchMedia?.("(prefers-reduced-motion: reduce)").matches ? 0 : parts.length));
  const skip = () => setN(parts.length);

  useEffect(() => {
    if (!animate) return;
    if (n >= parts.length) return;
    const step = Math.max(1, Math.ceil(parts.length / 140));
    const id = window.setInterval(() => setN((k) => (k + step >= parts.length ? (window.clearInterval(id), parts.length) : k + step)), 24);
    return () => window.clearInterval(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps -- the clock starts once per answer; n is read only to skip a finished one
  }, [animate, parts.length]);

  if (n >= parts.length) return <p className="ans">{text}</p>;
  return (
    <p className="ans ans--typing" onClick={skip} title="Click to show the whole answer">
      <span className="sr-only">{text}</span>
      <span aria-hidden="true">{parts.slice(0, n).join("")}</span>
      <span className="caret" aria-hidden="true" />
    </p>
  );
}
