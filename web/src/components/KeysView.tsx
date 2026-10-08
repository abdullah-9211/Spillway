"use client";

import { useRouter } from "next/navigation";
import { useEffect, useRef, useState } from "react";
import { defaultFilters, filterKeys, formFromKey, isFiltered, keyPayload, paginate, type ApiKey, type KeyFilters, type KeyFormValues } from "@/lib/format";
import { useRole } from "@/lib/role";
import { KeyForm } from "./KeyForm";
import { KeyReveal } from "./KeyReveal";
import { KeyTable } from "./KeyTable";
import { KeyToolbar } from "./KeyToolbar";
import { Button } from "./ui";

// Creating happens in the side panel; editing and revoking happen in place, under the key's own row.
type Panel =
  | { kind: "none" }
  | { kind: "create" }
  | { kind: "created"; key: ApiKey; secret: string };

type Inline = { kind: "edit"; key: ApiKey } | { kind: "revoke"; key: ApiKey };

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
  const [inline, setInline] = useState<Inline | null>(null);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [filters, setFilters] = useState<KeyFilters>(defaultFilters);
  const [page, setPage] = useState(1);
  const panelRef = useRef<HTMLElement>(null);
  const inlineRef = useRef<HTMLDivElement>(null);

  const matching = filterKeys(keys, filters);
  const view = paginate(matching, page);

  function open(p: Panel) {
    setError(null);
    setNotice(null);
    setInline(null);
    setPanel(p);
  }

  function openInline(i: Inline | null) {
    setError(null);
    setNotice(null);
    setPanel({ kind: "none" });
    setInline(i);
  }

  function changeFilters(f: KeyFilters) {
    setFilters(f);
    setPage(1);
    setInline(null);
  }

  // When a different panel opens, move focus into it so keyboard and screen reader users land where the action is.
  // Keyed on the panel's kind only, so typing in the form never pulls focus back.
  useEffect(() => {
    if (panel.kind !== "none") panelRef.current?.focus();
  }, [panel.kind]);

  // The in-place editor scrolls just enough to be seen, so the key being changed stays where it was.
  const inlineId = inline ? `${inline.kind}:${inline.key.id}` : "";
  useEffect(() => {
    if (inlineId) inlineRef.current?.scrollIntoView({ block: "nearest", behavior: "smooth" });
  }, [inlineId]);

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
      openInline(null);
      setNotice(`Saved changes to ${out.name}.`);
      router.refresh();
    }
  }

  async function revoke(k: ApiKey) {
    const out = await run(() => call<ApiKey>("DELETE", `/api/keys/${k.id}`));
    if (out) {
      openInline(null);
      setNotice(`Revoked ${out.name}. Requests using it are refused from now on.`);
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
        <section className="panel tbl" aria-label="Keys">
          {keys.length > 0 && <KeyToolbar all={keys} filters={filters} onChange={changeFilters} shown={matching.length} />}
          {notice && (
            <p className="notice" role="status">
              {notice}
            </p>
          )}
          {keys.length === 0 ? (
            <p className="empty">No keys yet. {isAdmin ? "Create one to let an application call the gateway." : "An admin can create one."}</p>
          ) : matching.length === 0 ? (
            <p className="empty">
              No keys match{isFiltered(filters) ? " these filters" : ""}.{" "}
              {isFiltered(filters) && (
                <button type="button" className="link-btn" onClick={() => changeFilters({ ...defaultFilters, sort: filters.sort })}>
                  Clear filters
                </button>
              )}
            </p>
          ) : (
            <>
              <div className="scroll">
                <KeyTable
                  keys={view.items}
                  canEdit={isAdmin}
                  onEdit={(k) => openInline({ kind: "edit", key: k })}
                  onRevoke={(k) => openInline({ kind: "revoke", key: k })}
                  inline={
                    isAdmin && inline
                      ? {
                          id: inline.key.id,
                          node: (
                            <div ref={inlineRef}>
                              {inline.kind === "edit" ? (
                                <>
                                  <h2 className="ttl ttl--sm">Edit {inline.key.name}</h2>
                                  <KeyForm key={inline.key.id} layout="inline" autoFocus initial={formFromKey(inline.key)} submitLabel="Save changes" pending={pending}
                                    error={error} onSubmit={(v) => save(inline.key, v)} onCancel={() => openInline(null)} />
                                </>
                              ) : (
                                <div className="kreveal kreveal--inline">
                                  <h2 className="ttl ttl--sm">Revoke {inline.key.name}?</h2>
                                  <p className="mute">Applications using this key will be refused from the next request. This cannot be undone. Create a new key to replace it.</p>
                                  {error && (
                                    <div className="err" role="alert">
                                      <span>{error}</span>
                                    </div>
                                  )}
                                  <div className="kform__actions">
                                    <Button className="danger" onClick={() => revoke(inline.key)} disabled={pending}>
                                      {pending ? "Revoking…" : "Revoke key"}
                                    </Button>
                                    <Button onClick={() => openInline(null)} disabled={pending}>
                                      Keep key
                                    </Button>
                                  </div>
                                </div>
                              )}
                            </div>
                          ),
                        }
                      : undefined
                  }
                />
              </div>
              {view.pages > 1 && (
                <nav className="kpager" aria-label="Pages of keys">
                  <span className="mute num">
                    Showing {view.from} to {view.to} of {view.total}
                  </span>
                  <span className="kpager__btns">
                    <Button size="sm" disabled={view.page <= 1} onClick={() => { setInline(null); setPage(view.page - 1); }}>
                      Previous
                    </Button>
                    <span className="num mute" aria-current="page">
                      Page {view.page} of {view.pages}
                    </span>
                    <Button size="sm" disabled={view.page >= view.pages} onClick={() => { setInline(null); setPage(view.page + 1); }}>
                      Next
                    </Button>
                  </span>
                </nav>
              )}
            </>
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
            {panel.kind === "created" && (
              <KeyReveal keyInfo={panel.key} secret={panel.secret} onDone={() => open({ kind: "none" })} onAnother={() => open({ kind: "create" })} />
            )}
          </aside>
        )}
      </div>
    </>
  );
}
