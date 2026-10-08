"use client";

import { useState } from "react";
import { rpmLabel, usd, type ApiKey } from "@/lib/format";
import { Button } from "./ui";

/** Shown once, straight after creation. Spillway keeps only a hash, so closing this loses the key for good. */
export function KeyReveal({
  keyInfo,
  secret,
  onDone,
  onAnother,
}: {
  keyInfo: ApiKey;
  secret: string;
  onDone: () => void;
  onAnother: () => void;
}) {
  const [copied, setCopied] = useState<"yes" | "no" | null>(null);

  async function copy() {
    try {
      await navigator.clipboard.writeText(secret);
      setCopied("yes");
    } catch {
      setCopied("no"); // clipboard blocked: the key is selectable below
    }
  }

  return (
    <div className="kreveal kreveal--wide">
      <div className="kreveal__main">
        <div>
          <h2 className="ttl">Key created</h2>
          <p className="mute kreveal__sub">{keyInfo.name} is ready to use.</p>
        </div>
        <div className="keybox">
          <span className="small">Your new key</span>
          <code data-testid="secret">{secret}</code>
          <Button size="sm" onClick={copy} className="keybox__copy">
            Copy key
          </Button>
          <span role="status" className="small">
            {copied === "yes" && "Copied to the clipboard."}
            {copied === "no" &&
              "Could not copy automatically. Select the key above and copy it."}
          </span>
        </div>
        <div className="warnbox">
          <svg
            width="16"
            height="16"
            viewBox="0 0 16 16"
            fill="none"
            stroke="var(--wait)"
            strokeWidth="1.5"
            strokeLinecap="round"
            aria-hidden="true"
            style={{ flex: "none", marginTop: 2 }}
          >
            <path d="M8 4v5M8 12v.01" />
          </svg>
          <span>
            Copy it now. For security, Spillway stores only a hash and cannot
            show this key again. If you lose it, revoke it and create a new one.
          </span>
        </div>
      </div>
      <div className="kreveal__side">
        <dl className="dl">
          <dt>Rate limit</dt>
          <dd className="num">
            {keyInfo.rate_limit_rpm == null
              ? "No limit"
              : rpmLabel(keyInfo.rate_limit_rpm).replace("a min", "per minute")}
          </dd>
          <dt>Monthly budget</dt>
          <dd className="num">
            {keyInfo.monthly_budget_usd == null
              ? "No limit"
              : usd(keyInfo.monthly_budget_usd)}
          </dd>
          <dt>Semantic cache</dt>
          <dd>{keyInfo.semantic_cache ? "On" : "Off"}</dd>
          <dt>Cache above temperature 0</dt>
          <dd>{keyInfo.cache_nonzero_temp ? "On" : "Off"}</dd>
        </dl>
        <div className="kform__actions">
          <Button variant="primary" onClick={onDone}>
            Done
          </Button>
          <Button onClick={onAnother}>Create another</Button>
        </div>
      </div>
    </div>
  );
}
