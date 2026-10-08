-- Sample usage for trying the dashboard: 14 days of requests on a key named "demo-data".
-- It is invented data, labelled by that key name. Remove it with `make demo-data-clear`.
-- Prices used: sonnet $3/$15 and mini $0.15/$0.60 per million tokens; local is free.

DELETE FROM usage_daily WHERE api_key_id IN (SELECT id FROM api_keys WHERE name = 'demo-data');
DELETE FROM usage WHERE api_key_id IN (SELECT id FROM api_keys WHERE name = 'demo-data');
DELETE FROM api_keys WHERE name = 'demo-data';

INSERT INTO api_keys (id, name, key_prefix, key_hash, rate_limit_rpm, monthly_budget_usd)
VALUES (gen_random_uuid(), 'demo-data', 'spw_demo', sha256(gen_random_uuid()::text::bytea), 120, 500);

WITH k AS (SELECT id FROM api_keys WHERE name = 'demo-data'),
base AS (
  SELECT d::date AS day, n,
         abs(hashtext(d::text || ':' || n))                AS h1,
         abs(hashtext('b' || d::text || ':' || n))         AS h2,
         abs(hashtext('c' || d::text || ':' || n))         AS h3,
         abs(hashtext('t' || d::text || ':' || n))         AS h4
  FROM generate_series((now() AT TIME ZONE 'UTC')::date - 13, (now() AT TIME ZONE 'UTC')::date, interval '1 day') AS d,
       LATERAL generate_series(1, 60 + (abs(hashtext(d::text)) % 60)) AS n
),
shaped AS (
  SELECT day, n, h4,
         CASE WHEN h1 % 100 < 45 THEN 'sonnet' WHEN h1 % 100 < 85 THEN 'mini' ELSE 'local' END AS model,
         CASE WHEN h2 % 100 < 62 THEN 'miss' WHEN h2 % 100 < 87 THEN 'hit_exact' ELSE 'hit_semantic' END AS cache,
         (h3 % 100) AS roll,
         800 + (h3 % 4000)  AS tin,
         100 + (h4 % 1500)  AS tout,
         day::timestamp AT TIME ZONE 'UTC' + make_interval(secs => (h4 % 86400)) AS at
  FROM base
)
INSERT INTO usage (id, api_key_id, policy, provider, model, input_tokens, output_tokens, cost_usd, saved_usd, latency_ms, ttfb_ms,
                   cache_status, outcome, attempts, created_at)
SELECT gen_random_uuid(), (SELECT id FROM k), 'default',
       CASE model WHEN 'sonnet' THEN 'anthropic' WHEN 'mini' THEN 'openai' ELSE 'ollama' END,
       model,
       CASE WHEN cache = 'miss' AND roll >= 99 THEN 0 WHEN cache = 'miss' THEN tin ELSE 0 END,
       CASE WHEN cache = 'miss' AND roll >= 99 THEN 0 WHEN cache = 'miss' THEN tout ELSE 0 END,
       CASE WHEN cache <> 'miss' OR roll >= 99 THEN 0
            WHEN model = 'sonnet' THEN (tin * 3 + tout * 15) / 1e6
            WHEN model = 'mini'   THEN (tin * 0.15 + tout * 0.6) / 1e6 ELSE 0 END,
       CASE WHEN cache = 'miss' THEN 0
            WHEN model = 'sonnet' THEN (tin * 3 + tout * 15) / 1e6
            WHEN model = 'mini'   THEN (tin * 0.15 + tout * 0.6) / 1e6 ELSE 0 END,
       CASE WHEN cache <> 'miss' THEN 2 + (h4 % 6)
            WHEN model = 'sonnet' THEN 1200 + (h4 % 4500) WHEN model = 'mini' THEN 400 + (h4 % 1800) ELSE 700 + (h4 % 3000) END,
       CASE WHEN cache <> 'miss' THEN 2 ELSE 150 + (h4 % 400) END,
       cache,
       CASE WHEN cache = 'miss' AND roll >= 99 THEN 'all_providers_failed' ELSE 'ok' END,
       CASE WHEN cache = 'miss' AND roll >= 99
              THEN jsonb_build_array(jsonb_build_object('provider', 'google', 'model', 'flash', 'kind', 'primary', 'latency_ms', 900, 'error_kind', 'server', 'error', 'demo failure'))
            WHEN cache = 'miss' AND roll BETWEEN 92 AND 98 AND model = 'mini'
              THEN jsonb_build_array(jsonb_build_object('provider', 'anthropic', 'model', 'sonnet', 'kind', 'primary', 'latency_ms', 600, 'error_kind', 'rate_limited', 'error', 'demo 429'),
                                     jsonb_build_object('provider', 'openai', 'model', 'mini', 'kind', 'fallback', 'latency_ms', 700))
            ELSE '[]'::jsonb END,
       at
FROM shaped
WHERE at <= now();

INSERT INTO usage_daily (api_key_id, day, model, requests, input_tokens, output_tokens, cost_usd, saved_usd, cache_hits)
SELECT api_key_id, (created_at AT TIME ZONE 'UTC')::date, model, count(*), sum(input_tokens), sum(output_tokens), sum(cost_usd), sum(saved_usd),
       count(*) FILTER (WHERE cache_status IN ('hit_exact', 'hit_semantic'))
FROM usage WHERE api_key_id = (SELECT id FROM api_keys WHERE name = 'demo-data')
GROUP BY 1, 2, 3;

SELECT count(*) AS demo_requests, round(sum(cost_usd), 2) AS demo_spend_usd FROM usage WHERE api_key_id = (SELECT id FROM api_keys WHERE name = 'demo-data');
