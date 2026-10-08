"use client";

import { useRouter } from "next/navigation";
import { useEffect, useRef, useState } from "react";
import { formFromKey, keyPayload, type ApiKey, type KeyFormValues } from "@/lib/format";
import { useRole } from "@/lib/role";
import { KeyForm } from "./KeyForm";
import { KeyReveal } from "./KeyReveal";
import { KeyTable } from "./KeyTable";
import { Button } from "./ui";

type Panel =
  | { kind: "none" }
  | { kind: "create" }
  | { kind: "created"; key: ApiKey; secret: string }
  | { kind: "edit"; key: ApiKey }
  | { kind: "revoke"; key: ApiKey };

class RequestError extends Error {
  constructor(message: string, readonly status: number) {
    super(message);
  }
}

async function call<T>(method: string, url: string, body?: unknown): Promise<T> {
  let res: Response;
  try {
    res = await fetch(url, { method, headers: body ? { "Content-Type": "application/json" } : undefined, body: body ? JSON.stringify(body) : undefined });
  } catch {
    throw new RequestError("Could not reach the dashboard. Check your connection and try again.", 0);
  }
  const data = (await res.json().catch(() => ({}))) as T & { message?: string };
  if (!res.ok) throw new RequestError(data.message ?? "Something went wrong. Try again.", res.status);
  return data;
}

export function KeysView({ keys }: { keys: ApiKey[] }) {
  const role = useRole();
  const router = useRouter();
  const isAdmin = role === "admin";
  const [panel, setPanel] = useState<Panel>({ kind: "none" });
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const panelRef = useRef<HTMLElement>(null);

  function open(p: Panel) {
    setError(null);
    setPanel(p);
  }

  // When a different panel opens, move focus into it so keyboard and screen reader users land where the action is.
  // Keyed on the panel's kind only, so typing in the form never pulls focus back.
  useEffect(() => {
    if (panel.kind !== "none") panelRef.current?.focus();
  }, [panel.kind]);

  async function run<T>(fn: () => Promise<T>): Promise<T | null> {
    setPending(true);
    setError(null);
    try {
      return await fn();
    } catch (e) {
      if (e instanceof RequestError && e.status === 401) {
        window.location.replace("/api/auth/logout"); // the session ended
        return null;
      }
      setError(e instanceof Error ? e.message : "Something went wrong.");
      return null;
    } finally {
      setPending(false);
    }
  }

  async function create(v: KeyFormValues) {
    const out = await run(() => call<{ key: ApiKey; secret: string }>("POST", "/api/keys", keyPayload(v)));
    if (out) {
      open({ kind: "created", key: out.key, secret: out.secret });
      router.refresh();
    }
  }

  async function save(k: ApiKey, v: KeyFormValues) {
    const out = await run(() => call<ApiKey>("PATCH", `/api/keys/${k.id}`, keyPayload(v)));
    if (out) {
      open({ kind: "none" });
      router.refresh();
    }
  }

  async function revoke(k: ApiKey) {
    const out = await run(() => call<ApiKey>("DELETE", `/api/keys/${k.id}`));
    if (out) {
      open({ kind: "none" });
      router.refresh();
    }
  }

  return (
    <>
      <div className="head head--end">
        <div>
          <h1 className="h1">API keys</h1>
          <p className="sub">Keys that applications use to call the gateway, with their limits and monthly spend.</p>
        </div>
        <Button variant="primary" disabled={!isAdmin} onClick={() => open({ kind: "create" })} title={isAdmin ? undefined : "Only admins can create keys."}>
          Create key
        </Button>
      </div>

      {!isAdmin && (
        <div className="lock">
          <svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" style={{ flex: "none", marginTop: 2 }}>
            <rect x="3" y="7" width="10" height="7" rx="1.5" />
            <path d="M5.5 7V5a2.5 2.5 0 0 1 5 0v2" />
          </svg>
          <span>You are signed in as a viewer. You can see limits and spend, but only an admin can create, change or revoke keys. Full key values are never shown.</span>
        </div>
      )}

      <div className="cols">
        <section className="panel tbl scroll" aria-label="Keys">
          {keys.length === 0 ? (
            <p className="empty">No keys yet. {isAdmin ? "Create one to let an application call the gateway." : "An admin can create one."}</p>
          ) : (
            <KeyTable keys={keys} canEdit={isAdmin} onEdit={(k) => open({ kind: "edit", key: k })} onRevoke={(k) => open({ kind: "revoke", key: k })} />
          )}
        </section>

        {isAdmin && panel.kind !== "none" && (
          <aside className="panel dr" aria-label="Key details" ref={panelRef} tabIndex={-1}>
            {panel.kind === "create" && (
              <>
                <h2 className="ttl">New key</h2>
                <KeyForm submitLabel="Create key" pending={pending} error={error} onSubmit={create} onCancel={() => open({ kind: "none" })} />
              </>
            )}
            {panel.kind === "edit" && (
              <>
                <h2 className="ttl">Edit {panel.key.name}</h2>
                <KeyForm key={panel.key.id} initial={formFromKey(panel.key)} submitLabel="Save changes" pending={pending} error={error}
                  onSubmit={(v) => save(panel.key, v)} onCancel={() => open({ kind: "none" })} />
              </>
            )}
            {panel.kind === "created" && (
              <KeyReveal keyInfo={panel.key} secret={panel.secret} onDone={() => open({ kind: "none" })} onAnother={() => open({ kind: "create" })} />
            )}
            {panel.kind === "revoke" && (
              <div className="kreveal">
                <h2 className="ttl">Revoke {panel.key.name}?</h2>
                <p className="mute">Applications using this key will be refused from the next request. This cannot be undone. Create a new key to replace it.</p>
                {error && (
                  <div className="err" role="alert">
                    <span>{error}</span>
                  </div>
                )}
                <div className="kform__actions">
                  <Button className="danger" onClick={() => revoke(panel.key)} disabled={pending}>
                    {pending ? "Revoking…" : "Revoke key"}
                  </Button>
                  <Button onClick={() => open({ kind: "none" })} disabled={pending}>
                    Keep key
                  </Button>
                </div>
              </div>
            )}
          </aside>
        )}
      </div>
    </>
  );
}
