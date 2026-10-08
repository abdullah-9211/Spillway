import type { Metadata } from "next";
import { PlaygroundView } from "@/components/playground/PlaygroundView";
import { adminFetch } from "@/lib/api";
import type { HistoryItem, PlaygroundState } from "@/lib/playground";
import { requireSession } from "@/lib/session";

export const metadata: Metadata = { title: "Playground" };

export default async function PlaygroundPage() {
  const { token } = await requireSession();
  const [state, hist] = await Promise.all([
    adminFetch<PlaygroundState>("/admin/playground", { token }),
    adminFetch<{ requests: HistoryItem[]; next_cursor: string | null }>("/admin/playground/history?limit=20", { token }),
  ]);
  if (!state.ok || !hist.ok) {
    const message = !state.ok ? state.message : !hist.ok ? hist.message : "";
    return (
      <>
        <h1 className="h1">Playground</h1>
        <div className="err" role="alert">
          <span>Could not load the playground: {message}</span>
        </div>
      </>
    );
  }
  return <PlaygroundView state={state.data} history={hist.data.requests} nextCursor={hist.data.next_cursor} />;
}
