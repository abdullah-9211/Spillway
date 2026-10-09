import type { components } from "./api-types";
import { ms } from "./usage";

export type Policy = components["schemas"]["CatalogPolicy"];
export type Fault = components["schemas"]["Fault"];
export type FaultKind = Fault["kind"];
export type Attempt = components["schemas"]["Attempt"];
export type PlaygroundState = components["schemas"]["PlaygroundState"];
export type PlaygroundResult = components["schemas"]["PlaygroundResult"];
export type HistoryItem = components["schemas"]["PlaygroundHistoryItem"];
export type ChatRequest = components["schemas"]["PlaygroundChatRequest"];

export const MAX_PROMPT = 8000;

export const SUGGESTIONS = ["Summarise this changelog", "Write a SQL query for monthly active users", "Translate this error into plain English"];

const IDEAS = [
  "Write a haiku about a failing API",
  "Tell me a joke about retries",
  "Explain a circuit breaker in two sentences",
  "Write a SQL query for monthly active users",
  "Summarise this changelog in three bullets",
  "Translate this error into plain English: permission denied",
];

/** A random prompt (never the same one twice in a row) and up to two faults on different providers of the route. */
export function surprise(providers: string[], current: string, rand: () => number = Math.random): { prompt: string; faults: Fault[] } {
  const pool = IDEAS.filter((p) => p !== current);
  const prompt = pool[Math.floor(rand() * pool.length)];
  const kinds: FaultKind[] = ["rate_limit", "server_error", "slow"];
  const order = [...providers].sort(() => rand() - 0.5);
  // Never break every provider on the route when there is a choice, so the surprise is a story and not only a failure.
  const n = Math.min(1 + (rand() < 0.5 ? 1 : 0), Math.max(providers.length - 1, 1));
  const faults = order.slice(0, n).map((provider) => ({ provider, kind: kinds[Math.floor(rand() * kinds.length)] }));
  return { prompt, faults };
}

const GO = "M2 6h7M6 3l3 3-3 3";
const OK = "M2.5 6.5l2.2 2.2L9.5 3.5";
const X = "M3 3l6 6M9 3l-6 6";
const SKIP = "M2.5 6h7";

export type Hop = { key: string; tone: "ok" | "fail" | "neutral"; icon: string; name: string; meta: string; label: string };

/** A provider name as people write it: "openai" is "OpenAI", the rest are capitalised. */
export function providerLabel(p: string): string {
  const known: Record<string, string> = { openai: "OpenAI", anthropic: "Anthropic", google: "Google", ollama: "Ollama" };
  return known[p] ?? p.charAt(0).toUpperCase() + p.slice(1);
}

const KIND_WORDS: Record<Attempt["kind"], string> = { primary: "", retry: ", retry", fallback: ", fallback", hedge: ", hedge", skipped: ", skipped" };

/** What an attempt would say in a sentence: shown to screen readers beside the shape so status is never colour alone. */
function attemptMeta(a: Attempt): string {
  if (a.kind === "skipped") return a.error ?? "skipped";
  if (a.error_kind) return `${a.status ? a.status + ", " : ""}${ms(a.latency_ms)}`;
  return `answered, ${ms(a.latency_ms)}`;
}

/** The route as a row of stops: the request, then one stop per attempt in the order they happened. */
export function hopsFor(r: Pick<PlaygroundResult, "policy" | "attempts">): Hop[] {
  const hops: Hop[] = [{ key: "request", tone: "neutral", icon: GO, name: "Request", meta: `policy ${r.policy}`, label: `Request, policy ${r.policy}` }];
  r.attempts.forEach((a, i) => {
    const failed = !!a.error_kind && a.kind !== "skipped";
    const tone = a.kind === "skipped" ? "neutral" : failed ? "fail" : "ok";
    const name = `${a.model}${KIND_WORDS[a.kind]}`;
    const meta = attemptMeta(a);
    const word = tone === "ok" ? "answered" : tone === "fail" ? "failed" : "skipped";
    hops.push({ key: `a${i}`, tone, icon: tone === "ok" ? OK : tone === "fail" ? X : SKIP, name, meta, label: `${name}, ${word}, ${meta}${a.injected ? ", made to fail on purpose" : ""}` });
  });
  return hops;
}

export type Segment = { key: string; tone: "ok" | "fail"; share: number; ms: number };

/** The timing bar: each attempt takes its share of the time spent at providers. Skipped attempts take none. */
export function segmentsFor(attempts: Attempt[]): Segment[] {
  const timed = attempts.filter((a) => a.kind !== "skipped");
  const total = timed.reduce((t, a) => t + Math.max(a.latency_ms, 0), 0);
  if (total <= 0) return [];
  return timed.map((a, i) => ({ key: `s${i}`, tone: a.error_kind ? "fail" : "ok", share: (Math.max(a.latency_ms, 0) / total) * 100, ms: a.latency_ms }));
}

export function timingLabel(r: Pick<PlaygroundResult, "attempts" | "latency_ms" | "overhead_ms">): { total: string; overhead: string; aria: string } {
  const parts = segmentsFor(r.attempts).map((s) => `${ms(s.ms)} ${s.tone === "fail" ? "failed" : "answered"}`);
  return {
    total: `${ms(r.latency_ms)} in total`,
    overhead: `${ms(r.overhead_ms)} added by Spillway`,
    aria: parts.length ? `Time spent: ${parts.join(", ")}` : "No provider was called",
  };
}

/** What the screen shows under the route: the answer, or why there is none. */
export function answerFor(r: Pick<PlaygroundResult, "answer" | "error" | "outcome">): { text: string; partial: boolean; failed: boolean } {
  if (r.error && !r.answer) return { text: r.error.message, partial: false, failed: true };
  if (r.error) return { text: r.answer, partial: true, failed: true };
  return { text: r.answer, partial: false, failed: false };
}

export function recentMeta(h: Pick<PlaygroundResult, "model" | "cache" | "attempts" | "outcome">): string {
  const failed = h.attempts.filter((a) => a.error_kind && a.kind !== "skipped").length;
  const who = h.model || "no answer";
  if (h.cache === "hit_exact" || h.cache === "hit_semantic") return `${who}, cache hit`;
  if (failed > 0) return `${who}, ${failed} failed ${failed === 1 ? "attempt" : "attempts"}`;
  return who;
}

// --- fault chips ---

export type Chip = { id: string; label: string; fault: Fault };

/** The chips offered for a policy: each provider it would try can be made to return 429 or 503; the first can also be slow or cut off. */
export function chipsFor(providers: string[]): Chip[] {
  const out: Chip[] = [];
  for (const p of providers) {
    out.push({ id: `${p}:rate_limit`, label: `${providerLabel(p)} returns 429`, fault: { provider: p, kind: "rate_limit" } });
    out.push({ id: `${p}:server_error`, label: `${providerLabel(p)} returns 503`, fault: { provider: p, kind: "server_error" } });
  }
  const first = providers[0];
  if (first) {
    out.push({ id: `${first}:slow`, label: "Slow response", fault: { provider: first, kind: "slow" } });
    out.push({ id: `${first}:cut_stream`, label: "Cut the stream halfway", fault: { provider: first, kind: "cut_stream" } });
  }
  return out;
}

/** Turning a chip on replaces any other fault on the same provider (one thing goes wrong at a time per provider). */
export function toggleFault(active: Fault[], f: Fault): Fault[] {
  const same = active.some((a) => a.provider === f.provider && a.kind === f.kind);
  const rest = active.filter((a) => a.provider !== f.provider);
  return same ? rest : [...rest, f];
}

/** After the policy changes, a fault stays only if its provider is still on the route. */
export function keepFaults(active: Fault[], providers: string[]): Fault[] {
  return active.filter((f) => providers.includes(f.provider));
}

export type Options = { system: string; temperature: string; maxTokens: string };
export const emptyOptions: Options = { system: "", temperature: "", maxTokens: "" };

export type OptionErrors = Partial<Record<"temperature" | "maxTokens" | "prompt", string>>;

export function validate(prompt: string, o: Options): OptionErrors {
  const e: OptionErrors = {};
  if (!prompt.trim()) e.prompt = "Write a prompt first.";
  else if (prompt.length > MAX_PROMPT) e.prompt = `Keep the prompt under ${MAX_PROMPT.toLocaleString("en-US")} characters.`;
  if (o.temperature.trim() !== "") {
    const t = Number(o.temperature);
    if (!Number.isFinite(t) || t < 0 || t > 2) e.temperature = "Temperature is a number from 0 to 2.";
  }
  if (o.maxTokens.trim() !== "") {
    const n = Number(o.maxTokens);
    if (!Number.isInteger(n) || n < 1 || n > 100000) e.maxTokens = "Max tokens is a whole number from 1 to 100,000.";
  }
  return e;
}

export function requestBody(policy: string, prompt: string, o: Options, faults: Fault[]): ChatRequest {
  const body: ChatRequest = { policy, prompt: prompt.trim() };
  if (o.system.trim()) body.system = o.system.trim();
  if (o.temperature.trim() !== "") body.temperature = Number(o.temperature);
  if (o.maxTokens.trim() !== "") body.max_tokens = Number(o.maxTokens);
  if (faults.length) body.faults = faults;
  if (faults.some((f) => f.kind === "cut_stream")) body.stream = true;
  return body;
}
