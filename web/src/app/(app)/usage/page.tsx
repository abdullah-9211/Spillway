import type { Metadata } from "next";
import { ComingSoon } from "@/components/ComingSoon";

export const metadata: Metadata = { title: "Usage and cost" };

export default function UsagePage() {
  return <ComingSoon title="Usage and cost" phase={6}>Requests, tokens, cost, latency and cache hit rate by key, model and day.</ComingSoon>;
}
