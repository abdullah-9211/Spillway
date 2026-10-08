import type { ReactNode } from "react";
import { dateLabel, relativeTime, rpmLabel, type ApiKey } from "@/lib/format";
import { BudgetBar } from "./BudgetBar";

function sub(k: ApiKey): string {
  if (k.builtin) return "Built in, used by this dashboard";
  if (k.revoked_at) return `Revoked on ${dateLabel(k.revoked_at)}`;
  return `Created ${dateLabel(k.created_at)}`;
}

/** Only the key's first characters are ever shown, so a viewer cannot learn a secret from this table. */
export function KeyTable({
  keys,
  canEdit,
  now,
  onEdit,
  onRevoke,
  inline,
}: {
  keys: ApiKey[];
  canEdit: boolean;
  now?: Date;
  onEdit: (k: ApiKey) => void;
  onRevoke: (k: ApiKey) => void;
  /** An editor shown directly under one key's row, so the key being changed never leaves the screen. */
  inline?: { id: string; node: ReactNode };
}) {
  return (
    <div role="table" aria-label="API keys">
      <div role="row" className="kgrid kgh">
        <div role="columnheader">Name</div>
        <div role="columnheader">Key</div>
        <div role="columnheader">Rate limit</div>
        <div role="columnheader">Spend this month</div>
        <div role="columnheader">Semantic cache</div>
        <div role="columnheader">Last used</div>
        <div role="columnheader" className="kacts">
          {canEdit ? "Actions" : "Read only"}
        </div>
      </div>
      {keys.map((k) => {
        const revoked = k.revoked_at != null;
        const editable = canEdit && !revoked && !k.builtin;
        const open = inline?.id === k.id;
        return (
          <div key={k.id} className={open ? "kgroup kgroup--open" : "kgroup"}>
          <div role="row" className={`kgrid krow${revoked ? " off" : ""}`} data-state={revoked ? "revoked" : "active"}>
            <div role="cell">
              <span className="nm">
                {k.name}
                {revoked && <span className="sr-only"> (revoked)</span>}
              </span>
              <span className="small">{sub(k)}</span>
            </div>
            <div role="cell" className="mono mute">
              {k.prefix}…
            </div>
            <div role="cell" className="num">
              {rpmLabel(k.rate_limit_rpm)}
            </div>
            <div role="cell">
              <BudgetBar spend={k.spend_usd} budget={k.monthly_budget_usd} />
            </div>
            <div role="cell">
              <span className="tag">{k.semantic_cache ? "On" : "Off"}</span>
            </div>
            <div role="cell" className="mute">
              {relativeTime(k.last_used_at, now)}
            </div>
            <div role="cell" className="kacts">
              {editable && (
                <>
                  <button type="button" className="btn sm" onClick={() => onEdit(k)} aria-label={`Edit ${k.name}`}>
                    Edit
                  </button>
                  <button type="button" className="btn sm danger" onClick={() => onRevoke(k)} aria-label={`Revoke ${k.name}`}>
                    Revoke
                  </button>
                </>
              )}
            </div>
          </div>
          {open && (
            <div role="row" className="kinline">
              <div role="cell" className="kinline__cell">
                {inline.node}
              </div>
            </div>
          )}
          </div>
        );
      })}
    </div>
  );
}
