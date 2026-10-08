CREATE TABLE api_keys (
  id                 uuid PRIMARY KEY,
  name               text NOT NULL,
  key_prefix         text NOT NULL,
  key_hash           bytea NOT NULL UNIQUE,
  rate_limit_rpm     int,
  monthly_budget_usd numeric(14,8),
  semantic_cache     boolean NOT NULL DEFAULT false,
  cache_nonzero_temp boolean NOT NULL DEFAULT false,
  created_at         timestamptz NOT NULL DEFAULT now(),
  revoked_at         timestamptz
);

-- One row per gateway request. run_id has no foreign key yet: the runs table arrives in Phase 8,
-- which adds the constraint.
CREATE TABLE usage (
  id            uuid PRIMARY KEY,
  api_key_id    uuid REFERENCES api_keys(id),
  run_id        uuid,
  policy        text NOT NULL,
  provider      text,
  model         text,
  input_tokens  int NOT NULL DEFAULT 0,
  output_tokens int NOT NULL DEFAULT 0,
  cost_usd      numeric(14,8) NOT NULL DEFAULT 0,
  saved_usd     numeric(14,8) NOT NULL DEFAULT 0,
  latency_ms    int NOT NULL,
  ttfb_ms       int,
  cache_status  text NOT NULL CHECK (cache_status IN ('miss','hit_exact','hit_semantic','bypass')),
  outcome       text NOT NULL CHECK (outcome IN
                  ('ok','client_cancelled','upstream_error','all_providers_failed','rate_limited','over_budget','invalid')),
  attempts      jsonb NOT NULL DEFAULT '[]',
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX usage_key_time ON usage (api_key_id, created_at);
CREATE INDEX usage_model_time ON usage (model, created_at);

CREATE TABLE usage_daily (
  api_key_id    uuid NOT NULL,
  day           date NOT NULL,
  model         text NOT NULL,
  requests      bigint NOT NULL DEFAULT 0,
  input_tokens  bigint NOT NULL DEFAULT 0,
  output_tokens bigint NOT NULL DEFAULT 0,
  cost_usd      numeric(14,8) NOT NULL DEFAULT 0,
  saved_usd     numeric(14,8) NOT NULL DEFAULT 0,
  cache_hits    bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (api_key_id, day, model)
);
