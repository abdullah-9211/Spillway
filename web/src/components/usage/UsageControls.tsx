"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { PRESETS, type Preset } from "@/lib/usage";

type KeyOption = { id: string; name: string };

function href(range: Preset, key: string) {
  const q = new URLSearchParams({ range });
  if (key) q.set("key", key);
  return `/usage?${q}`;
}

/** The range buttons, the key filter and the CSV link. State lives in the URL, so a view can be shared and the back button works. */
export function UsageControls({ preset, keyId, keys, exportQuery }: { preset: Preset; keyId: string; keys: KeyOption[]; exportQuery: string }) {
  const router = useRouter();
  return (
    <div className="ucontrols">
      <div className="seg" role="group" aria-label="Date range">
        {PRESETS.map((p) => (
          <Link key={p.value} href={href(p.value, keyId)} className={preset === p.value ? "on" : undefined} aria-current={preset === p.value ? "true" : undefined} scroll={false}>
            {p.label}
          </Link>
        ))}
      </div>
      <div>
        <label className="sr-only" htmlFor="usage-key">API key</label>
        <select id="usage-key" className="inp sel" value={keyId} onChange={(e) => router.push(href(preset, e.target.value), { scroll: false })}>
          <option value="">All API keys</option>
          {keys.map((k) => (
            <option key={k.id} value={k.id}>
              {k.name}
            </option>
          ))}
        </select>
      </div>
      <a className="btn" href={`/api/usage/export?${exportQuery}`} download>
        Export CSV
      </a>
    </div>
  );
}
