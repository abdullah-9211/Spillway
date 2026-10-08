"use client";

import { countByStatus, defaultFilters, isFiltered, type ApiKey, type BudgetFilter, type KeyFilters, type SortKey, type StatusFilter } from "@/lib/format";

const STATUS: { value: StatusFilter; label: string }[] = [
  { value: "active", label: "Active" },
  { value: "revoked", label: "Revoked" },
  { value: "all", label: "All" },
];

const BUDGET: { value: BudgetFilter; label: string }[] = [
  { value: "any", label: "Any budget" },
  { value: "attention", label: "Needs attention" },
  { value: "over", label: "Over budget" },
  { value: "none", label: "No budget limit" },
];

const SORT: { value: SortKey; label: string }[] = [
  { value: "newest", label: "Newest first" },
  { value: "name", label: "Name, A to Z" },
  { value: "spend", label: "Highest spend" },
  { value: "recent", label: "Recently used" },
];

/** Search, filter and sort for the key list. Everything happens in the browser on the list the page already has. */
export function KeyToolbar({ all, filters, onChange, shown }: { all: ApiKey[]; filters: KeyFilters; onChange: (f: KeyFilters) => void; shown: number }) {
  const counts = countByStatus(all);
  const set = (patch: Partial<KeyFilters>) => onChange({ ...filters, ...patch });
  return (
    <div className="ktool">
      <div className="ktool__row">
        <div className="ktool__search">
          <label className="sr-only" htmlFor="key-search">Search keys</label>
          <svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" aria-hidden="true">
            <circle cx="7" cy="7" r="4.5" />
            <path d="M10.5 10.5L14 14" />
          </svg>
          <input id="key-search" className="inp" type="search" placeholder="Search by name or key prefix" value={filters.query} onChange={(e) => set({ query: e.target.value })} />
        </div>
        <div className="seg" role="group" aria-label="Show keys">
          {STATUS.map((s) => (
            <button key={s.value} type="button" className={filters.status === s.value ? "on" : undefined} aria-pressed={filters.status === s.value} onClick={() => set({ status: s.value })}>
              {s.label} <span className="num">{counts[s.value]}</span>
            </button>
          ))}
        </div>
      </div>
      <div className="ktool__row">
        <div>
          <label className="sr-only" htmlFor="key-budget-filter">Filter by budget</label>
          <select id="key-budget-filter" className="inp sel" value={filters.budget} onChange={(e) => set({ budget: e.target.value as BudgetFilter })}>
            {BUDGET.map((b) => <option key={b.value} value={b.value}>{b.label}</option>)}
          </select>
        </div>
        <div>
          <label className="sr-only" htmlFor="key-sort">Sort keys</label>
          <select id="key-sort" className="inp sel" value={filters.sort} onChange={(e) => set({ sort: e.target.value as SortKey })}>
            {SORT.map((s) => <option key={s.value} value={s.value}>{s.label}</option>)}
          </select>
        </div>
        <p className="ktool__count" role="status">
          {shown} of {all.length} {all.length === 1 ? "key" : "keys"}
        </p>
        {isFiltered(filters) && (
          <button type="button" className="link-btn" onClick={() => onChange({ ...defaultFilters, sort: filters.sort })}>
            Clear filters
          </button>
        )}
      </div>
    </div>
  );
}
