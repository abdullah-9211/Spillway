import type { ReactNode } from "react";
import { Shell } from "@/components/Shell";
import { adminFetch, type Status } from "@/lib/api";
import { RoleProvider } from "@/lib/role";
import { requireSession } from "@/lib/session";

export default async function AppLayout({ children }: { children: ReactNode }) {
  const { token, user } = await requireSession();
  const status = await adminFetch<Status>("/admin/status", { token });
  return (
    <RoleProvider role={user.role}>
      <Shell user={user} status={status.ok ? status.data : null}>
        {children}
      </Shell>
    </RoleProvider>
  );
}
