"use client";

import { forwardRef } from "react";
import { MAX_SEARCH, SORTS, statusChips, type Filters, type RunCounts, type SortKey, type StatusFilter } from "@/lib/runs";

type Props = {
  filters: Filters;
  q: string;
  onQ: (q: string) => void;
  onStatus: (s: StatusFilter) => void;
  onKey: (id: string) => void;
  onSort: (s: SortKey) => void;
  onClear: () => void;
  counts: RunCounts;
  keys: { id: string; name: string }[];
  filtered: boolean;
};

/** Find a run: search its task, narrow by status or API key, and choose the order. */
export const Toolbar = forwardRef<HTMLInputElement, Props>(function Toolbar({ filters, q, onQ, onStatus, onKey, onSort, onClear, counts, keys, filtered }, ref) {
  return (
    <div className="rtool" role="search" aria-label="Find runs">
      <div className="rtool__row">
        <div className="rtool__search">
          <svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" aria-hidden="true">
            <circle cx="7" cy="7" r="4.5" />
            <path d="M10.5 10.5L14 14" />
          </svg>
          <label className="sr-only" htmlFor="runs-search">Search runs by task</label>
          <input
            id="runs-search" ref={ref} className="inp" type="search" value={q} maxLength={MAX_SEARCH} placeholder="Search runs by task" autoComplete="off"
            onChange={(e) => onQ(e.target.value)}
          />
          <kbd className="kbd" aria-hidden="true">/</kbd>
        </div>
        {keys.length > 1 && (
          <div className="rtool__sel">
            <label className="sr-only" htmlFor="runs-key">API key</label>
            <select id="runs-key" className="inp" value={filters.key} onChange={(e) => onKey(e.target.value)}>
              <option value="">All API keys</option>
              {keys.map((k) => (
                <option key={k.id} value={k.id}>{k.name}</option>
              ))}
            </select>
          </div>
        )}
        <div className="rtool__sel">
          <label className="sr-only" htmlFor="runs-sort">Order</label>
          <select id="runs-sort" className="inp" value={filters.sort} onChange={(e) => onSort(e.target.value as SortKey)}>
            {SORTS.map((s) => (
              <option key={s.value} value={s.value}>{s.label}</option>
            ))}
          </select>
        </div>
      </div>
      <div className="chips2" role="group" aria-label="Show runs that are">
        {statusChips(counts).map((c) => (
          <button key={c.id} type="button" className={`chip2 ${filters.status === c.id ? "on" : ""}`.trim()} aria-pressed={filters.status === c.id} onClick={() => onStatus(c.id)}>
            {c.label}
            {c.count !== null && <span className="num">{c.count}</span>}
          </button>
        ))}
        {filtered && (
          <button type="button" className="link-btn" onClick={onClear}>
            Clear filters
          </button>
        )}
      </div>
    </div>
  );
});
