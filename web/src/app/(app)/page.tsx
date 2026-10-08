import type { Metadata } from "next";
import { Panel, StatTile } from "@/components/ui";
import { adminFetch, type Status } from "@/lib/api";
import { requireSession } from "@/lib/session";

export const metadata: Metadata = { title: "Runs" };

const word = { up: "Connected", down: "Down", disabled: "Off", unknown: "Unknown" } as const;

export default async function RunsPage() {
  const { token, user } = await requireSession();
  const status = await adminFetch<Status>("/admin/status", { token });
  return (
    <>
      <div className="head">
        <h1 className="h1">Runs</h1>
      </div>
      <Panel className="soon">
        <h2>Signed in as {user.username}</h2>
        <p>
          You have the <strong>{user.role}</strong> role.{" "}
          {user.role === "admin" ? "You can change keys and approve runs." : "You can read everything and change nothing."}
        </p>
        <p className="mute">The runs page arrives in phase 10. These are live checks of the service you are signed in to.</p>
      </Panel>
      {status.ok && (
        <Panel className="pulse" aria-label="Service status">
          <div className="stats stats--wide">
            <StatTile label="Postgres" value={word[status.data.postgres]} tone={status.data.postgres === "up" ? "ok" : "fail"} />
            <StatTile label="Redis" value={word[status.data.redis]} tone={status.data.redis === "up" ? "ok" : status.data.redis === "down" ? "fail" : undefined} />
          </div>
        </Panel>
      )}
    </>
  );
}
