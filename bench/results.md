# Benchmark results

Measured with `go run ./bench` against a single gateway process and the fake provider with no delay, so the numbers are
the gateway's own cost (auth, routing, accounting, usage write queue) and not a provider's. Postgres and Redis ran in
Docker on the same machine, and so did the load generator.

| | |
|---|---|
| Machine | Apple M5 Pro, 15 logical CPUs, 24 GB, macOS (darwin/arm64), Go 1.27 |
| Date | 2026-10-10 |
| Gateway | `spillway serve --role=api`, one process, exact cache on (Redis), semantic cache off |
| Requests | 3000 per latency sample at concurrency 1, unique prompts |

## Latency added by the gateway

| | p50 | p95 | p99 |
|---|---|---|---|
| direct to the upstream | 0.04 ms | 0.06 ms | 0.08 ms |
| through the gateway | 0.24 ms | 0.34 ms | 0.58 ms |
| **added by the gateway** | **0.20 ms** | **0.28 ms** | **0.50 ms** |

## Throughput

32 concurrent clients for 10 seconds, unique prompts, 0 failed: **5,283 requests per second** through the gateway
(60,008 per second direct to the fake upstream, which does no work). The gap is the gateway doing its job on every
request: authenticating the key, planning the route, checking limits and budget, and queueing the usage row.

## Exact cache

2,000 requests drawn from 300 distinct prompts with a Zipf-like popularity, at temperature 0.

| | |
|---|---|
| Hits / misses | 1,774 / 226 |
| Hit rate | **88.7%** |
| Spend with the cache | $0.0237 |
| Avoided by hits | $0.1863 (88.7% saved) |

Prices are illustrative ($3 and $15 per million tokens) and the fake provider returns tiny answers, so the dollar
figures show the arithmetic, not a real bill. The hit rate follows the workload's repetition, not the gateway.

## Not measured

- **Semantic cache.** It needs an Ollama embedding model, which this run did not have. It is tested for correctness
  (threshold, scoping, bypass) but there is no hit-rate number for it.
- **A real provider.** Real model latency is hundreds of milliseconds to seconds on both sides of the comparison.
- **More than one instance.**
