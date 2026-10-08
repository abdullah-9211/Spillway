/**
 * A run status: a neutral pill holding a coloured shape and a word. Never colour alone. The labels and
 * 12 by 12 icon paths come from docs/design/tokens.json ("status").
 */
export type RunStatus = "succeeded" | "running" | "waiting" | "sleeping" | "failed" | "cancelled" | "queued";

const STATUS: Record<RunStatus, { label: string; color: string; icon: string }> = {
  succeeded: { label: "Succeeded", color: "var(--ok)", icon: "M2.5 6.5l2.2 2.2L9.5 3.5" },
  running: { label: "Running", color: "var(--run)", icon: "M6 1.5a4.5 4.5 0 1 0 4.5 4.5" },
  waiting: { label: "Waiting for approval", color: "var(--wait)", icon: "M4 2.5v7M8 2.5v7" },
  sleeping: { label: "Sleeping", color: "var(--sleep)", icon: "M9.5 7A4 4 0 0 1 5 2.5a4 4 0 1 0 4.5 4.5z" },
  failed: { label: "Failed", color: "var(--fail)", icon: "M3 3l6 6M9 3l-6 6" },
  cancelled: { label: "Cancelled", color: "var(--sleep)", icon: "M2.5 6h7" },
  queued: { label: "Queued", color: "var(--sleep)", icon: "M6 3v3l2 1.2" },
};

export const statusLabel = (s: RunStatus) => STATUS[s].label;

export function StatusBadge({ status }: { status: RunStatus }) {
  const s = STATUS[status];
  return (
    <span className={`tag status-${status}`} style={{ ["--c" as string]: s.color }}>
      <svg width="12" height="12" viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
        <path d={s.icon} />
      </svg>
      {s.label}
    </span>
  );
}
