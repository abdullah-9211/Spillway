import type { Metadata } from "next";
import { ComingSoon } from "@/components/ComingSoon";

export const metadata: Metadata = { title: "Playground" };

export default function PlaygroundPage() {
  return <ComingSoon title="Playground" phase={7}>Send a prompt, pick a routing policy, and watch the route it takes.</ComingSoon>;
}
