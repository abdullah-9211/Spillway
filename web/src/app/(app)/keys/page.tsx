import type { Metadata } from "next";
import { KeysView } from "@/components/KeysView";
import { adminFetch } from "@/lib/api";
import type { ApiKey } from "@/lib/format";
import { requireSession } from "@/lib/session";

export const metadata: Metadata = { title: "API keys" };

export default async function KeysPage() {
  const { token } = await requireSession();
  const res = await adminFetch<{ keys: ApiKey[] }>("/admin/keys", { token });
  if (!res.ok) {
    return (
      <>
        <h1 className="h1">API keys</h1>
        <div className="err" role="alert">
          <span>Could not load the keys: {res.message}</span>
        </div>
      </>
    );
  }
  return <KeysView keys={res.data.keys} />;
}
