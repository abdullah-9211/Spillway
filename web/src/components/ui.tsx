import type { ButtonHTMLAttributes, InputHTMLAttributes, ReactNode } from "react";

export function Panel({ children, className = "", ...rest }: { children: ReactNode; className?: string } & React.HTMLAttributes<HTMLDivElement>) {
  return (
    <div className={`panel ${className}`.trim()} {...rest}>
      {children}
    </div>
  );
}

/** A big number with a label. Numbers use Inter with tabular figures, never the mono face. */
export function StatTile({ label, value, tone, icon, hint }: { label: string; value: ReactNode; tone?: "ok" | "run" | "wait" | "fail"; icon?: ReactNode; hint?: string }) {
  return (
    <div className={`stat ${icon ? "stat--icon" : ""}`.trim()} style={tone ? { ["--c" as string]: `var(--${tone})` } : undefined}>
      <div className="stat__label">
        {icon}
        {label}
      </div>
      <div className="stat__value">{value}</div>
      {hint && <div className="stat__hint">{hint}</div>}
    </div>
  );
}

export function Button({ variant = "default", size = "md", className = "", ...rest }: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: "default" | "primary"; size?: "md" | "sm" }) {
  const cls = ["btn", variant === "primary" ? "pri" : "", size === "sm" ? "sm" : "", className].filter(Boolean).join(" ");
  return <button className={cls} {...rest} />;
}

export function Field({ label, id, ...input }: { label: string; id: string } & InputHTMLAttributes<HTMLInputElement>) {
  return (
    <div>
      <label className="lab" htmlFor={id}>
        {label}
      </label>
      <input id={id} className="inp" {...input} />
    </div>
  );
}

/**
 * A checkbox with its explanation, drawn as a selectable card: the whole card is the click target, a checked one
 * is tinted and outlined in the accent, and the box itself is custom-drawn so it matches both themes.
 */
export function Checkbox({ label, hint, className = "", ...input }: { label: string; hint?: string } & Omit<InputHTMLAttributes<HTMLInputElement>, "type">) {
  return (
    <label className={`cb ${className}`.trim()}>
      <input type="checkbox" {...input} />
      <span className="cb__text">
        <span className="cb__label">{label}</span>
        {hint && <span className="cb__hint">{hint}</span>}
      </span>
    </label>
  );
}
