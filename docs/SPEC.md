# Spillway: durable agent runtime and model gateway (Go)

Name: **Spillway** (binary `spillway`). Go module path: `github.com/<handle>/spillway` (handle TBD; check name availability on GitHub and pkg.go.dev before creating the repo). License: Apache-2.0.

## What it is

A Go service, with a Next.js dashboard, that sits between applications and LLM providers and does two jobs:

1. **Model gateway.** One API in front of several providers, with routing, fallback, caching, rate limits and cost accounting.
2. **Durable agent runs.** It executes multi-step agent loops (model call, tool call, model call...) and persists every step, so a run survives a crash or a deploy and resumes where it stopped.

The name fits both jobs: a spillway is the safe overflow path when the main channel fails (fallback), and it meters and survives flood conditions (limits, crash recovery).

## Why this project

- It is backend work already done well (Go, Kubernetes, state machines, ingestion), applied to the problem every AI team has.
- It demonstrates the claims in the LinkedIn posts: systems that don't care which model is swapped in, and long-running agents that need saved state.
- Project 2 runs on top of it, so the two repos tell one story.

## What a reviewer should be able to say after 5 minutes in the README

- "This person understands failure modes of LLM calls in production."
- "The crash-recovery demo works and is tested."
- "There are real numbers: latency overhead, throughput, cache hit rate."
- "The dashboard makes the whole thing visible."

## Decisions locked

| Topic | Decision |
|---|---|
| Run model | Runtime-driven loop. The client posts a goal, system prompt, model policy and tool allowlist; the runtime loops model call, tool call, model call until the model stops calling tools or a limit trips. Explicit client-defined workflows are out of scope, but the step table must not preclude them later. |
| Model catalog | YAML/JSON file listing each model: provider, per-token input/output prices, tags (`fast`, `reasoning`, `long-context`, ...), context window. Hot-reloadable. Drives routing and cost accounting. |
| Semantic cache | In scope, behind a flag. Embeddings come from a local Ollama embedding model. Vectors are stored in Postgres with pgvector, so Redis is not needed for it. |
| Tool registry | Tools (MCP servers and HTTP webhooks) are registered once through the admin API/UI, including any secret auth headers. A run request names the tools it may use. |
| MCP | Client over streamable HTTP only. No stdio subprocess servers. |
| Run visibility | `GET /v1/runs/{id}/events` streams step events over SSE, resumable from a step cursor (`Last-Event-ID`) because steps are append-only. |
| API keys | Managed by the admin API/UI and by CLI (`spillway keys create\|list\|revoke`). |
| Dashboard | Separate Next.js app in `web/` (same repo), a second deployable. Talks to the Go admin API from its server-side layer; no admin token reaches the browser. |
| Dashboard auth | Username and password, two fixed roles. **admin**: full access. **viewer**: read-only (can see runs, usage and past playground requests, but cannot create or revoke keys, edit budgets, approve/reject steps or register tools, and never sees secret values). Both users are seeded from env on first boot (hash in Postgres, argon2 or bcrypt), session cookie issued by Next.js. Roles are enforced in the Go admin API, not only hidden in the UI. No user-management screen. |
| Deployment | docker-compose only (service, web, Postgres, optional Redis, optional Ollama). Helm chart dropped. |
| Scope cuts | None planned. See Risks. |
| Timeline | Built in phases with no fixed duration; each phase is done when its checks pass. See [TECHNICAL.md](TECHNICAL.md) section 10. |

## Scope

### In scope

| Area | Requirement |
|---|---|
| API | OpenAI-compatible `/v1/chat/completions` (streaming and non-streaming) so existing SDKs work unchanged, plus a native `/v1/runs` API |
| Providers | Adapters for Anthropic, OpenAI, Google and one local backend (Ollama). One `Provider` interface; adding a provider is one file |
| Routing | Per-request policy: fixed model, ordered fallback chain, cheapest-that-meets-a-tag, or weighted split for A/B |
| Reliability | Timeouts, retries with jittered backoff, per-provider circuit breaker, fallback on 429/5xx/timeout, hedged requests as an option |
| Streaming | SSE passthrough with correct behaviour when the client disconnects mid-stream and when the upstream fails mid-stream |
| Caching | Exact-match cache keyed on normalised request; semantic cache behind a flag with a similarity threshold |
| Limits | Per-API-key rate limits (token bucket) and monthly budgets in dollars; a request over budget gets a clear error |
| Accounting | Tokens in and out, cost, latency and cache status per request, stored and queryable by key, model and day |
| Agent runs | Run = ordered steps persisted in Postgres. Step types: model call, tool call, wait-for-human, sleep. Resume after crash. Idempotency keys on tool calls so a retried step does not run a side effect twice |
| Workers | Runs are claimed by workers using leases with heartbeats; a dead worker's runs are picked up by another |
| Tools | Registry of MCP (streamable HTTP) and HTTP webhook tools; per-run allowlist |
| Human in the loop | A step can pause the run until an approve or reject call arrives |
| Run events | SSE event stream per run, replayable from a cursor |
| Playground faults | Admin-only, per-request fault injection (429, 503, slow, cut stream) through the built-in playground key, kept out of breaker state and metrics |
| Admin API | Runs, usage, API keys and budgets, tool registry, model catalog (read-only). Every request carries the caller's role (admin or viewer) from the dashboard's session layer; write endpoints reject viewers with 403 |
| Dashboard | Next.js app, four screens (below) |
| Observability | OpenTelemetry traces (one trace per run, one span per step), Prometheus metrics, structured logs |
| Deploy | Dockerfile for the service and for `web/`, docker-compose (service, web, Postgres, optional Redis, optional Ollama) used for both local and single-server deployment |

### Dashboard screens

1. **Runs.** A page built around what needs attention: counters and a 24 hour activity chart, a Needs you card for runs waiting on a person, Running now cards each with a small graph of the run's steps, and a compact list of earlier runs. Opening a run shows its **live run graph**: model calls and tool calls in two lanes, worker bands, the point where a worker was lost and another took over, and any stopped or re-issued attempt, with a step inspector and a Timeline toggle. This is the showpiece for the crash-recovery demo.
2. **Usage and cost.** Requests, tokens, cost, latency percentiles and cache hit rate by key, model and day. Also the source of the numbers in the README.
3. **API keys and budgets.** Create, revoke, edit rate limits and monthly budgets.
4. **Playground.** A place to play with the gateway: pick a routing policy from small picture cards, send a prompt, and see the route it took (each attempt, the fallback, the retry), a timing bar, the answer and what it cost. An admin can also make a provider fail on purpose (429, 503, slow, cut stream) to watch fallback happen. Viewers can only read past requests.

The visual design is final. Dark follows `docs/design/DESIGN-linear.app.md` and light follows `docs/design/DESIGN-claude.md`; the exported tokens are in `docs/design/tokens.json` and `docs/design/tokens.css`.

### Out of scope

- Fine-tuning, embeddings API for clients (embeddings are used internally for the semantic cache only), image or audio endpoints.
- User management, custom roles, SSO. Gateway auth is API keys; dashboard auth is two seeded logins (admin, viewer).
- Helm chart and Kubernetes manifests. Deployment is docker-compose on a single server.
- Client-defined workflow graphs or a workflow DSL.
- MCP stdio transport.
- Policy or governance features. Keep this a runtime, not a security product.

## Architecture

```
 browser --> Next.js dashboard (web/)
                  | server-side calls, session cookie
client / SDK      v
     |       admin API
  HTTP API  (chat completions, runs, run events, admin)
     |
  +--------------------+---------------------+
  | gateway path       | run path            |
  | router -> cache    | run store (Postgres)|
  | -> limiter         | scheduler + leases  |
  | -> provider adapter| worker pool         |
  +---------+----------+----------+----------+
            |                     |
       LLM providers        tools (MCP, HTTP)
```

- **Postgres** holds runs, steps, API keys, usage, the tool registry, the admin user and (with pgvector) semantic-cache vectors. Steps are append-only; current run state is derived from them.
- **Redis** holds rate-limit buckets and the exact-match cache. The service must still start without Redis, with those features disabled.
- **Model catalog** is a YAML/JSON file read at startup and reloaded on change.
- **Workers** are goroutine pools inside the same binary. A `--role=api|worker|all` flag lets them scale separately.
- **Runs use the gateway path** for every model call, so routing, fallback, caching, limits and accounting apply to runs automatically. A run is billed to the API key that created it, and the key's budget applies.
- **Admin API** is bound to the dashboard's server-side layer through a shared secret; it is not exposed publicly in the default compose setup.

### Run state machine

`queued -> running -> (waiting_tool | waiting_human | sleeping) -> running -> succeeded | failed | cancelled`

Rules:
- A step is written as `started` before its side effect and `finished` after. On resume, a `started` model call is re-issued; a `started` tool call is re-issued with the same idempotency key.
- A run has a max step count, a max cost and a wall-clock deadline. Hitting any of them fails the run with a specific reason.
- Context growth is handled by a pluggable compaction hook (default: summarise oldest turns once the context passes a token threshold).
- The loop ends when the model returns a response with no tool calls.

### Defaults (not yet confirmed, change if you disagree)

- Lease TTL 30 s, heartbeat every 10 s, reaper scan every 10 s. A worker that misses its lease renewal stops executing that run.
- Run defaults: max 50 steps, max $1.00, deadline 15 min; all overridable per run, capped by server config.
- Circuit breaker opens after 5 consecutive failures or a 50% error rate over a 20-request window, half-open after 30 s.
- Retries: up to 2 per provider, jittered exponential backoff from 200 ms.
- Exact-cache key excludes `stream`, `user` and other non-semantic fields; only temperature 0 requests are cached unless a key opts in.
- Semantic cache similarity threshold 0.95, opt-in per key or per request.
- Migrations with `golang-migrate`, Postgres driver `pgx`, queries with `sqlc`.
- Go 1.23+, Next.js App Router with TypeScript.
- Webhook tools: signed with an HMAC header, 10 s timeout, idempotency key sent as `Idempotency-Key`.

## Suggested layout

```
cmd/spillway/           main, flags, keys CLI
internal/api/           HTTP handlers, SSE, admin API
internal/gateway/       router, policies, cache, limiter
internal/provider/      anthropic/, openai/, google/, ollama/
internal/runs/          store, scheduler, worker, state machine
internal/tools/         registry, mcp client, http tools
internal/usage/         accounting, budgets
internal/telemetry/     otel, metrics
config/                 models.yaml (catalog), example config
migrations/             SQL migrations
web/                    Next.js dashboard
deploy/                 compose, example env
bench/                  k6 scripts, results
docs/                   design notes, ADRs, this spec
```

## Build plan

The work is split into feature phases, in order, with no fixed duration. In each phase the backend is built first, then the frontend, then the work stops for you to verify both before the next phase starts. Phases with no screen yet say so, and are verified through the API, the CLI and tests. The full task lists and verification checklists are in [TECHNICAL.md](TECHNICAL.md) section 10.

| Phase | Feature | Backend | Frontend |
|---|---|---|---|
| 0 | Bootstrap | yes | no |
| 1 | Gateway passthrough | yes | no |
| 2 | Routing and reliability | yes | no |
| 3 | Limits, budgets, caches, telemetry | yes | no |
| 4 | Login, roles, dashboard shell | yes | yes |
| 5 | API keys | yes | yes |
| 6 | Usage and cost | yes | yes |
| 7 | Playground and fault injection | yes | yes |
| 8 | Run engine | yes | no |
| 9 | Tools, run events, chaos test | yes | no |
| 10 | Runs page | yes | yes |
| 11 | Run graph and timeline | yes | yes |
| 12 | Approvals, MCP, sleep, compaction | yes | yes |
| 13 | Deploy, evidence, README | yes | yes |

Two gates: after Phase 3 the gateway is complete and measured, and after Phase 9 the chaos test passes 50 of 50 locally and in CI.

### Risks

The full scope is kept. The two largest risks are the dashboard (ten screens counting both themes, a live graph, and auth) and carrying four provider adapters alongside everything else. Mitigations: each feature's screen is built right after its backend so the UI always has real data, the verification stops catch problems before the next phase builds on them, and the gates are the only places to trim polish (semantic-cache tuning, extra chart views) if time runs short.

## Tests and evidence

Every phase ships its own tests with its code and ends with a cheap self-check before you are asked to verify: Go unit tests under the race detector, the phase's integration tests, web unit tests, an accessibility check that runs inside the unit tests, a token-drift script, and at most one basic smoke check per screen. There are no extensive browser suites. Any test that runs longer than about a minute is skipped locally and reported, and the slow ones (the full chaos test, load tests) run in CI. Details are in [TECHNICAL.md](TECHNICAL.md) section 10.

- **Unit tests** for router policies, limiter, state machine transitions, lease logic.
- **Integration tests** against a fake provider that can return 429, 500, slow responses and broken streams on demand.
- **Chaos test:** start 50 runs, kill the worker process at random, restart, assert every run finishes exactly once with no duplicated tool side effects. Put this in CI.
- **Load test (k6):** report p50/p95/p99 added latency of the gateway over a direct call, and requests per second on one small instance. State the machine used.
- **Cache test:** replay a recorded workload and report hit rate and dollars saved, for exact and semantic cache separately.
- **Dashboard:** one basic smoke check per screen against a live compose stack (the page loads, its key element shows, a viewer's controls are disabled).
- **Roles:** integration test that a viewer session gets 403 on every admin write endpoint (keys, budgets, tools, approve/reject) and that secrets never appear in viewer responses.

## README must include

1. One-paragraph description and an architecture diagram.
2. A 60-second quick start with docker-compose.
3. The crash-recovery demo as a GIF or short video, shown in the dashboard.
4. A results table: gateway overhead, throughput, cache savings, chaos test outcome.
5. Design decisions: why append-only steps, why leases, what you would do differently at 100x scale.
6. Known limits, stated plainly. Include that exactly-once side effects depend on remote tools honouring the idempotency key.

## Resume line once it exists

"Built an open-source agent runtime in Go: multi-provider LLM gateway with fallback, caching and budgets, plus durable agent runs that resume after worker crashes (N runs, 0 duplicated side effects in chaos tests; p95 overhead X ms)."
