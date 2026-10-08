"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useState, type FormEvent } from "react";
import { MORE_PRESETS, PRESETS, type Preset } from "@/lib/usage";

type KeyOption = { id: string; name: string };

type State = { preset: Preset; from: string; to: string; keyId: string };

/** The page address for a view. Everything the server needs to rebuild the page is in it, so views can be shared. */
export function usageHref({ preset, from, to, keyId }: State): string {
  const q = new URLSearchParams({ range: preset });
  if (preset === "custom") {
    q.set("from", from);
    q.set("to", to);
  }
  if (keyId) q.set("key", keyId);
  return `/usage?${q}`;
}

/**
 * The range buttons, a menu of longer and custom ranges, the key filter and the CSV link. The range is shown as
 * selected whichever way it was chosen, and a custom range keeps its two dates in view and editable.
 */
export function UsageControls({ preset, from, to, today, keyId, keys, exportQuery }: State & { today: string; keys: KeyOption[]; exportQuery: string }) {
  const router = useRouter();
  const inMore = MORE_PRESETS.some((p) => p.value === preset);
  const [customOpen, setCustomOpen] = useState(preset === "custom");
  const [draft, setDraft] = useState({ from, to });
  const draftBad = !draft.from || !draft.to ? "Choose a first and a last day." : draft.from > draft.to ? "The first day is after the last day." : draft.to > today ? "The last day cannot be in the future." : "";

  function applyCustom(e: FormEvent) {
    e.preventDefault();
    if (draftBad) return;
    router.push(usageHref({ preset: "custom", from: draft.from, to: draft.to, keyId }), { scroll: false });
  }

  return (
    <div className="ucontrols">
      <div className="seg" role="group" aria-label="Date range">
        {PRESETS.map((p) => (
          <Link key={p.value} href={usageHref({ preset: p.value, from, to, keyId })} className={preset === p.value ? "on" : undefined} aria-current={preset === p.value ? "true" : undefined} scroll={false}>
            {p.label}
          </Link>
        ))}
      </div>
      <div>
        <label className="sr-only" htmlFor="usage-more">More ranges</label>
        <select id="usage-more" className={`inp sel${inMore ? " sel--on" : ""}`} value={inMore ? preset : ""}
          onChange={(e) => {
            const v = e.target.value as Preset;
            if (v === "custom") setCustomOpen(true);
            else if (v) router.push(usageHref({ preset: v, from, to, keyId }), { scroll: false });
          }}>
          <option value="" disabled>More ranges</option>
          {MORE_PRESETS.map((p) => <option key={p.value} value={p.value}>{p.label}</option>)}
        </select>
      </div>
      <div>
        <label className="sr-only" htmlFor="usage-key">API key</label>
        <select id="usage-key" className="inp sel" value={keyId} onChange={(e) => router.push(usageHref({ preset, from, to, keyId: e.target.value }), { scroll: false })}>
          <option value="">All API keys</option>
          {keys.map((k) => <option key={k.id} value={k.id}>{k.name}</option>)}
        </select>
      </div>
      <a className="btn" href={`/api/usage/export?${exportQuery}`} download>
        Export CSV
      </a>
      {customOpen && (
        <form className="ucustom" onSubmit={applyCustom} aria-label="Custom range">
          <div>
            <label className="lab" htmlFor="usage-from">First day</label>
            <input id="usage-from" className="inp" type="date" max={today} value={draft.from} onChange={(e) => setDraft({ ...draft, from: e.target.value })} />
          </div>
          <div>
            <label className="lab" htmlFor="usage-to">Last day</label>
            <input id="usage-to" className="inp" type="date" max={today} value={draft.to} onChange={(e) => setDraft({ ...draft, to: e.target.value })} />
          </div>
          <button type="submit" className="btn pri" disabled={!!draftBad}>Apply</button>
          {draftBad && <p className="field-err ucustom__err" role="alert">{draftBad}</p>}
          <p className="small ucustom__hint">Days are UTC, up to 366 at a time.</p>
        </form>
      )}
    </div>
  );
}
