"use client";

import { useRouter } from "next/navigation";
import { useState, type FormEvent } from "react";
import { Button, Field } from "./ui";

type Failure = { status: number; message: string; retryAfter?: number };

/** The sentence under a failed sign-in. The 401 text is the design's own copy. */
export function failureText(f: Failure): string {
  if (f.status === 401) {
    return "That username and password do not match. After 5 failed tries in a minute, sign in is paused for a minute.";
  }
  if (f.status === 429) {
    const wait = f.retryAfter ? ` Try again in ${f.retryAfter} second${f.retryAfter === 1 ? "" : "s"}.` : "";
    return `Too many failed tries, so sign in is paused.${wait}`;
  }
  return f.message || "Could not sign in. Try again.";
}

export function LoginForm() {
  const router = useRouter();
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState(false);

  async function onSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const form = new FormData(e.currentTarget);
    setPending(true);
    setError(null);
    try {
      const res = await fetch("/api/auth/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username: form.get("username"), password: form.get("password") }),
      });
      if (res.ok) {
        router.replace("/");
        router.refresh();
        return;
      }
      const body = (await res.json().catch(() => ({}))) as { message?: string };
      const retry = Number(res.headers.get("Retry-After")) || undefined;
      setError(failureText({ status: res.status, message: body.message ?? "", retryAfter: retry }));
    } catch {
      setError("Could not reach the Spillway dashboard. Check your connection and try again.");
    } finally {
      setPending(false);
    }
  }

  return (
    <>
      <form onSubmit={onSubmit} className="login__form" noValidate>
        <Field label="Username" id="username" name="username" type="text" autoComplete="username" placeholder="admin" required />
        <Field label="Password" id="password" name="password" type="password" autoComplete="current-password" placeholder="Your password" required />
        <Button type="submit" variant="primary" disabled={pending} className="login__submit">
          {pending ? "Signing in…" : "Sign in"}
        </Button>
      </form>
      {error && (
        <div className="err" role="alert">
          <svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="var(--fail)" strokeWidth="1.5" strokeLinecap="round" aria-hidden="true" style={{ flex: "none", marginTop: 2 }}>
            <circle cx="8" cy="8" r="6" />
            <path d="M8 5v3.5M8 11v.01" />
          </svg>
          <span>{error}</span>
        </div>
      )}
    </>
  );
}
