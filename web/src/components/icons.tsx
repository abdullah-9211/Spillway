import type { ReactNode } from "react";

/** 16px line icons from the design's sidebar. Decorative: the label next to each carries the meaning. */
function Icon({ children, size = 16, viewBox = "0 0 16 16" }: { children: ReactNode; size?: number; viewBox?: string }) {
  return (
    <svg width={size} height={size} viewBox={viewBox} fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      {children}
    </svg>
  );
}

export const RunsIcon = () => <Icon><path d="M3 4h10M3 8h10M3 12h6" /></Icon>;
export const UsageIcon = () => <Icon><path d="M3 13V7M8 13V3M13 13V9" /></Icon>;
export const KeysIcon = () => (
  <Icon>
    <circle cx="5.5" cy="10.5" r="2.5" />
    <path d="M7.3 8.7L13 3M11 5l1.5 1.5" />
  </Icon>
);
export const PlaygroundIcon = () => <Icon><path d="M5 3l8 5-8 5z" /></Icon>;
export const SunIcon = () => (
  <Icon>
    <circle cx="8" cy="8" r="3" />
    <path d="M8 1.5v1.5M8 13v1.5M1.5 8H3M13 8h1.5M3.4 3.4l1 1M11.6 11.6l1 1M12.6 3.4l-1 1M4.4 11.6l-1 1" />
  </Icon>
);
export const MoonIcon = () => <Icon><path d="M13 9.5A5.5 5.5 0 0 1 6.5 3a5.5 5.5 0 1 0 6.5 6.5z" /></Icon>;

/** The Spillway mark: a step path, in the accent colour. */
export function BrandMark({ size = 22 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="var(--accent)" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M3 6h6v6h6v6h6" />
    </svg>
  );
}
