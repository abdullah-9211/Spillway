-- name: InsertUsage :exec
INSERT INTO usage (
  id, api_key_id, run_id, policy, provider, model, input_tokens, output_tokens,
  cost_usd, saved_usd, latency_ms, ttfb_ms, cache_status, outcome, attempts, created_at
) VALUES (
  @id, @api_key_id, sqlc.narg(run_id), @policy, @provider, @model, @input_tokens, @output_tokens,
  @cost_usd, @saved_usd, @latency_ms, sqlc.narg(ttfb_ms), @cache_status, @outcome, @attempts, @created_at
);

-- name: UpsertUsageDaily :exec
INSERT INTO usage_daily (api_key_id, day, model, requests, input_tokens, output_tokens, cost_usd, saved_usd, cache_hits)
VALUES (@api_key_id, @day, @model, 1, @input_tokens, @output_tokens, @cost_usd, @saved_usd, @cache_hits)
ON CONFLICT (api_key_id, day, model) DO UPDATE SET
  requests      = usage_daily.requests + 1,
  input_tokens  = usage_daily.input_tokens + EXCLUDED.input_tokens,
  output_tokens = usage_daily.output_tokens + EXCLUDED.output_tokens,
  cost_usd      = usage_daily.cost_usd + EXCLUDED.cost_usd,
  saved_usd     = usage_daily.saved_usd + EXCLUDED.saved_usd,
  cache_hits    = usage_daily.cache_hits + EXCLUDED.cache_hits;

-- name: GetUsage :one
SELECT id, api_key_id, run_id, policy, provider, model, input_tokens, output_tokens, cost_usd, saved_usd,
       latency_ms, ttfb_ms, cache_status, outcome, attempts, created_at
FROM usage WHERE id = @id;

-- name: GetUsageDaily :one
SELECT requests, input_tokens, output_tokens, cost_usd, saved_usd, cache_hits
FROM usage_daily WHERE api_key_id = @api_key_id AND day = @day AND model = @model;

-- name: SumUsageDailySince :one
SELECT COALESCE(SUM(cost_usd), 0)::numeric AS cost_usd
FROM usage_daily
WHERE api_key_id = @api_key_id AND day >= @since;
