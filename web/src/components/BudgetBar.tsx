import { budgetState, usd } from "@/lib/format";

const FLAG = {
  close: { text: "Close to the limit", cls: "wait", icon: "M6 2.5v4M6 9v.01" },
  over: { text: "Over budget, requests are refused", cls: "fail", icon: "M3 3l6 6M9 3l-6 6" },
} as const;

/**
 * Spend against a monthly budget: the amounts, a bar, and, when it matters, a flag with a shape and words.
 * The bar's colour is a second cue only. The state is always readable as text.
 */
export function BudgetBar({ spend, budget }: { spend: string; budget: string | null }) {
  const { state, ratio } = budgetState(spend, budget);
  const width = `${Math.min(100, Math.max(0, ratio * 100)).toFixed(0)}%`;
  const flag = state === "close" || state === "over" ? FLAG[state] : null;
  return (
    <div className="bud">
      <div className="bud__t num">
        <span>{usd(spend)}</span>
        <span className="mute">{budget == null ? "no limit" : `of ${usd(budget)}`}</span>
      </div>
      {budget != null && (
        <div className="trk" role="img" aria-label={`${Math.round(ratio * 100)}% of the monthly budget spent`}>
          <i className={state === "close" ? "warn" : state === "over" ? "bad" : undefined} style={{ width }} />
        </div>
      )}
      {flag && (
        <span className={`flag ${flag.cls}`}>
          <svg width="12" height="12" viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" aria-hidden="true">
            <path d={flag.icon} />
          </svg>
          {flag.text}
        </span>
      )}
    </div>
  );
}
