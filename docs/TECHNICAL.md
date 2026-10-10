# Spillway: technical design and development plan

Companion to [SPEC.md](SPEC.md). The spec says *what* and *why*; this document says *how*, and ends with the phased build plan. If the two disagree, the spec wins on scope and this document wins on mechanics. Section 11 lists the places where this document sharpens or changes a spec statement.

## 1. Conventions

- Go 1.23+, `net/http` with the standard `ServeMux` (method + path patterns), `pgx/v5`, `sqlc`, `golang-migrate`, `log/slog` (JSON), OpenTelemetry Go SDK, Prometheus client.
- One binary: `spillway serve --role=api|worker|all`, plus subcommands `spillway migrate`, `spillway keys create|list|revoke`, `spillway seed`.
- All times are UTC `timestamptz`. All ids are UUIDv7 (time-ordered) unless noted.
- Money is `numeric(14,8)` USD in the database and a fixed-point integer (micro-dollars, `int64`) in Go. Never `float64` for cost.
- Canonical internal request/response types follow the OpenAI chat-completions schema. Provider adapters translate to and from it.
- Every package takes a `context.Context` first and respects cancellation. Nothing in `internal/` calls `os.Exit` or reads env vars; config is parsed once in `cmd/spillway` and passed in.

## 2. Data model (Postgres)

Source of truth for runs is the append-only `run_steps` table. Columns on `runs` are a **projection** (status, counters) plus operational lease fields; the projection is updated in the same transaction as the step that causes it and can be rebuilt from steps.

```sql
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE users (
  id            uuid PRIMARY KEY,
  username      text UNIQUE NOT NULL,
  password_hash text NOT NULL,                       -- argon2id
  role          text NOT NULL CHECK (role IN ('admin','viewer')),
  created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
  id                 uuid PRIMARY KEY,
  name               text NOT NULL,
  key_prefix         text NOT NULL,                   -- first 8 chars, shown in UI
  key_hash           bytea NOT NULL UNIQUE,           -- sha256 of full key
  rate_limit_rpm     int,                             -- null = unlimited
  monthly_budget_usd numeric(14,8),                   -- null = unlimited
  semantic_cache     boolean NOT NULL DEFAULT false,
  cache_nonzero_temp boolean NOT NULL DEFAULT false,  -- allow caching temperature > 0
  created_at         timestamptz NOT NULL DEFAULT now(),
  revoked_at         timestamptz
);

CREATE TABLE tools (
  id                uuid PRIMARY KEY,
  name              text UNIQUE NOT NULL,             -- exposed to the model
  kind              text NOT NULL CHECK (kind IN ('mcp','http')),
  endpoint          text NOT NULL,
  headers_enc       bytea,                            -- AES-GCM encrypted JSON of auth headers
  description       text,
  input_schema      jsonb,                            -- http: supplied; mcp: discovered
  timeout_ms        int NOT NULL DEFAULT 10000,
  requires_approval boolean NOT NULL DEFAULT false,
  created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE runs (
  id               uuid PRIMARY KEY,
  api_key_id       uuid NOT NULL REFERENCES api_keys(id),
  idempotency_key  text,                              -- client-supplied, dedupes run creation
  status           text NOT NULL CHECK (status IN
                     ('queued','running','waiting_tool','waiting_human','sleeping',
                      'succeeded','failed','cancelled')),
  request          jsonb NOT NULL,                    -- original POST body, immutable
  failure_reason   text,                              -- max_steps|max_cost|deadline|provider_failed|tool_failed|cancelled|rejected
  step_count       int NOT NULL DEFAULT 0,
  cost_usd         numeric(14,8) NOT NULL DEFAULT 0,
  deadline_at      timestamptz NOT NULL,
  -- operational, not history:
  lease_owner      text,
  lease_expires_at timestamptz,
  lease_epoch      bigint NOT NULL DEFAULT 0,         -- fencing token, bumped on every claim
  wake_at          timestamptz,                       -- set while sleeping
  created_at       timestamptz NOT NULL DEFAULT now(),
  finished_at      timestamptz,
  UNIQUE (api_key_id, idempotency_key)
);
CREATE INDEX runs_claimable ON runs (status, lease_expires_at, wake_at)
  WHERE status IN ('queued','running','waiting_tool','sleeping');

CREATE TABLE run_steps (
  id              bigserial PRIMARY KEY,              -- also the SSE cursor
  run_id          uuid NOT NULL REFERENCES runs(id),
  step_no         int,                                -- null for run_status rows
  type            text NOT NULL CHECK (type IN
                    ('model_call','tool_call','wait_human','sleep','compaction','run_status')),
  phase           text NOT NULL CHECK (phase IN ('started','reissued','finished','failed')),
  idempotency_key text,                               -- tool_call only
  payload         jsonb NOT NULL,
  cost_usd        numeric(14,8) NOT NULL DEFAULT 0,
  lease_epoch     bigint NOT NULL,                    -- epoch of the writer, for audit
  worker_id       text NOT NULL DEFAULT '',           -- worker that wrote the row; the run graph's worker bands read this
  created_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (run_id, step_no, phase, lease_epoch)        -- one row per phase per lease epoch
);
-- Whatever epoch wrote it, a step has at most one terminal row.
CREATE UNIQUE INDEX run_steps_terminal ON run_steps (run_id, step_no) WHERE phase IN ('finished','failed');
CREATE INDEX run_steps_by_run ON run_steps (run_id, id);

CREATE TABLE usage (                                  -- one row per gateway request
  id              uuid PRIMARY KEY,
  api_key_id      uuid REFERENCES api_keys(id),
  run_id          uuid REFERENCES runs(id),
  policy          text NOT NULL,                       -- policy or model name the client asked for
  provider        text,
  model           text,                                -- model that actually answered
  input_tokens    int NOT NULL DEFAULT 0,
  output_tokens   int NOT NULL DEFAULT 0,
  cost_usd        numeric(14,8) NOT NULL DEFAULT 0,
  saved_usd       numeric(14,8) NOT NULL DEFAULT 0,    -- what a cache hit avoided
  latency_ms      int NOT NULL,
  ttfb_ms         int,
  cache_status    text NOT NULL CHECK (cache_status IN ('miss','hit_exact','hit_semantic','bypass')),
  outcome         text NOT NULL CHECK (outcome IN
                    ('ok','client_cancelled','upstream_error','all_providers_failed','rate_limited','over_budget','invalid')),
  attempts        jsonb NOT NULL DEFAULT '[]',         -- [{provider,model,kind,latency_ms,error}]
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX usage_key_time ON usage (api_key_id, created_at);
CREATE INDEX usage_model_time ON usage (model, created_at);

CREATE TABLE usage_daily (                            -- rollup; drives budgets and the dashboard
  api_key_id   uuid NOT NULL,
  day          date NOT NULL,
  model        text NOT NULL,
  requests     bigint NOT NULL DEFAULT 0,
  input_tokens bigint NOT NULL DEFAULT 0,
  output_tokens bigint NOT NULL DEFAULT 0,
  cost_usd     numeric(14,8) NOT NULL DEFAULT 0,
  saved_usd    numeric(14,8) NOT NULL DEFAULT 0,
  cache_hits   bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (api_key_id, day, model)
);

CREATE TABLE semantic_cache (
  id          uuid PRIMARY KEY,
  scope       uuid NOT NULL,                           -- api_key_id (cache is per key by default)
  model_group text NOT NULL,                           -- policy the entry answers
  embedding   vector(768) NOT NULL,                    -- dimension fixed by the configured embed model
  response    jsonb NOT NULL,
  cost_usd    numeric(14,8) NOT NULL,                  -- cost of the original call, for "saved"
  created_at  timestamptz NOT NULL DEFAULT now(),
  expires_at  timestamptz NOT NULL
);
CREATE INDEX semantic_cache_ann ON semantic_cache USING hnsw (embedding vector_cosine_ops);
```

Notes:
- `usage` insert and the `usage_daily` upsert happen in one transaction per request, off the response path (buffered channel with a flush worker, so a slow database never adds request latency; on shutdown the buffer is drained).
- Budget check reads `SUM(cost_usd)` for the key and current month from `usage_daily`, cached in memory for 5 s per key. This makes budgets **approximate**: concurrent in-flight requests can overshoot by what they cost. The README states this plainly.
- Secrets in `tools.headers_enc` are encrypted with AES-256-GCM using `SPILLWAY_SECRET_KEY`. Admin API responses return header *names* only, never values.

## 3. Gateway internals

### 3.1 Request pipeline

```
auth (API key) -> parse + validate -> resolve policy -> rate limit -> budget check
  -> exact cache -> semantic cache (if enabled) -> router -> [retry / breaker / fallback / hedge]
  -> provider adapter -> response (stream or body) -> usage record + cache fill
```

- The `model` field of a chat request is either a concrete catalog model id or a **policy name** from config (section 8). A concrete id is shorthand for a `fixed` policy.
- Response headers on every response: `X-Spillway-Request-Id`, `X-Spillway-Provider`, `X-Spillway-Model`, `X-Spillway-Cache` (`miss|hit-exact|hit-semantic|bypass`), `X-Spillway-Attempts`. For non-streaming responses also `X-Spillway-Cost-Usd`. Streaming responses fix headers before cost is known, so cost for streams is visible only via the admin API.
- The service forces `stream_options.include_usage=true` upstream so token counts exist for streams. If a provider omits usage anyway, tokens are estimated with a tokenizer approximation and the usage row is flagged `estimated` in `attempts`.

### 3.2 Provider interface

```go
type Provider interface {
    Name() string
    Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
    ChatStream(ctx context.Context, req *ChatRequest) (StreamReader, error)
}

// Implemented only by providers that can embed (Ollama is required; others optional).
type Embedder interface {
    Embed(ctx context.Context, model string, input []string) ([][]float32, error)
}

type StreamReader interface {
    Next() (*ChatChunk, error) // io.EOF at normal end
    Close() error              // must cancel the upstream request
}

type ErrKind int
const (
    KindRateLimited ErrKind = iota // 429
    KindServer                     // 5xx
    KindTimeout
    KindBadRequest                 // 4xx the client caused: never retried, never failed over
    KindAuth                       // bad upstream credentials: fails over, logged loudly
    KindContextLength              // fails over only to a model with a larger context window
    KindCanceled                   // caller went away
)
type ProviderError struct {
    Kind       ErrKind
    Status     int
    RetryAfter time.Duration
    Err        error
}
```

Adapters live in `internal/provider/<name>/` and register themselves in a map. Each translates the OpenAI schema (messages, `tools`, `tool_calls`, `tool` role, `stream`, `max_tokens`, `temperature`) to the provider's wire format and back, including tool calling, because runs depend on it. Adapter tests use recorded fixtures (`httptest` servers replaying captured responses); no test hits a real provider.

A `fake` provider (`internal/provider/fake`) is scriptable per test and also ships as a tiny HTTP server (`cmd/fakeprovider`) speaking the OpenAI API. It can return 429 with `Retry-After`, 500, a fixed or random delay, a stream that breaks after N chunks, and scripted tool calls. It is used by integration tests, the chaos test, and the load test baseline.

### 3.3 Routing policies

```go
type Policy interface {
    // Candidates returns the ordered attempts the router may make for this request.
    Candidates(req *ChatRequest, cat *Catalog, health Health) []Candidate
}
```

| Type | Behaviour |
|---|---|
| `fixed` | One model. No fallback unless `fallback:` is listed. |
| `fallback` | Ordered list; next candidate on retryable failure. |
| `cheapest` | Models in the catalog carrying `tag`, filtered by context window fit and breaker state, sorted by estimated cost (input tokens estimated from the request, output assumed at `max_tokens` or a configured default). Remaining candidates form the fallback order. |
| `weighted` | Picks the first candidate by weighted random draw (sticky per `X-Spillway-Bucket` header if present, for clean A/B), then falls back in listed order. The chosen arm is recorded in `usage`. |

Policies only *propose* candidates. Execution (retries, breaker, hedging) is one shared function, so policies stay pure and unit-testable without I/O.

### 3.4 Reliability

- **Timeouts:** per attempt `request_timeout` (default 60 s non-streaming); for streams a `first_byte_timeout` (default 15 s) and an `idle_timeout` between chunks (default 30 s).
- **Retries:** per candidate, up to `max_retries` (default 2) on `RateLimited`, `Server`, `Timeout`; delay = `min(cap, base * 2^n) * rand(0.5..1.0)` (full jitter lower bound), base 200 ms, cap 5 s. A `RetryAfter` from the provider overrides the computed delay if it is shorter than the remaining request budget; otherwise the candidate is skipped.
- **Breaker:** one per provider, sliding window of the last 20 attempts. Opens at 5 consecutive failures or ≥50% failures in a full window. While open, candidates from that provider are skipped. After 30 s it goes half-open and admits one probe; success closes it, failure re-opens. `KindBadRequest` and `KindCanceled` never count as failures.
- **Fallback:** when a candidate is exhausted (retries spent or breaker open), move to the next. If all are exhausted the client gets `503 all_providers_failed` with the attempt list.
- **Hedging:** opt-in per policy (`hedge_after_ms`), non-streaming only. If the first attempt has not answered after the delay, fire the next candidate in parallel; first success wins and the loser's context is cancelled. Both attempts are recorded; the loser's cost is recorded if the provider reports it.
- **Total budget:** a request-level deadline (`gateway.max_total_ms`, default 120 s) caps retries plus fallbacks.

### 3.5 Streaming semantics

| Situation | Behaviour |
|---|---|
| Failure **before the first byte** reaches the client | Handled like a non-streaming failure: retry or fail over transparently. |
| Upstream fails **after** bytes were sent | Cannot switch provider without corrupting the stream. Send a final SSE chunk `data: {"error": {...}}`, then `data: [DONE]`, close. Record `outcome=upstream_error` with tokens received so far. No retry. |
| Client disconnects mid-stream | The request context is cancelled; `StreamReader.Close()` cancels the upstream request so tokens stop being generated. Record `outcome=client_cancelled`, tokens so far (estimated if needed). The partial response is never cached. |
| Cache hit on a stream request | Replay the cached completion as SSE chunks, paced without artificial delay. |
| Slow client | Writes use a per-write deadline; a client that stalls past it is treated as disconnected. |

SSE implementation: `http.ResponseController` for flushing and write deadlines, `Content-Type: text/event-stream`, `Cache-Control: no-cache`, and `X-Accel-Buffering: no`.

### 3.6 Caching

- **Exact cache (Redis):** key = sha256 of the canonicalised request (model/policy name, messages, tools, temperature, top_p, max_tokens, response_format, stop; excluding `stream`, `user`, `stream_options`) prefixed with the API key id (cache is per key by default; a server flag `cache.scope: global` shares entries). Only requests with `temperature == 0` (or unset where the provider default is 0) are cached unless the key sets `cache_nonzero_temp`. TTL default 24 h. Value = serialised response + original cost.
- **Semantic cache (pgvector):** only for keys with `semantic_cache=true`. Embed the last user message plus a hash of the system prompt and tool list (entries only match when those are identical) with the Ollama embedding model; nearest neighbour by cosine in the same `scope` + `model_group`; hit if similarity ≥ `0.95` (configurable). Entries are written asynchronously after a successful `miss`. Skipped for requests with tool calls in history.
- **Savings:** a hit records `cost_usd=0` and `saved_usd=` the stored original cost.
- Without Redis the exact cache and the rate limiter are disabled and the startup log says so. The semantic cache and budgets live in Postgres and keep working.

### 3.7 Rate limiting and budgets

- Token bucket per API key in Redis via a Lua script (atomic refill + take), capacity = `rate_limit_rpm`, refill `rate_limit_rpm/60` per second. Rejection: `429 rate_limit_exceeded` with `Retry-After`.
- Budget: if `spend_this_month >= monthly_budget_usd` the request is rejected before any provider call with `402 budget_exceeded`. Month boundary is UTC.

### 3.8 Error shapes

OpenAI-compatible so SDKs surface them: `{"error": {"message", "type", "code", "param"}}`.

| HTTP | `code` | When |
|---|---|---|
| 400 | `invalid_request` | Malformed body, unknown model or policy |
| 401 | `invalid_api_key` | Missing, unknown or revoked key |
| 402 | `budget_exceeded` | Monthly budget spent |
| 429 | `rate_limit_exceeded` | Key bucket empty (`Retry-After` set) |
| 502 | `upstream_error` | Single upstream failed with a non-retryable error |
| 503 | `all_providers_failed` | Every candidate exhausted; body includes `attempts` |

## 4. Run engine

### 4.1 Run request

`POST /v1/runs` (header `Idempotency-Key` optional, dedupes creation per key):

```json
{
  "input": "Research X and email me a summary",       // string, or a messages array
  "system": "You are ...",
  "model": "default",                                   // policy name or model id
  "tools": ["web_search", "send_email"],                // names from the registry
  "approval_required": ["send_email"],                  // optional, adds to tools.requires_approval
  "limits": { "max_steps": 50, "max_cost_usd": 1.0, "deadline_seconds": 900 },
  "compaction": { "threshold_tokens": 24000, "keep_last_turns": 6 },
  "metadata": { }
}
```

Server caps override request limits. Response: `202` with `{id, status: "queued"}`.

The model also receives two **built-in tools** (no registry entry): `sleep(seconds)` and `request_human_approval(reason)`. Calling them produces a `sleep` or `wait_human` step. Registry tools marked `requires_approval` produce a `wait_human` step *before* the tool call runs.

### 4.2 Steps and phases

A logical step has a `step_no`. Its lifecycle is two or three appended rows (never updated):

| Type | `started` payload | `finished` payload |
|---|---|---|
| `model_call` | `{policy, message_count}` | `{message, finish_reason, usage, provider, model, usage_id}` |
| `tool_call` | `{tool, arguments, tool_call_id}` (+ `idempotency_key` column) | `{result}` or `failed` `{error}` |
| `wait_human` | `{reason, tool?, arguments?}` | `{decision: approve\|reject, by, note}` |
| `sleep` | `{seconds, wake_at}` | `{}` |
| `compaction` | `{replaces_through_step}` | `{summary}` |
| `run_status` | (no `started`) | `{status, reason?}`; `step_no` is null |

`run_status` rows are how status changes reach the event stream and the audit trail. `runs.status` is set in the same transaction.

**Re-issued steps.** When a worker resumes a step that has a `started` row and no terminal row, it appends a `reissued` row (payload `{previous_worker, previous_epoch}`, written with its own `worker_id` and `lease_epoch`) instead of a second `started`, then performs the side effect again. A step interrupted twice has two `reissued` rows, one per epoch. The `started` row stays as history of the first attempt. The run graph is drawn from these rows: a `started` with a later `reissued` and no terminal row from the same epoch is the stopped attempt, and the `reissued` row with the terminal row is the recovered one.

**Idempotency key** for a tool call: `hex(sha256(run_id || ":" || step_no))`. It is a pure function of the step, so a re-issued step after a crash sends the identical key. Webhook tools get it as `Idempotency-Key`; MCP tool calls carry it in `_meta` as `spillway/idempotencyKey` (servers that ignore it cannot dedupe; see Known limits).

### 4.3 Claiming and leases

A worker loop (N goroutines, `--workers`, default 8) polls every 1 s (immediately after finishing a run). Claim:

```sql
UPDATE runs SET lease_owner = $1,
                lease_expires_at = now() + $2::interval,   -- 30s
                lease_epoch = lease_epoch + 1,
                status = CASE WHEN status = 'queued' THEN 'running' ELSE status END
WHERE id = (
  SELECT id FROM runs
  WHERE (
          status IN ('queued','running','waiting_tool')
          AND (lease_expires_at IS NULL OR lease_expires_at < now())
        )
     OR (status = 'sleeping' AND wake_at <= now()
          AND (lease_expires_at IS NULL OR lease_expires_at < now()))
  ORDER BY created_at
  FOR UPDATE SKIP LOCKED
  LIMIT 1
)
RETURNING *;
```

- `waiting_human` runs are never claimable. The approve/reject API appends the `wait_human` `finished` row, sets status `running` and clears the lease, which makes the run claimable.
- A run in `running` or `waiting_tool` with an expired lease belongs to a dead worker; the claim above is the recovery path. No separate reaper is needed.
- **Heartbeat** every 10 s: `UPDATE runs SET lease_expires_at = now() + 30s WHERE id=$1 AND lease_owner=$2 AND lease_epoch=$3`. Zero rows means the lease was lost; the worker cancels that run's context immediately and abandons it.
- **Fencing:** every step append is `INSERT ... SELECT ... WHERE EXISTS (SELECT 1 FROM runs WHERE id=$run AND lease_owner=$me AND lease_epoch=$epoch)`. A zombie worker that is paused past its lease and wakes up cannot write, because a new claim bumped the epoch. The `run_steps_terminal` unique index (one `finished` or `failed` row per step across all epochs) is the second line of defence.
- A graceful shutdown (SIGTERM) stops claiming, lets in-flight steps finish up to a grace period, then releases leases (`lease_expires_at = now()`) so another worker resumes at once.

### 4.4 The loop and resume algorithm

On claim, the worker **replays** instead of keeping state:

1. Load `run_steps` for the run ordered by `id`.
2. Rebuild the message list: system + input, then for each `finished` `model_call` the assistant message, for each `finished` `tool_call` the tool result message; a `compaction` `finished` row replaces everything up to `replaces_through_step` with its summary.
3. Find the **frontier**:
   - A step with `started` and no `finished`/`failed`: append a `reissued` row (section 4.2), then re-issue it. A `model_call` is simply called again (no side effect, cost is charged again and visible). A `tool_call` is re-issued with the same idempotency key. A `wait_human` re-enters the waiting state. A `sleep` computes remaining time from `wake_at`.
   - Otherwise, if the last finished step is a `model_call` with tool calls not yet executed: execute the next unexecuted tool call as a new step (tool calls from one model response run sequentially, each as its own step).
   - Otherwise if it is a `model_call` without tool calls: write `run_status succeeded`.
   - Otherwise (fresh run or after tool results): start a new `model_call`.
4. Before **every** new step, check limits: `step_count >= max_steps`, `cost_usd >= max_cost`, `now() >= deadline_at`. A trip writes `run_status failed` with the specific `failure_reason`.
5. Write `started`, perform the side effect, write `finished`/`failed`, update the projection and run `cost_usd` in the same transaction as the `finished` row. Repeat.

Failure handling inside a run:
- Model call: the gateway already retries and fails over. If it returns `503 all_providers_failed`, the run retries the step with backoff up to 3 times, then fails with `provider_failed`.
- Tool call failure (error or timeout): recorded as `failed` and the error text is fed back to the model as the tool result, so the model can recover. Only after `tool_error_budget` (default 3 consecutive) does the run fail with `tool_failed`.
- Model calls from runs go through the gateway path in-process (not over HTTP), tagged with `run_id` so `usage` rows link to the run, and charged to the creating key, subject to its rate limit and budget. An over-budget key fails the run with `max_cost` reason `key_budget`.

### 4.5 Run events (SSE)

`GET /v1/runs/{id}/events` (and the admin equivalent). Each event is one `run_steps` row:

```
id: 1842
event: step.finished
data: {"step_no":3,"type":"tool_call","phase":"finished","payload":{...},"cost_usd":"0.0000","at":"..."}
```

- Event names: `step.started`, `step.reissued`, `step.finished`, `step.failed`, `run.status`.
- `id` is `run_steps.id`. A client reconnects with `Last-Event-ID` (or `?after=`) and the server first replays rows greater than that id, then switches to live. Ordering within a run is safe because only the lease holder writes a run's steps, serialised by the fencing check.
- Live delivery: on every step insert, `NOTIFY run_steps, '<run_id>:<id>'`. Each API instance `LISTEN`s and fans out to subscribed connections for that run. A 15 s heartbeat comment (`: ping`) keeps proxies from closing idle streams; a 1 s catch-up poll covers a missed notification.
- The stream ends after the terminal `run.status` event.

### 4.6 Context compaction

Before each model call the worker estimates the history's token count. If it exceeds `compaction.threshold_tokens`, it appends a `compaction` step: a model call (through the gateway, `compaction_policy`, default the cheapest policy) summarises all turns except the last `keep_last_turns`. The hook is an interface (`Compactor`) with the summarising implementation as the default; replaying a run honours compaction rows, so resume stays deterministic.

## 5. Tools

- **HTTP tool:** `POST endpoint` with JSON body `{"tool": name, "arguments": {...}, "run_id", "step_no"}`, headers from the registry, plus `Idempotency-Key` and `X-Spillway-Signature: sha256=<hmac of body>` (secret from the tool's headers or `SPILLWAY_WEBHOOK_SECRET`). Expects `200` with JSON; non-2xx or timeout is a tool failure. Response size capped (default 256 KB) and truncated with a marker.
- **MCP tool (streamable HTTP):** at registration, `POST /admin/tools/{id}/discover` connects, runs `initialize` and `tools/list`, and stores the schemas. A registry row for an MCP server exposes each discovered tool as `<server>.<tool>`. Calls use `tools/call`. One MCP session per run, re-initialised after resume.
- The tool schemas shown to the model come from `input_schema`. Tool names the run did not allowlist are never offered, and a model-produced call to a non-allowlisted tool is rejected as a tool failure fed back to the model.

## 6. APIs

### 6.1 Public API (API key in `Authorization: Bearer`)

| Method and path | Purpose |
|---|---|
| `POST /v1/chat/completions` | OpenAI-compatible, streaming and non-streaming |
| `GET /v1/models` | Catalog models and policy names (OpenAI list shape) |
| `POST /v1/runs` | Create a run |
| `GET /v1/runs/{id}` | Run summary: status, counters, cost, failure reason |
| `GET /v1/runs/{id}/steps?after=` | Step rows |
| `GET /v1/runs/{id}/events` | SSE, resumable |
| `POST /v1/runs/{id}/approve` · `/reject` | Body `{note}`; resolves the pending `wait_human` step |
| `POST /v1/runs/{id}/cancel` | Cooperative cancel; the worker writes `run_status cancelled` at the next step boundary |
| `GET /healthz` · `GET /readyz` · `GET /metrics` | Liveness, readiness (Postgres reachable), Prometheus |

A run is visible only to the key that created it.

### 6.2 Admin API (session token)

Auth is owned by the Go service so the role is never merely asserted by the web app:
- `POST /admin/login {username,password}` verifies against `users` and returns an HMAC-signed token (claims: user, role, expiry 12 h), signed with `ADMIN_SESSION_SECRET`.
- The Next.js route handler stores it in an `httpOnly`, `SameSite=Strict`, `Secure` cookie and forwards it as `Authorization: Bearer` on server-side calls. The browser never sees the token in JS.
- Login attempts are rate-limited per IP and username (5 per minute) with constant-time comparison and argon2id.
- The admin API listens on the same port under `/admin` but compose does not publish it beyond the internal network; only the Next.js container reaches it.

| Endpoint | admin | viewer |
|---|---|---|
| `GET /admin/me` | ✔ | ✔ |
| `GET /admin/runs`, `GET /admin/runs/{id}`, `GET /admin/runs/{id}/events` | ✔ | ✔ |
| `GET /admin/runs/summary`, `GET /admin/runs/activity`, `GET /admin/runs/{id}/graph` (section 6.3) | ✔ | ✔ |
| `POST /admin/runs/{id}/approve\|reject\|cancel` | ✔ | 403 |
| `GET /admin/usage/summary?from&to&group_by=key\|model\|day` | ✔ | ✔ |
| `GET /admin/usage/requests?...` (paged request log with attempts) | ✔ | ✔ |
| `GET /admin/keys` | ✔ | ✔ (prefix, limits, spend; never the key) |
| `POST /admin/keys` (full key returned once) · `PATCH` · `DELETE` (revoke) | ✔ | 403 |
| `GET /admin/tools` | ✔ | ✔ (header names only) |
| `POST/PUT/DELETE /admin/tools`, `POST /admin/tools/{id}/discover` | ✔ | 403 |
| `GET /admin/models` (catalog and policies, read-only) | ✔ | ✔ |
| `POST /admin/playground/chat` (with optional fault injection, section 6.4) | ✔ | 403 |
| `GET /admin/playground/history` | ✔ | ✔ |

Playground requests run through the normal gateway path under a built-in `playground` API key (budget configurable, default $5/month) and return the answer plus `{provider, model, cache_status, tokens, cost, latency_ms, attempts}`. Because it spends money it is admin-only; viewers see the recorded history.

List endpoints use keyset pagination (`?limit=&cursor=`). Errors use `{"error": {"code","message"}}`.

### 6.3 Data the dashboard needs

The final design (docs/design) needs these reads beyond plain lists. All are read-only and open to both roles.

| Endpoint | Returns | Used by |
|---|---|---|
| `GET /admin/runs/summary` | Counts by status, and the runs waiting for a person: `{id, goal, tool, waiting_since, key}` | Runs page counters and the Needs you card |
| `GET /admin/runs/activity?hours=24` | One bucket per hour: `{hour, succeeded, failed, in_progress}` | Runs page activity chart |
| `GET /admin/runs?status=&cursor=` | Each run carries `strip`: up to 16 `{kind: model\|tool, state: done\|current\|failed\|waiting}` plus a `more` count | Running now cards and Earlier rows |
| `GET /admin/runs/{id}/graph` | The graph payload below | Run graph and Timeline |

The graph payload (one `nodes` entry per attempt, so a re-issued step appears twice):

```json
{
  "run": { "id": "…", "status": "running", "steps": 8, "cost_usd": "0.0624", "deadline_at": "…" },
  "workers": [ { "id": "w-2", "epochs": [1, 2] }, { "id": "w-4", "epochs": [3] } ],
  "nodes": [
    { "step_no": 3, "type": "model_call", "state": "finished", "worker": "w-2", "epoch": 1,
      "duration_ms": 2900, "cost_usd": "0.0071", "model": "sonnet",
      "attempts": [ { "provider": "openai", "model": "mini", "kind": "server", "status": 503, "ms": 340 },
                    { "provider": "anthropic", "model": "sonnet", "ok": true, "ms": 2560 } ] },
    { "step_no": 6, "type": "tool_call", "state": "stopped",  "worker": "w-2", "epoch": 2 },
    { "step_no": 6, "type": "tool_call", "state": "finished", "worker": "w-4", "epoch": 3,
      "reissued": true, "idempotency_key": "4b21d0…77", "duration_ms": 200 }
  ],
  "recoveries": [ { "after_step": 6, "from_worker": "w-2", "to_worker": "w-4", "epoch": 3, "at": "…" } ]
}
```

Derivation: nodes come from `run_steps` grouped by `(step_no, lease_epoch)`; `state` is `stopped`, `finished`, `running` or `failed` by the rules in section 4.2; `attempts` for a model call come from the `usage` row named by `model_call.finished.payload.usage_id`; `recoveries` come from epoch changes between consecutive rows.

### 6.4 Playground fault injection

The Playground lets an admin make a provider fail on purpose, to watch fallback happen.

- `POST /admin/playground/chat` takes `faults: [{provider, kind}]` where `kind` is `rate_limit` (429), `server_error` (503), `slow` (adds a configurable delay) or `cut_stream` (breaks the stream after N chunks; needs `stream: true`).
- The handler wraps each named provider in the same fault decorator the fake provider uses in tests, for that one request only. It is never global and never reachable from `/v1/chat/completions`, which ignores any fault input.
- Injected failures are tagged `injected: true` in `usage.attempts` and **do not count toward breaker state or provider-health metrics**, so playing with faults cannot trip a real breaker.
- It is allowed only for the built-in `playground` key, only for the admin role, and only while `playground.fault_injection` is true in config.

### 6.5 Dashboard app (`web/`)

- **Stack.** Next.js App Router with TypeScript. Server components fetch data through the Go admin API; client components handle SSE-driven views.
- **Routes.** `/login`, `/` (Runs), `/runs/[id]` (Graph by default, `?view=timeline`), `/usage`, `/keys`, `/playground`.
- **Themes.** `data-theme="dark|light"` on `<html>`, chosen by a cookie and defaulting to the system preference. The two themes are separate token sets from `docs/design/tokens.json`, generated into `web/src/styles/tokens.css` by `npm run tokens`. Each follows its own design system file, so the title typeface, accent colour and button size differ between themes by design.
- **Components.** `Shell`, `NavItem`, `StatusBadge`, `StepStrip`, `StatTile`, `ActivityBars`, `NeedsYouCard`, `RunCard`, `RunRow`, `RunGraph`, `Timeline`, `StepInspector`, `KpiRow`, `SpendChart`, `CacheBar`, `BreakerPill`, `KeyTable`, `BudgetBar`, `KeyRevealPanel`, `PolicyCard`, `RouteTrace`, `FaultChips`, `Composer`, `RoleGate`.
- **Run graph layout.** A pure function `layoutGraph(payload) → {nodes, edges, bands, cuts}` using the geometry in `tokens.json` (`graph`): columns are attempts in order, two lanes by step type, a worker band per worker, a dashed cut line and an extra horizontal shift per recovery, and a small chip above a model call whose first attempt failed. Nodes and edges are absolutely positioned elements, not one SVG, so they stay inspectable and selectable. `layoutGraph` has unit tests with fixtures (no recovery, one, two, with fallback chip, with a running node).
- **Live updates.** The browser opens `/api/runs/[id]/events`, a Next route handler that proxies the Go SSE stream with the session's Bearer token. The graph store applies events by id and reconnects with `Last-Event-ID`.
- **Roles.** `/admin/me` provides the role; `RoleGate` disables controls for a viewer and explains why. The API enforces it regardless.
- **Accessibility.** Status is always shape plus word. The focus ring uses the theme's `--focus` at full strength. Animations stop under `prefers-reduced-motion`. The design's known contrast gaps are listed in `docs/design/tokens.json` under `knownGaps`.

## 7. Observability

- **Traces:** one trace per run (`run.execute`), a child span per step (`step.model_call`, `step.tool_call`, ...), and gateway spans (`gateway.request`, `gateway.attempt`) nested under the step. On resume, the new worker starts a span linked to the run's trace id, which is stored on the run so the trace survives a crash. Standard OTLP exporter via env.
- **Metrics (Prometheus):** `spillway_gateway_requests_total{policy,provider,model,outcome,cache}`, `spillway_gateway_latency_seconds` and `..._overhead_seconds` (time spent in Spillway excluding the upstream call), `spillway_provider_attempts_total{provider,kind}`, `spillway_breaker_state{provider}`, `spillway_cost_usd_total{key,model}`, `spillway_cache_hits_total{kind}`, `spillway_runs{status}` gauge, `spillway_run_steps_total{type}`, `spillway_lease_lost_total`, `spillway_worker_inflight_runs`.
- **Logs:** slog JSON with `request_id`, `run_id`, `step_no`, `api_key_id`, `trace_id`. Prompts and completions are **not** logged by default (`log.bodies: false`).

## 8. Configuration

`config/models.yaml` (catalog and policies; prices below are placeholders to be replaced with current provider prices):

```yaml
providers:
  anthropic: { api_key_env: ANTHROPIC_API_KEY }
  openai:    { api_key_env: OPENAI_API_KEY }
  google:    { api_key_env: GOOGLE_API_KEY }
  ollama:    { base_url: http://ollama:11434, embed_model: nomic-embed-text }

models:
  - id: sonnet
    provider: anthropic
    upstream: <provider model id>
    input_usd_per_mtok: 0.00      # fill in
    output_usd_per_mtok: 0.00     # fill in
    context_window: 200000
    tags: [reasoning, long-context]
  - id: mini
    provider: openai
    upstream: <provider model id>
    input_usd_per_mtok: 0.00
    output_usd_per_mtok: 0.00
    context_window: 128000
    tags: [fast]
  - id: local
    provider: ollama
    upstream: <ollama model>
    input_usd_per_mtok: 0
    output_usd_per_mtok: 0
    context_window: 32000
    tags: [fast, free]

policies:
  - { name: default,    type: fallback, models: [sonnet, mini, local] }
  - { name: cheap-fast, type: cheapest, tag: fast }
  - { name: ab-test,    type: weighted, arms: [{model: sonnet, weight: 50}, {model: mini, weight: 50}] }
  - { name: hedged,     type: fallback, models: [mini, sonnet], hedge_after_ms: 2000 }

gateway:    { request_timeout: 60s, first_byte_timeout: 15s, idle_timeout: 30s, max_total_ms: 120000, max_retries: 2 }
breaker:    { consecutive_failures: 5, error_rate: 0.5, window: 20, open_for: 30s }
cache:      { exact_ttl: 24h, scope: key, semantic_threshold: 0.95 }
runs:       { lease_ttl: 30s, heartbeat: 10s, workers: 8, max_steps: 50, max_cost_usd: 1.0, deadline: 15m, tool_error_budget: 3 }
playground: { key: playground, monthly_budget_usd: 5, fault_injection: true }
```

The file is watched and reloaded atomically; an invalid reload is rejected and the previous catalog stays active, with an error log.

Environment variables: `DATABASE_URL`, `REDIS_URL` (optional), `SPILLWAY_ROLE`, `SPILLWAY_CONFIG`, `SPILLWAY_SECRET_KEY` (32 bytes, base64), `ADMIN_SESSION_SECRET`, `SEED_ADMIN_USER/PASSWORD`, `SEED_VIEWER_USER/PASSWORD`, `SPILLWAY_WEBHOOK_SECRET`, provider keys named in config, `OTEL_EXPORTER_OTLP_ENDPOINT`, `LOG_LEVEL`. `web/` uses `SPILLWAY_ADMIN_URL` (internal service URL) and `SESSION_COOKIE_SECURE`.

## 9. Testing design

| Layer | What | How |
|---|---|---|
| Unit | Policies (candidate ordering, cheapest selection, weighted distribution with seeded RNG), limiter (against Redis in a container), breaker transitions, backoff bounds, cache key canonicalisation, state machine transitions, replay/frontier logic on synthetic step lists, compaction replay | Pure Go tests, table-driven |
| Adapter | Request/response translation incl. tool calls and streams for each provider | Fixture replay with `httptest` |
| Integration | Gateway against the fake provider: 429 then success (retry), persistent 500 (fallback), slow response (timeout then fallback), stream broken before first byte (transparent failover), stream broken mid-way (error chunk, usage recorded), client disconnect (upstream cancelled, `client_cancelled`), hedge (loser cancelled), budget and rate limit rejections, cache hit/miss/semantic hit | `testcontainers-go` for Postgres, Redis, pgvector |
| Run engine | Create run, scripted model tool-calls, tool webhook, approval pause and resume, sleep and wake, limit trips, cancel, resume after simulated lease expiry, fencing (zombie write rejected) | Same containers, in-process workers with a controllable clock where possible |
| Roles | Every admin write endpoint returns 403 for a viewer; viewer responses contain no secrets | Table of endpoints × roles, generated from the router |
| Chaos (CI) | See below | Real subprocesses |
| Load | k6: direct-to-fake-provider baseline vs through Spillway with identical fixed provider latency; report p50/p95/p99 added latency and RPS on one small instance (state CPU/RAM) | `bench/` |
| Cache replay | Replay a recorded workload twice (exact, then semantic); report hit rate and dollars saved from `usage.saved_usd` | `bench/` |
| UI smoke | One basic check per screen: the page loads, its key element shows, a viewer's controls are disabled. The API enforces roles regardless (see Roles). | A small Playwright or fetch script, optional, skipped if slow. No extensive browser suite. |
| Graph derivation | `/admin/runs/{id}/graph` from fixture step lists: no recovery, one recovery, two recoveries, fallback attempts on a model call, a running step; `layoutGraph` snapshots in `web/` | Go table tests; Vitest |
| Fault injection | A playground request with two injected faults returns the attempts in order, marks them `injected`, leaves breaker state and metrics untouched, and is rejected for a viewer, for a non-playground key, and via `/v1/chat/completions` | Integration, fake provider |

### Chaos test

Full size (50 runs, up to 3 minutes) runs **in CI only**. Locally it runs reduced with `CHAOS_RUNS=10` and a 60 second cap, and is skipped and reported if it cannot finish in that time.

1. Start Postgres, the fake provider server, and a **tool effects server** that records every webhook call into a ledger `(idempotency_key, received_at)` and applies each side effect only the first time a key is seen.
2. Start `spillway --role=api` and `spillway --role=worker` as subprocesses with `lease_ttl=3s`, `heartbeat=1s`.
3. Submit 50 runs, each scripted as 8 steps alternating model and tool calls, with a side-effecting tool.
4. A killer goroutine `SIGKILL`s the worker at random intervals (1–4 s) and restarts it, for the duration of the test.
5. Wait until all runs are terminal (timeout 3 minutes).
6. Assert: all 50 `succeeded`; for each run the effects ledger holds exactly one *applied* effect per tool step and the set of applied keys equals the set of `finished` tool steps; no `started`-only step remains; every step that was interrupted by a kill has a `reissued` row; step numbers are contiguous.
7. Report (do not hide) the number of **duplicate deliveries** the effects server received. A re-issued step is *expected* to arrive twice with the same key; exactly-once is achieved at the receiver by honouring the key. The result line is "N runs, 0 duplicated side effects, D redelivered requests deduplicated by key".

## 10. Development plan, by phase

The plan is a sequence of **feature phases**, not days. Take as long as each needs.

### How every phase works

1. **Backend tasks first** (labelled BE), each written **together with its tests**, not after. They must pass before any frontend task starts.
2. **Frontend tasks next** (labelled FE), against the real backend from step 1, never against mock data, again with their tests.
3. **Self-check, kept cheap.** Before asking you anything, Claude runs the fast checks: Go unit tests with the race detector for everything built so far, this phase's integration tests, web unit tests (Vitest), type-check and lint. For a phase with a screen it also does one basic smoke check: the page loads and its key element shows, and a viewer's controls are disabled. That is one small script or a page load against the running stack, not a browser test suite. Claude then spot-checks the phase's "Verify with me" list where that is cheap. **Time limit:** any test or check that takes more than about 60 seconds is skipped, reported as skipped with how long it ran, and left to CI or to you. Long runs are not retried and full suites are not re-run just to re-confirm. Results are reported as they are: a failure, a skip or anything not run is stated, and nothing is described as working unless it was run.
4. **CI green** for the phase, with all earlier phases' tests still passing.
5. **Stop and ask you to verify.** Claude posts a "Verify phase N" message with: what was built (backend and frontend), what the self-check ran and its results, how to run it, a checklist of backend checks and frontend checks with the result to expect, and anything known to be missing. Claude then waits.
6. **You reply** with "go", or with what to fix. Fixes are made, re-tested and re-verified. The next phase does not start until you say go.

A phase with no frontend says so and says which later phase gives the feature its first screen. Those phases are verified through the API, the CLI and the tests instead.

**Test rules for every phase**
- Tests live in the same task as the code they cover. A task is not done without them.
- Logic with branches or state (routing policies, limiter, breaker, state machine, lease and replay logic, graph derivation, `layoutGraph`) gets table-driven unit tests, including the failure cases.
- Anything that talks to Postgres, Redis or a provider gets an integration test against real containers and the fake provider, not mocks.
- The admin API is described in one `openapi.yaml`. TypeScript types for `web/` are generated from it, and a contract test checks the real responses against the schema, so the frontend and backend cannot drift apart.
- Every screen gets Vitest tests for its pure logic and components, and an accessibility check with `vitest-axe`, which runs inside Vitest with no browser. Browser tests are limited to **one basic smoke check per screen** (the page loads, its key element shows, a viewer's controls are disabled), and only if it runs fast. No extensive browser or end-to-end suites.
- A token-drift test compares the generated `tokens.css` to `docs/design/tokens.json` with a plain script, no browser.
- Tests must be fast. Unit tests run in seconds and a phase's integration tests should stay under about a minute. Slow tests (the full chaos test, load tests) run in CI, with a reduced version locally. A test that runs too long is skipped and reported, not waited on.
- Automated tests can show that a screen is structurally right, accessible and uses the right tokens. They cannot judge whether it looks like the design. That judgment stays with you in the verify step.

### Phases at a glance

| Phase | Feature | Backend | Frontend | Needs |
|---|---|---|---|---|
| 0 | Bootstrap | yes | no | none |
| 1 | Gateway passthrough | yes | no (first screen: Playground, phase 7) | 0 |
| 2 | Routing and reliability | yes | no (first screen: Playground, phase 7) | 1 |
| 3 | Limits, budgets, caches, telemetry | yes | no (first screen: Usage, phase 6) | 2 |
| 4 | Login, roles, dashboard shell | yes | yes | 3 |
| 5 | API keys | yes | yes | 4 |
| 6 | Usage and cost | yes | yes | 5 |
| 7 | Playground and fault injection | yes | yes | 6 |
| 8 | Run engine | yes | no (first screen: Runs, phase 10) | 3 |
| 9 | Tools, run events, chaos test | yes | no (first screen: Run graph, phase 11) | 8 |
| 10 | Runs page | yes | yes | 9 and 4 |
| 11 | Run graph and timeline | yes | yes | 10 |
| 12 | Approvals, MCP, sleep, compaction | yes | yes | 11 |
| 13 | Deploy, evidence, README | yes | yes | 12 |

**Order.** Phases 4 to 7 (the dashboard and the gateway screens) and phases 8 and 9 (the run engine) are independent once Phase 3 is done. The order above shows the dashboard first, so you see a frontend early. If you would rather prove durability first, run 8 and 9 before 4 to 7; nothing else changes.

**Gates.**
- **Gate A, after Phase 3:** the gateway is complete and measured. If scope is too big, trim here: shrink semantic-cache polish (fixed threshold, no tuning), not runs.
- **Gate B, after Phase 9:** the chaos test passes 50 of 50 in CI, and the reduced version passes locally. Nothing after it starts until it does, because everything after it assumes durability is real.

### Phase 0: bootstrap
*Backend only.*
- [ ] **BE** `git init`, `LICENSE` (Apache-2.0), `go mod init github.com/<handle>/spillway`, directory layout from the spec.
- [ ] **BE** `docker-compose.yml` with Postgres (pgvector image) and Redis; `Makefile` targets: `up`, `migrate`, `test`, `lint`, `sqlc`.
- [ ] **BE** CI workflow: `go vet`, `staticcheck`, unit tests, integration tests (Postgres and Redis service containers).
- [ ] **BE** `cmd/spillway` skeleton: flag parsing, config loading, slog, `/healthz`.
- [ ] **Tests:** a smoke test boots the binary against the compose Postgres and Redis and asserts `/healthz`; lint and vet run in CI.

**Verify with me**
- Backend: `make up && make test` passes on an empty project; `curl localhost:8080/healthz` returns ok; CI is green on a pushed branch.
- Frontend: none yet.

### Phase 1: gateway passthrough
*Backend only. First screen: Playground, Phase 7.*
- [ ] **BE** Migration 001: `api_keys`, `usage`, `usage_daily`. `spillway keys create|list|revoke`.
- [ ] **BE** Catalog loader (`models.yaml`), policy resolution for `fixed` only.
- [ ] **BE** `Provider` interface, typed errors, **OpenAI** and **Anthropic** adapters (chat, streaming, tool calls).
- [ ] **BE** `/v1/chat/completions` with auth, non-streaming and SSE streaming, `/v1/models`, response headers.
- [ ] **BE** Usage recording with a buffered writer; cost from the catalog.
- [ ] **BE** `fake` provider package and `cmd/fakeprovider`.
- [ ] **Tests:** a shared conformance suite that every `Provider` adapter must pass (chat, stream, tool calls, error mapping); adapter fixture tests; cost calculation; usage writer flush on shutdown; auth for unknown, revoked and missing keys; golden request and response tests that match what the OpenAI SDK expects; client disconnect cancels the upstream request.

**Verify with me**
- Backend: create a key with the CLI; point the official OpenAI SDK at Spillway and run a streaming and a non-streaming chat against both providers; confirm a `usage` row with tokens and cost exists for each; disconnect a stream mid-way and confirm the upstream request is cancelled.
- Frontend: none yet.

### Phase 2: routing and reliability
*Backend only. First screen: Playground, Phase 7.*
- [ ] **BE** Policies: `fallback`, `cheapest`, `weighted`; hedging for non-streaming.
- [ ] **BE** Executor: per-attempt timeouts, retries with jitter and `Retry-After`, a breaker per provider, the fallback chain, a request-level deadline.
- [ ] **BE** Mid-stream failure semantics (section 3.5) and error shapes (3.8).
- [ ] **BE** **Google** and **Ollama** adapters.
- [ ] **BE** Attempts recorded in `usage.attempts`, with an `injected` flag reserved for Phase 7.
- [ ] **Tests:** each policy's candidate order; backoff bounds; the breaker's transition table; one integration test per row of the streaming table; a property test that the executor never exceeds the request-level deadline; the whole suite under `-race`.

**Verify with me**
- Backend: run the scripted scenarios against the fake provider and read the `usage.attempts` rows: 429 then retry; persistent 500 then fallback to the second provider; the breaker opening and skipping a dead provider; a hedge beating a slow first attempt; a stream broken mid-way ending with the error chunk and recorded usage.
- Frontend: none yet.

### Phase 3: limits, budgets, caches, telemetry
*Backend only. First screen: Usage, Phase 6.*
- [ ] **BE** Redis token bucket (Lua) and the exact cache; behaviour when Redis is absent.
- [ ] **BE** Budget enforcement from `usage_daily`; the `402` and `429` responses.
- [ ] **BE** `semantic_cache` migration; Ollama embeddings; threshold lookup; asynchronous fill.
- [ ] **BE** Prometheus metrics including the overhead histogram; OpenTelemetry gateway spans.
- [ ] **Tests:** the Lua token bucket against real Redis; cache key canonicalisation; semantic threshold edges; budget boundary (exactly at, just under, just over); Redis down; a benchmark for added latency that fails if it regresses past a set budget.

**Verify with me**
- Backend: replay a small workload twice and see exact hits on the second pass with `saved_usd` recorded; send a paraphrase and see a semantic hit; push a key over its budget and see `402`; exceed its rate limit and see `429` with `Retry-After`; stop Redis and confirm the service still starts; open `/metrics` and see the overhead histogram.
- Frontend: none yet.
- **Gate A.**

### Phase 4: login, roles, dashboard shell
- [ ] **BE** `users` migration, `spillway seed`, `POST /admin/login`, signed session tokens, role middleware, `GET /admin/me`.
- [ ] **BE** Login rate limiting; role table test generated from the router.
- [ ] **BE tests:** login success and failure, constant-time comparison, token tamper and expiry, throttling, the role matrix.
- [ ] **Contract:** `openapi.yaml` for `/admin/login` and `/admin/me`, generated types, and a response-shape test.
- [ ] **FE** Scaffold `web/` (Next.js App Router, TypeScript). Generate `web/src/styles/tokens.css` from `docs/design/tokens.json` with `npm run tokens`.
- [ ] **FE** Theme switch (`data-theme`), stored in a cookie, defaulting to the system preference.
- [ ] **FE** Login page and route handler, session cookie, API client that forwards the Bearer token, `useRole`, `RoleGate`.
- [ ] **FE** Shell: sidebar, system status, user chip. Shared components: `StatusBadge`, `StatTile`, `Panel`, button and input styles.
- [ ] **FE** CI job for the web build, type-check and lint.
- [ ] **FE tests:** Vitest for the API client, `useRole`, `RoleGate` and the theme switch; `vitest-axe` on the login page and shell; the token-drift script; one basic smoke check that the login page loads and a wrong password shows the error.

**Verify with me**
- Backend: `curl` login with the admin and the viewer seeds and read the role from `/admin/me`; a viewer token gets `403` on a write; five bad logins in a minute are throttled.
- Frontend: open the app; log in as admin and as viewer; see the role in the user chip; toggle dark and light and see the title typeface, accent and button size change as designed; a wrong password shows the error message; sign out; a page behind login redirects when signed out.

### Phase 5: API keys
- [ ] **BE** `GET /admin/keys` (both roles, never the full key), `POST /admin/keys` (full key returned once), `PATCH` limits and budgets, `DELETE` (revoke). Spend this month on each row.
- [ ] **BE tests:** key lifecycle (create, shown once, edit, revoke, revoked key rejected by the gateway), spend per key, viewer `403`.
- [ ] **FE** API keys screen: table with budget bars and over-budget and close-to-limit states, the one-time key reveal panel, create and edit form, revoke with confirmation, the viewer's read-only state.
- [ ] **FE tests:** Vitest for the key table states (normal, close to limit, over budget, revoked) and form validation; `vitest-axe`; one basic smoke check that the screen loads and a viewer's controls are disabled.

**Verify with me**
- Backend: create a key over HTTP; use it on `/v1/chat/completions`; revoke it and see `401`; check spend appears on the key.
- Frontend: create a key in the UI and copy it from the reveal panel; call the gateway with it; watch the spend bar move; edit its budget; revoke it; sign in as viewer and confirm every control is disabled and no key value is visible; check both themes.

### Phase 6: usage and cost
- [ ] **BE** `GET /admin/usage/summary` (totals; group by day, model, key), `GET /admin/usage/requests` (paged, with attempts), cache outcome split, provider health (breaker state and fallback counts), added-latency percentiles from the overhead histogram, CSV export.
- [ ] **BE tests:** aggregation against a seeded workload with known totals; day boundaries in UTC; empty ranges; group by key and model; CSV content.
- [ ] **FE** Usage and cost screen: KPI row, spend-per-day chart with hover values, cache outcomes bar with the striped semantic-hit segment, provider health pills, the by-model table, the range selector, the key filter, CSV export.
- [ ] **FE tests:** Vitest for chart data mapping and that the table totals equal the bar totals; `vitest-axe`; the chart has a legend and a table view; one basic smoke check that the screen loads.

**Verify with me**
- Backend: replay the Phase 3 workload and compare the summary totals with a SQL `SUM` over `usage`.
- Frontend: open Usage and check that the KPIs match; hover a bar; switch the range and the key filter; trigger a provider failure and see the breaker pill and fallback count change; export CSV; view in both themes; confirm the chart has a legend and the table matches the bars.

### Phase 7: playground and fault injection
Two slices, each backend then frontend.

**7a. Playground**
- [ ] **BE** `POST /admin/playground/chat` through the normal gateway path under the built-in `playground` key, `GET /admin/playground/history`; admin only for sending.
- [ ] **FE** Playground screen: policy cards, the route trace and timing bar, the answer and facts line, the prompt bar with Options, the recent-requests rail, the viewer's disabled state.

**7b. Fault injection**
- [ ] **BE** `faults: [{provider, kind}]` on the playground request (section 6.4): per-request wrapper, `injected` flag, kept out of breaker state and metrics, allowed only for the playground key and the admin role.
- [ ] **FE** Fault chips in the prompt bar wired to the request; the route trace shows injected failures.
- [ ] **BE tests:** playground request through the gateway path; history; the fault wrapper in isolation; with faults, attempts come back in order and are marked `injected`; breaker state and metrics are untouched; faults rejected for a viewer, another key and `/v1/chat/completions`.
- [ ] **FE tests:** Vitest rendering the route trace from attempt fixtures (success, one failure, two failures, cut stream); the viewer's disabled state; `vitest-axe`; one basic smoke check that the screen loads.

**Verify with me**
- Backend: call the playground endpoint with two faults and read the attempts; confirm breaker state did not change; confirm `/v1/chat/completions` ignores fault input.
- Frontend: send a prompt; switch on "Anthropic returns 429" and "OpenAI returns 503", send again, and watch the route trace show two failures then the answer; try "Cut the stream halfway"; sign in as viewer and confirm the prompt bar is disabled and history is readable; both themes.

### Phase 8: run engine
*Backend only. First screen: Runs, Phase 10. Verified through the CLI and the API.*
- [x] **BE** Migrations: `runs`, `run_steps` (with `worker_id` and the `reissued` phase), `tools`; encryption helper for `headers_enc`.
- [x] **BE** Run store: append with fencing, projection update, idempotent create. `POST /v1/runs`, `GET /v1/runs/{id}`, `GET /v1/runs/{id}/steps`.
- [x] **BE** Worker pool: claim, heartbeat, release on shutdown, limits check, replay and frontier logic, `reissued` rows, and the model-call step through the in-process gateway path.
- [x] **BE** CLI for checking by hand: `spillway runs create|get|steps|cancel`.
- [ ] **BE tests:** replay on synthetic step lists as property tests (any prefix of a valid run resumes to the same result); every state transition; fencing (a zombie worker's write is rejected); lease expiry with a fake clock; a step reissued twice; every limit trip; the CLI commands.

**Verify with me**
- Backend: create a run of model calls with the CLI and watch it complete; start it again, `kill -9` the worker mid-run, and see another worker finish it; read the steps and find the `reissued` row with a different `worker_id` and a higher epoch; a run hitting each limit fails with the right reason.
- Frontend: none yet.

### Phase 9: tools, run events, chaos test
*Backend only. First screen: Run graph, Phase 11.*
- [x] **BE** HTTP tool executor with idempotency key and signature; tool-call steps; the tool error feedback loop.
- [x] **BE** Run events: `NOTIFY` and `LISTEN`, SSE with `Last-Event-ID` replay.
- [x] **BE** `--role=api|worker|all`; the tool effects server and the chaos test harness.
- [ ] **BE tests:** tool executor (signature, timeout, size truncation, idempotency key sent); SSE cursor replay and reconnect; role flags; the chaos test run with `-race` and kept in CI.

**Verify with me**
- Backend: run the chaos test locally and see 50 of 50 succeed with zero duplicated applied side effects and a count of redelivered requests; `curl` the events stream of a live run, drop the connection, reconnect with `Last-Event-ID` and see it resume; run `--role=api` and `--role=worker` as separate processes.
- Frontend: none yet.
- **Gate B.**

### Phase 10: runs page
- [x] **BE** `GET /admin/runs/summary` (counts and runs waiting for a person), `GET /admin/runs/activity`, `GET /admin/runs` with step strips and a keyset cursor (section 6.3).
- [ ] **BE tests:** summary, activity and list endpoints against fixture runs in every status; keyset pagination; step strip capping.
- [x] **FE** Runs page: counters, activity bars, the Needs you card, Running now cards with step strips, the Earlier list, time-range control, live updates, and the viewer state.
- [ ] **FE tests:** Vitest for the strip mapping, counters and the live-update reducer; `vitest-axe`; one basic smoke check that the page loads and shows the counters.

**Verify with me**
- Backend: start several runs against the scripted fake provider and compare `/admin/runs/summary` with the database.
- Frontend: open Runs while a handful of runs execute and watch cards appear, strips grow and runs move to Earlier; the Needs you card appears for a run that needs approval (its Review button opens the run page; the approve controls arrive in Phase 12); both themes; viewer.

### Phase 11: run graph and timeline
- [x] **BE** `GET /admin/runs/{id}/graph` (section 6.3), the admin events endpoint, cancel.
- [ ] **BE tests:** graph derivation from fixtures: no recovery, one, two recoveries, fallback attempts, a running step, a step reissued twice.
- [x] **FE** `layoutGraph()` with unit tests first, then `RunGraph`, the worker bands and cut line, the step inspector, the Graph and Timeline toggle, live node updates from the SSE stream, and the Cancel run button.
- [ ] **FE tests:** `layoutGraph` unit and snapshot tests for each of those fixtures; the SSE reducer; `vitest-axe`; one basic smoke check that the graph page loads. The kill-the-worker demo is checked by you in the verify step, not by a browser test.

**Verify with me**
- Backend: fetch the graph for a run that was killed mid-step and check the stopped node, the re-issued node and the recovery entry.
- Frontend: start a run, `kill -9` the worker while it runs, and watch the graph show the dashed cut, the w-2 to w-4 bands and the re-issued node; select a node and read the inspector; switch to Timeline; cancel a run; both themes. This is the crash-recovery demo.

### Phase 12: approvals, MCP, sleep, compaction
- [x] **BE** Tool registry endpoints and a CLI (`spillway tools add|list|discover`); MCP streamable-HTTP client with `discover`; MCP tool steps; allowlist enforcement.
- [x] **BE** `wait_human` (built-in tool and `requires_approval`), approve, reject and cancel endpoints; the `sleep` built-in and the wake path; compaction step and replay support.
- [x] **BE tests:** approval pause and resume, reject path, approve while the worker is dead, sleep and wake, MCP client against a test MCP server, allowlist enforcement, compaction replay gives the same history; the chaos test extended and still passing.
- [x] **FE** Approval card in the run view with the email-style preview, note field, and the viewer's disabled state; the Sleeping state in the Runs page and graph.
- [x] **FE tests:** Vitest for the approval card states and the viewer gate; `vitest-axe`; one basic smoke check that the approval card shows and a viewer's buttons are disabled.

**Verify with me**
- Backend: register an MCP server with the CLI; run a task that calls one of its tools and needs approval; approve over the API and watch it finish; crash a worker during the pause and confirm it still resumes; a long run compacts its history.
- Frontend: watch a run stop at the approval card in the dashboard, approve it, and see it continue in the graph; reject another and see it end with the reason; confirm a viewer sees the card but cannot decide; check the Needs you card on the Runs page; both themes.
- **Open point.** The final design has no screen for the tool registry. This phase manages tools through the API and CLI. If you want a Tools screen, it needs a design first.

### Phase 13: evidence and README (deployment deferred)
- [ ] **BE** `Dockerfile` for the service and for `web/`; compose finalised; `docker compose up` quick start. **Deferred by decision: no deployment for now.**
- [x] **BE** Load test and cache replay with measured numbers (`bench/`, `bench/results.md`), machine stated. Written as a small Go program instead of k6, which is not installed here; it measures the same things.
- [x] **FE** Crash-recovery demo recorded as a GIF from a replay of a real recovered run (`docs/img/crash-recovery.gif`), plus a terminal demo (`scripts/demo.sh`, `make demo`) with its own database and ports.
- [x] README per the spec's checklist, including Known limits, with screenshots of every screen that matters in both themes where it differs.
- [x] **Tests:** the Phase 12 chaos test is the evidence for crash safety; the reduced run is recorded in the README.
- [ ] A CI job that does a clean-checkout quick start. Deferred with deployment.

**Verify with me**
- Run `make up` then `scripts/demo.sh` and read the output; open the README and check the screenshots and the results table against `bench/results.md`.

### Cross-cutting checklist before calling it done
- [ ] No secrets in logs or admin responses; a `gitleaks`-style scan is clean.
- [ ] `go test -race ./...` is clean.
- [ ] Shutdown tested: SIGTERM mid-run releases leases and another worker resumes immediately.
- [ ] Startup without Redis tested and documented.
- [ ] Metrics scraped, with a sample query shown in the README.
- [ ] Playground fault injection verified to be unreachable from `/v1/chat/completions` and from the viewer role.
- [ ] Every screen in the design artifact exists in both themes and both roles.

## 11. Deviations from and clarifications to the spec

These are the places where this document makes a call the spec left open or phrased differently. They need your confirmation:

1. **Admin auth ownership.** The spec says the role comes from the dashboard's session layer. Here the Go service issues and verifies the signed session token, so a compromised or buggy web app cannot claim to be an admin. The Next.js app only stores and forwards the token.
2. **Playground is admin-only.** The spec says viewers can see "the playground's results". Because a playground prompt spends money, viewers get read-only *history* and cannot send prompts. If you want viewers to be able to run prompts, give the viewer a small separate budget instead.
3. **`run_status` rows live in `run_steps`.** This keeps one cursor for the event stream and keeps state derivable from the append-only log, at the cost of a nullable `step_no`.
4. **Budgets are approximate**, not hard caps, because spend is checked before the call and recorded after it. Stated under Known limits.
5. **Streams cannot fail over after the first byte.** The client gets an error chunk instead. This is a limit of the streaming protocol and is stated in the README.
6. **Model calls inside runs are non-streaming upstream.** The dashboard timeline shows whole steps appearing, not token-by-token text. Token streaming for runs is a possible later addition.
7. **Cache scope defaults to per API key** to avoid leaking one tenant's completions to another; `cache.scope: global` opts into sharing.
8. **Re-issued steps get their own `reissued` row**, and `run_steps` gains a `worker_id` column. The original unique constraint on `(run_id, step_no, phase)` is replaced by one per lease epoch plus a unique index on terminal rows. This is what lets the run graph show the stopped attempt and the recovery, and it keeps fencing intact.
9. **Playground fault injection is a new feature** not in the spec: an admin-only, per-request, playground-key-only way to make a provider fail, with injected failures kept out of breaker state and metrics (section 6.4).
10. **The dashboard has two token sets, not one themed palette.** The dark theme follows the Linear design file and the light theme follows the Claude design file, as decided with the design. Title typeface, accent and button size therefore change with the theme.
11. **The Runs page and run view are bigger than the spec's "list and timeline".** They add a needs-approval card, an activity chart, per-run step strips and a graph view, which require the read endpoints in section 6.3.
12. **The plan is phased, not dated, and organised by feature.** Each phase builds the backend first, then the frontend, then stops for you to verify both (section 10). The dashboard shell and login come at Phase 4 so you see a frontend early, instead of building all the backend first.
14. **Runs gain a `cancel_requested_at` column and a `limits` column (Phase 8).** A cancel for a run a live worker holds cannot be written by the API (the fence would refuse it), so the API marks the run and the lease holder sees the mark on its next heartbeat. `limits` stores the limits in force (the request's, capped by the server's) so they survive a config change.
15. **`failure_reason` has one value beyond the spec's list: `key_revoked`**, for a run whose API key was revoked mid-run. The column has no check constraint, so no migration is needed.
16. **`POST /v1/runs` refuses `tools` until the tool registry exists** (Phase 12), instead of accepting names it cannot offer. The engine already runs tool steps behind a `ToolRunner` interface, tested with a stub; Phase 9 supplies the HTTP executor. Until then a run is a single model call, so only the deadline can trip from the CLI; the other limits are covered by the engine tests.
17. **The `spillway runs` CLI is an HTTP client** (`--url`, `--key`), so it exercises the same path and the same key-scoped visibility as any API client, rather than reading the database.
18. **`waiting_tool` is a projection of "a tool call is open"**, set by the tool step's rows without a separate `run_status` row, so a tool step does not double its rows.
19. **Tool signing secrets (Phase 9).** Calls are signed with `SPILLWAY_WEBHOOK_SECRET`; a tool may carry its own secret as an `X-Spillway-Signing-Secret` entry among its stored headers, which signs and is never sent. With no secret at all a call is unsigned. Redirects from a tool are not followed, so a registered endpoint cannot bounce a call to another host.
20. **The registry had a store but no endpoints until Phase 12** (it has them now, see 36), so Phase 9 tests register tools through `tools.Store`. `POST /v1/runs` accepts `tools` and refuses names that are not registered.
21. **The chaos test is gated by the environment.** It runs in full when `CI` is set, reduced when `CHAOS_RUNS` is set (60 second cap, skipped and reported if exceeded), and is skipped otherwise. Its runs are nine steps (four tool calls, five model calls), not eight, so that a run ends on a model answer.
22. **The Runs page polls every 2 seconds instead of streaming (Phase 10).** The per-run event stream is for one run, and a page of many runs would need a stream of all of them. Polling one snapshot is simpler and keeps the page correct after any pause; it stops while the tab is hidden. The run page (Phase 11) uses the SSE stream.
23. **A strip shows a run's latest 16 steps**, with `+N` for the earlier ones in front, rather than the first 16 with the rest behind. For a long live run the step that matters is the current one.
24. **The activity chart has a fourth colour for cancelled runs**, so every run started in a slice is drawn.
25. **The dashboard can start and cancel runs (added after Phase 10).** `POST /admin/runs` and `POST /admin/runs/{id}/cancel` are admin-only. A person at the dashboard has no API key secret, so a run started there is made under the built-in playground key and counts against its budget and rate limit; cancel works on any run. The New run form offers registered tools since Phase 12 (see 36).
26. **The Runs page filters on the server and sorts what is loaded.** Search (case-insensitive, on the task text), status and API key narrow the lists in the Go service and page with a keyset cursor; the order (longest, most expensive, most steps) applies to the runs already loaded, and the page says so, because keyset paging needs one fixed order.
27. **The run page reads the graph again when a row arrives, rather than applying rows to a client store (Phase 11).** The Go service derives the graph, so the browser never re-implements the derivation rules; each stream event only says the graph is stale. Rows are de-duplicated by id, the stream resumes from Last-Event-ID, and the page falls back to polling every 3 s if the browser gives up on the connection.
28. **`GET /admin/runs/{id}/events` is the admin twin of the public stream**, open to both roles because it only reads. Node details (`arguments`, `result`, `message`, `error`) are cut to 2,000 characters in the graph payload.
29. **The run page has a live stage (added after Phase 11).** A three.js scene above the graph shows the run as spinning crystals (model calls) and cubes (tool calls) with a packet following the run, a flash where a worker took over, a replay with a scrubber, and a celebration when a run ends in front of you. The graph, timeline and inspector below stay the accessible account of the same run; the scene is decoration, and without WebGL the page says so and shows the graph. Beside them are a worker panel and an activity feed derived from the same graph payload.
30. **The lower half of the run page is richer than the design file (added after Phase 11).** The graph can be zoomed and dragged and its running edge flows; a "Where the time went" waterfall lays every attempt along time (a lost worker's attempt runs until the next began, and idle stretches are hatched); the inspector has tabs (overview with shares of time, cost and tokens, input, output with copy, providers with per-attempt latency bars, attempts) and previous and next; the timeline filters by kind or problems and folds long output. All of it is derived from the same graph payload.
31. **A parked run is held by nobody (Phase 12).** A run that sleeps or waits for a person writes its `started` row and the worker releases the lease; status becomes `sleeping` (with `wake_at`) or `waiting_human`. A sleeping run is claimable only once `wake_at` has passed. A waiting run is not claimable at all until a decision exists. Approving writes the `wait_human` finished row from the API (worker `api`, at the run's current epoch) and makes the run claimable, so it resumes on any worker, including one that starts after the worker that parked it has died. Rejecting writes the finished row and `run_status failed` with reason `rejected`. Cancelling a parked run cancels it on the spot. Time spent waiting for a person does not count against the deadline: approving moves `deadline_at` on by the wait. A run that is never decided stays waiting past its deadline until someone decides or cancels it.
32. **Built-in tools are on every run.** `sleep(seconds)` (1 to 86400, and not past the run's deadline) and `request_human_approval(reason)` are offered to the model whether or not the request lists tools, and their names are reserved in the registry. A bad `sleep` argument is fed back to the model as the call's result, like any tool error.
33. **A tool in `approval_required` waits before each call.** A tool registered with `requires_approval` always does. The gate is a `wait_human` step before the tool step; the approval is remembered by tool call id, so a crash after the decision and before the tool ran does not ask again. `approval_required` must be a subset of `tools`.
34. **MCP tools are named `server.tool`**, and `server__tool` to the model (some providers refuse a dot). A server is registered once as kind `mcp`; `discover` stores its `tools/list` answer on the row, and only discovered tools can be named in a run. An MCP server's `requires_approval` applies to all its tools. The client opens a session per endpoint, reuses it, and opens a fresh one when the server forgets it; answers may be JSON or SSE. The idempotency key goes in `_meta["spillway/idempotencyKey"]` and in an `Idempotency-Key` header, and, as for HTTP tools, exactly-once rests on the server honouring it.
35. **Compaction keeps what the caller gave.** The system prompt and the input are never folded. When the estimated history (characters divided by four) passes `compaction.threshold_tokens` (default 24000), everything between them and the last `keep_last_turns` model turns (default 6) is summarised by a model call through the gateway (policy `runs.compaction_policy`, else the run's own). The summary is a step with its own cost, so replay rebuilds the same history. A failed compaction is recorded and not retried by that worker, and the run goes on uncompacted. The estimate is a heuristic, not a tokenizer.
36. **Tool registry endpoints and CLI.** `GET /admin/tools` (viewer) returns header names, never values; `POST`, `PUT`, `DELETE /admin/tools` and `POST /admin/tools/{id}/discover` are admin only. `spillway tools add|list|discover|rm` works on the database directly, like `keys`, and needs `SPILLWAY_SECRET_KEY` to store auth headers. The dashboard's New run form lists registered tools and lets an admin mark which ask first.
37. **Approval endpoints.** `POST /v1/runs/{id}/approve|reject` are scoped to the run's own key and record the key's name as the decider; `POST /admin/runs/{id}/approve|reject` are admin only and record the signed-in username. Both take `{note}`; the note is shown to the model on approve, and the dashboard asks for a reason on reject. Only one decision can win: the second gets 409.
13. **The tool registry has no screen** in the final design, so Phase 12 manages tools through the API and CLI. Adding a screen needs a design first.
8. **MCP idempotency** depends on the server honouring `spillway/idempotencyKey`. Exactly-once for side effects is guaranteed only for receivers that deduplicate by key; the chaos test measures that against a receiver that does.
