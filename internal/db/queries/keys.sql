-- Money is numeric(14,8) in the database. internal/db converts to and from micro-dollars (int64).

-- name: CreateAPIKey :one
INSERT INTO api_keys (id, name, key_prefix, key_hash, rate_limit_rpm, monthly_budget_usd, semantic_cache, cache_nonzero_temp)
VALUES (@id, @name, @key_prefix, @key_hash, sqlc.narg(rate_limit_rpm), sqlc.narg(monthly_budget_usd), @semantic_cache, @cache_nonzero_temp)
RETURNING created_at;

-- name: ListAPIKeys :many
SELECT id, name, key_prefix, rate_limit_rpm, monthly_budget_usd, semantic_cache, cache_nonzero_temp, created_at, revoked_at
FROM api_keys
ORDER BY created_at DESC, id DESC;

-- name: GetActiveAPIKeyByHash :one
SELECT id, name, key_prefix, rate_limit_rpm, monthly_budget_usd, semantic_cache, cache_nonzero_temp, created_at, revoked_at
FROM api_keys
WHERE key_hash = @key_hash AND revoked_at IS NULL;

-- name: RevokeAPIKey :one
UPDATE api_keys SET revoked_at = now() WHERE id = @id AND revoked_at IS NULL RETURNING id;

-- name: SetSemanticCache :one
UPDATE api_keys SET semantic_cache = @semantic_cache WHERE key_hash = @key_hash RETURNING id;

-- name: ListAPIKeysWithStats :many
SELECT k.id, k.name, k.key_prefix, k.rate_limit_rpm, k.monthly_budget_usd, k.semantic_cache, k.cache_nonzero_temp,
       k.created_at, k.revoked_at,
       COALESCE((SELECT SUM(d.cost_usd) FROM usage_daily d WHERE d.api_key_id = k.id AND d.day >= @since), 0)::numeric AS spend_usd,
       COALESCE((SELECT MAX(u.created_at) FROM usage u WHERE u.api_key_id = k.id), 'epoch'::timestamptz)::timestamptz AS last_used_at -- epoch means never used
FROM api_keys k
ORDER BY (k.revoked_at IS NOT NULL), k.created_at DESC, k.id DESC;

-- name: GetAPIKeyByID :one
SELECT id, name, key_prefix, rate_limit_rpm, monthly_budget_usd, semantic_cache, cache_nonzero_temp, created_at, revoked_at
FROM api_keys WHERE id = @id;

-- name: UpdateAPIKey :one
UPDATE api_keys SET
  name               = COALESCE(sqlc.narg(name), name),
  rate_limit_rpm     = CASE WHEN @set_rpm::bool THEN sqlc.narg(rate_limit_rpm) ELSE rate_limit_rpm END,
  monthly_budget_usd = CASE WHEN @set_budget::bool THEN sqlc.narg(monthly_budget_usd) ELSE monthly_budget_usd END,
  semantic_cache     = COALESCE(sqlc.narg(semantic_cache), semantic_cache),
  cache_nonzero_temp = COALESCE(sqlc.narg(cache_nonzero_temp), cache_nonzero_temp)
WHERE id = @id AND revoked_at IS NULL
RETURNING id;
