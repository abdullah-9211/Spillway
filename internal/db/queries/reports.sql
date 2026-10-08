-- Reports read the per-request `usage` table so that every figure can be checked with a plain SUM over it.
-- The range is [from_ts, to_ts), in UTC; an optional key narrows it.

-- name: ReportTotals :one
SELECT
  count(*)::bigint                                                          AS requests,
  COALESCE(sum(input_tokens), 0)::bigint                                    AS input_tokens,
  COALESCE(sum(output_tokens), 0)::bigint                                   AS output_tokens,
  COALESCE(sum(cost_usd), 0)::numeric                                       AS cost_usd,
  COALESCE(sum(saved_usd), 0)::numeric                                      AS saved_usd,
  count(*) FILTER (WHERE outcome IN ('upstream_error', 'all_providers_failed'))::bigint AS errors,
  count(*) FILTER (WHERE outcome IN ('rate_limited', 'over_budget'))::bigint            AS rejected,
  count(*) FILTER (WHERE cache_status = 'miss')::bigint                     AS cache_miss,
  count(*) FILTER (WHERE cache_status = 'hit_exact')::bigint                AS cache_hit_exact,
  count(*) FILTER (WHERE cache_status = 'hit_semantic')::bigint             AS cache_hit_semantic,
  count(*) FILTER (WHERE cache_status = 'bypass')::bigint                   AS cache_bypass
FROM usage
WHERE created_at >= @from_ts AND created_at < @to_ts
  AND (sqlc.narg(key_id)::uuid IS NULL OR api_key_id = sqlc.narg(key_id)::uuid);

-- name: ReportByDayModel :many
SELECT
  (created_at AT TIME ZONE 'UTC')::date                AS day,
  COALESCE(model, '')::text                            AS model,
  count(*)::bigint                                     AS requests,
  COALESCE(sum(input_tokens), 0)::bigint               AS input_tokens,
  COALESCE(sum(output_tokens), 0)::bigint              AS output_tokens,
  COALESCE(sum(cost_usd), 0)::numeric                  AS cost_usd,
  COALESCE(sum(saved_usd), 0)::numeric                 AS saved_usd,
  count(*) FILTER (WHERE cache_status IN ('hit_exact', 'hit_semantic'))::bigint AS cache_hits,
  count(*) FILTER (WHERE outcome IN ('upstream_error', 'all_providers_failed'))::bigint AS errors
FROM usage
WHERE created_at >= @from_ts AND created_at < @to_ts
  AND (sqlc.narg(key_id)::uuid IS NULL OR api_key_id = sqlc.narg(key_id)::uuid)
GROUP BY 1, 2
ORDER BY 1, 2;

-- name: ReportByModel :many
SELECT
  COALESCE(model, '')::text                            AS model,
  count(*)::bigint                                     AS requests,
  COALESCE(sum(input_tokens), 0)::bigint               AS input_tokens,
  COALESCE(sum(output_tokens), 0)::bigint              AS output_tokens,
  COALESCE(sum(cost_usd), 0)::numeric                  AS cost_usd,
  COALESCE(sum(saved_usd), 0)::numeric                 AS saved_usd,
  count(*) FILTER (WHERE cache_status IN ('hit_exact', 'hit_semantic'))::bigint AS cache_hits,
  count(*) FILTER (WHERE outcome IN ('upstream_error', 'all_providers_failed'))::bigint AS errors,
  -- Latency is judged on requests that really went to a provider and succeeded: a cache hit says nothing about the model.
  count(*) FILTER (WHERE outcome = 'ok' AND cache_status IN ('miss', 'bypass'))::bigint AS timed_requests,
  COALESCE(percentile_cont(0.5)  WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE outcome = 'ok' AND cache_status IN ('miss', 'bypass')), 0)::float8 AS p50_ms,
  COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE outcome = 'ok' AND cache_status IN ('miss', 'bypass')), 0)::float8 AS p95_ms
FROM usage
WHERE created_at >= @from_ts AND created_at < @to_ts
  AND (sqlc.narg(key_id)::uuid IS NULL OR api_key_id = sqlc.narg(key_id)::uuid)
GROUP BY 1
ORDER BY sum(cost_usd) DESC NULLS LAST, count(*) DESC, 1;

-- name: ReportByKey :many
SELECT
  u.api_key_id                                         AS key_id,
  COALESCE(k.name, '(deleted key)')::text              AS key_name,
  count(*)::bigint                                     AS requests,
  COALESCE(sum(u.input_tokens), 0)::bigint             AS input_tokens,
  COALESCE(sum(u.output_tokens), 0)::bigint            AS output_tokens,
  COALESCE(sum(u.cost_usd), 0)::numeric                AS cost_usd,
  COALESCE(sum(u.saved_usd), 0)::numeric               AS saved_usd,
  count(*) FILTER (WHERE u.cache_status IN ('hit_exact', 'hit_semantic'))::bigint AS cache_hits,
  count(*) FILTER (WHERE u.outcome IN ('upstream_error', 'all_providers_failed'))::bigint AS errors
FROM usage u LEFT JOIN api_keys k ON k.id = u.api_key_id
WHERE u.created_at >= @from_ts AND u.created_at < @to_ts
  AND (sqlc.narg(key_id)::uuid IS NULL OR u.api_key_id = sqlc.narg(key_id)::uuid)
GROUP BY u.api_key_id, k.name
ORDER BY sum(u.cost_usd) DESC NULLS LAST, count(*) DESC;

-- name: ReportProviderAttempts :many
-- Failed attempts and fallbacks per provider, read out of the attempts recorded on each request.
SELECT
  COALESCE(a->>'provider', '')::text                   AS provider,
  count(*) FILTER (WHERE COALESCE(a->>'error_kind', '') <> '' AND a->>'kind' <> 'skipped')::bigint AS failed_attempts,
  count(*) FILTER (WHERE a->>'kind' = 'fallback'  )::bigint AS fallbacks_to
FROM usage u, jsonb_array_elements(u.attempts) a
WHERE u.created_at >= @from_ts AND u.created_at < @to_ts
  AND (sqlc.narg(key_id)::uuid IS NULL OR u.api_key_id = sqlc.narg(key_id)::uuid)
  AND NOT (u.attempts @> '[{"injected": true}]')  -- playground requests with faults say nothing about provider health
GROUP BY 1
ORDER BY 1;

-- name: ReportRequests :many
-- Newest first, keyset-paged on (created_at, id).
SELECT u.id, u.api_key_id, COALESCE(k.name, '')::text AS key_name, u.run_id, u.policy, u.provider, u.model,
       u.input_tokens, u.output_tokens, u.cost_usd, u.saved_usd, u.latency_ms, u.ttfb_ms, u.cache_status, u.outcome,
       u.attempts, u.created_at
FROM usage u LEFT JOIN api_keys k ON k.id = u.api_key_id
WHERE u.created_at >= @from_ts AND u.created_at < @to_ts
  AND (sqlc.narg(key_id)::uuid IS NULL OR u.api_key_id = sqlc.narg(key_id)::uuid)
  AND (sqlc.narg(model)::text IS NULL OR u.model = sqlc.narg(model)::text)
  AND (sqlc.narg(outcome)::text IS NULL OR u.outcome = sqlc.narg(outcome)::text)
  AND (sqlc.narg(policy)::text IS NULL OR u.policy = sqlc.narg(policy)::text)
  AND (sqlc.narg(before_ts)::timestamptz IS NULL OR (u.created_at, u.id) < (sqlc.narg(before_ts)::timestamptz, sqlc.narg(before_id)::uuid))
ORDER BY u.created_at DESC, u.id DESC
LIMIT @row_limit;

-- name: ReportCSVRows :many
SELECT
  (u.created_at AT TIME ZONE 'UTC')::date              AS day,
  COALESCE(k.name, '(deleted key)')::text              AS key_name,
  COALESCE(u.model, '')::text                          AS model,
  count(*)::bigint                                     AS requests,
  COALESCE(sum(u.input_tokens), 0)::bigint             AS input_tokens,
  COALESCE(sum(u.output_tokens), 0)::bigint            AS output_tokens,
  COALESCE(sum(u.cost_usd), 0)::numeric                AS cost_usd,
  COALESCE(sum(u.saved_usd), 0)::numeric               AS saved_usd,
  count(*) FILTER (WHERE u.cache_status IN ('hit_exact', 'hit_semantic'))::bigint AS cache_hits,
  count(*) FILTER (WHERE u.outcome IN ('upstream_error', 'all_providers_failed'))::bigint AS errors
FROM usage u LEFT JOIN api_keys k ON k.id = u.api_key_id
WHERE u.created_at >= @from_ts AND u.created_at < @to_ts
  AND (sqlc.narg(key_id)::uuid IS NULL OR u.api_key_id = sqlc.narg(key_id)::uuid)
GROUP BY 1, 2, 3
ORDER BY 1, 2, 3;

-- name: ReportByDayKey :many
SELECT
  (u.created_at AT TIME ZONE 'UTC')::date              AS day,
  u.api_key_id                                         AS key_id,
  COALESCE(k.name, '(deleted key)')::text              AS key_name,
  count(*)::bigint                                     AS requests,
  COALESCE(sum(u.input_tokens), 0)::bigint             AS input_tokens,
  COALESCE(sum(u.output_tokens), 0)::bigint            AS output_tokens,
  COALESCE(sum(u.cost_usd), 0)::numeric                AS cost_usd,
  COALESCE(sum(u.saved_usd), 0)::numeric               AS saved_usd,
  count(*) FILTER (WHERE u.cache_status IN ('hit_exact', 'hit_semantic'))::bigint AS cache_hits,
  count(*) FILTER (WHERE u.outcome IN ('upstream_error', 'all_providers_failed'))::bigint AS errors
FROM usage u LEFT JOIN api_keys k ON k.id = u.api_key_id
WHERE u.created_at >= @from_ts AND u.created_at < @to_ts
  AND (sqlc.narg(key_id)::uuid IS NULL OR u.api_key_id = sqlc.narg(key_id)::uuid)
GROUP BY 1, 2, 3
ORDER BY 1, 2;
