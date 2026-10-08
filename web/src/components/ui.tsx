import type { ButtonHTMLAttributes, InputHTMLAttributes, ReactNode } from "react";

export function Panel({ children, className = "", ...rest }: { children: ReactNode; className?: string } & React.HTMLAttributes<HTMLDivElement>) {
  return (
    <div className={`panel ${className}`.trim()} {...rest}>
      {children}
    </div>
  );
}

/** A big number with a label. Numbers use Inter with tabular figures, never the mono face. */
export function StatTile({ label, value, tone, icon }: { label: string; value: ReactNode; tone?: "ok" | "run" | "wait" | "fail"; icon?: ReactNode }) {
  return (
    <div className="stat" style={tone ? { ["--c" as string]: `var(--${tone})` } : undefined}>
      <div className="stat__label">
        {icon}
        {label}
      </div>
      <div className="stat__value">{value}</div>
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
