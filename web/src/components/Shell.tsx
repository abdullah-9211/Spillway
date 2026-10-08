import type { ReactNode } from "react";
import type { Status, User } from "@/lib/api";
import { BrandMark } from "./icons";
import { NavLinks } from "./NavLinks";
import { UserChip } from "./UserChip";
import { ThemeSwitch } from "./ThemeSwitch";

function Dot({ state }: { state: "ok" | "bad" | "off" }) {
  return <span className={`dot dot--${state}`} aria-hidden="true" />;
}

/** What is connected, from the Go service's own health check. Words as well as a dot: never colour alone. */
export function SystemStatus({ status }: { status: Status | null }) {
  if (!status) {
    return (
      <div className="sys" role="status">
        <Dot state="bad" />
        Cannot reach the Spillway service
      </div>
    );
  }
  const pg = status.postgres === "up";
  const redis = status.redis;
  return (
    <>
      <div className="sys" role="status">
        <Dot state={pg ? "ok" : "bad"} />
        {pg ? "Postgres connected" : "Postgres is down"}
      </div>
      <div className="sys" role="status">
        <Dot state={redis === "up" ? "ok" : redis === "down" ? "bad" : "off"} />
        {redis === "up" ? "Redis connected" : redis === "down" ? "Redis is down" : "Redis is off (no rate limits or cache)"}
      </div>
    </>
  );
}

export function Shell({ user, status, children }: { user: User; status: Status | null; children: ReactNode }) {
  return (
    <div className="app">
      <aside className="side">
        <div className="brand">
          <BrandMark />
          Spillway
        </div>
        <NavLinks />
        <div className="sysbox">
          <SystemStatus status={status} />
          <ThemeSwitch />
          <UserChip user={user} />
        </div>
      </aside>
      <main className="main" id="main">
        {children}
      </main>
    </div>
  );
}
