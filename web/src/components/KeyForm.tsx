"use client";

import { useState, type FormEvent } from "react";
import { emptyForm, validateKeyForm, type KeyFormValues } from "@/lib/format";
import { Button, Checkbox } from "./ui";

function Err({ id, text }: { id: string; text?: string }) {
  return text ? (
    <p id={id} className="field-err">
      {text}
    </p>
  ) : null;
}

/** The create and edit form. Empty limits mean "no limit". The server checks everything again. */
export function KeyForm({
  initial = emptyForm,
  submitLabel,
  pending,
  error,
  onSubmit,
  onCancel,
  layout = "stacked",
  autoFocus = false,
}: {
  initial?: KeyFormValues;
  submitLabel: string;
  pending: boolean;
  error: string | null;
  onSubmit: (v: KeyFormValues) => void;
  onCancel?: () => void;
  /** inline lays the fields out across the full width of a table row */
  layout?: "stacked" | "inline";
  autoFocus?: boolean;
}) {
  const [v, setV] = useState<KeyFormValues>(initial);
  const [shown, setShown] = useState<ReturnType<typeof validateKeyForm>>({});

  function submit(e: FormEvent) {
    e.preventDefault();
    const errs = validateKeyForm(v);
    setShown(errs);
    if (Object.keys(errs).length === 0) onSubmit(v);
  }

  return (
    <form onSubmit={submit} className={`kform${layout === "inline" ? " kform--inline" : ""}`} noValidate>
      <div className="kform__fields">
        <div className="kform__name">
          <label className="lab" htmlFor="key-name">Name</label>
          <input id="key-name" autoFocus={autoFocus} className="inp" type="text" placeholder="For example, ci-pipeline" value={v.name}
            onChange={(e) => setV({ ...v, name: e.target.value })} aria-invalid={!!shown.name} aria-describedby={shown.name ? "key-name-err" : undefined} />
          <Err id="key-name-err" text={shown.name} />
        </div>
        <div>
          <label className="lab" htmlFor="key-rpm">Requests a minute</label>
          <input id="key-rpm" className="inp num" type="text" inputMode="numeric" placeholder="No limit" value={v.rpm}
            onChange={(e) => setV({ ...v, rpm: e.target.value })} aria-invalid={!!shown.rpm} aria-describedby={shown.rpm ? "key-rpm-err" : undefined} />
          <Err id="key-rpm-err" text={shown.rpm} />
        </div>
        <div>
          <label className="lab" htmlFor="key-budget">Monthly budget (USD)</label>
          <input id="key-budget" className="inp num" type="text" inputMode="decimal" placeholder="No limit" value={v.budget}
            onChange={(e) => setV({ ...v, budget: e.target.value })} aria-invalid={!!shown.budget} aria-describedby={shown.budget ? "key-budget-err" : undefined} />
          <Err id="key-budget-err" text={shown.budget} />
        </div>
      </div>
      <div className="kform__options">
        <Checkbox label="Use the semantic cache" hint="Returns saved answers for near-identical questions." checked={v.semanticCache}
          onChange={(e) => setV({ ...v, semanticCache: e.target.checked })} />
        <Checkbox label="Cache answers above temperature 0" hint="Off by default, because those answers can vary." checked={v.cacheNonzeroTemp}
          onChange={(e) => setV({ ...v, cacheNonzeroTemp: e.target.checked })} />
      </div>
      {error && (
        <div className="err" role="alert">
          <span>{error}</span>
        </div>
      )}
      <div className="kform__actions">
        <Button type="submit" variant="primary" disabled={pending}>
          {pending ? "Saving…" : submitLabel}
        </Button>
        {onCancel && (
          <Button type="button" onClick={onCancel} disabled={pending}>
            Cancel
          </Button>
        )}
      </div>
    </form>
  );
}
