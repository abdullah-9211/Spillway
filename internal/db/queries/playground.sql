-- name: InsertPlaygroundHistory :exec
INSERT INTO playground_history (id, policy, prompt, system, answer, stream, faults, result, created_at)
VALUES (@id, @policy, @prompt, @system, @answer, @stream, @faults, @result, @created_at);

-- name: ListPlaygroundHistory :many
SELECT id, policy, prompt, system, answer, stream, faults, result, created_at
FROM playground_history
WHERE (sqlc.narg(before_ts)::timestamptz IS NULL OR (created_at, id) < (sqlc.narg(before_ts)::timestamptz, sqlc.narg(before_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT @row_limit;

-- name: GetPlaygroundHistory :one
SELECT id, policy, prompt, system, answer, stream, faults, result, created_at FROM playground_history WHERE id = @id;

-- name: GetAPIKeyByName :one
SELECT id, name, key_prefix, rate_limit_rpm, monthly_budget_usd, semantic_cache, cache_nonzero_temp, created_at, revoked_at
FROM api_keys WHERE name = @name ORDER BY created_at LIMIT 1;

-- name: SetAPIKeyLimits :exec
UPDATE api_keys SET rate_limit_rpm = sqlc.narg(rate_limit_rpm), monthly_budget_usd = sqlc.narg(monthly_budget_usd) WHERE id = @id;
