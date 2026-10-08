"use client";

import { createContext, useContext, type ReactNode } from "react";
import type { Role } from "./api";

const RoleContext = createContext<Role | null>(null);

export function RoleProvider({ role, children }: { role: Role; children: ReactNode }) {
  return <RoleContext.Provider value={role}>{children}</RoleContext.Provider>;
}

/** The signed-in user's role. The API enforces roles regardless; this only decides what the screen offers. */
export function useRole(): Role {
  const r = useContext(RoleContext);
  if (!r) throw new Error("useRole must be used inside a RoleProvider");
  return r;
}

export function RoleGate({
  allow,
  children,
  reason = "Only admins can do this.",
}: {
  allow: Role;
  children: ReactNode;
  /** Shown to a user who may not use the controls, so a disabled button is never a mystery. */
  reason?: string;
}) {
  const role = useRole();
  if (role === allow || role === "admin") return <>{children}</>;
  return (
    <div className="role-gate" title={reason}>
      {/* A disabled fieldset disables every control inside it, with no per-control work. */}
      <fieldset disabled className="role-gate__fieldset">
        {children}
      </fieldset>
      <p className="role-gate__reason">{reason}</p>
    </div>
  );
}
