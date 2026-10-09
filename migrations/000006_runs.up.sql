-- The run engine. run_steps is the append-only source of truth; the columns on runs are a projection of it
-- plus the lease fields a worker needs to claim, hold and lose a run.

CREATE TABLE runs (
  id                  uuid PRIMARY KEY,
  api_key_id          uuid NOT NULL REFERENCES api_keys(id),
  idempotency_key     text,                              -- client-supplied, dedupes run creation per key
  status              text NOT NULL CHECK (status IN
                        ('queued','running','waiting_tool','waiting_human','sleeping',
                         'succeeded','failed','cancelled')),
  request             jsonb NOT NULL,                    -- the original POST body, immutable
  limits              jsonb NOT NULL,                    -- the limits in force: the request's, capped by the server's
  failure_reason      text,                              -- max_steps|max_cost|deadline|provider_failed|tool_failed|cancelled|rejected|key_revoked
  step_count          int NOT NULL DEFAULT 0,            -- logical steps started
  cost_usd            numeric(14,8) NOT NULL DEFAULT 0,
  deadline_at         timestamptz NOT NULL,
  -- operational, not history:
  lease_owner         text,
  lease_expires_at    timestamptz,
  lease_epoch         bigint NOT NULL DEFAULT 0,         -- fencing token, bumped on every claim
  wake_at             timestamptz,                       -- set while sleeping
  cancel_requested_at timestamptz,                       -- a cancel waiting for the lease holder to act on it
  created_at          timestamptz NOT NULL DEFAULT now(),
  finished_at         timestamptz,
  UNIQUE (api_key_id, idempotency_key)
);
CREATE INDEX runs_claimable ON runs (status, lease_expires_at, wake_at)
  WHERE status IN ('queued','running','waiting_tool','sleeping');
CREATE INDEX runs_by_key_time ON runs (api_key_id, created_at DESC);

CREATE TABLE run_steps (
  id              bigserial PRIMARY KEY,                 -- also the event stream cursor
  run_id          uuid NOT NULL REFERENCES runs(id),
  step_no         int,                                   -- null for run_status rows
  type            text NOT NULL CHECK (type IN
                    ('model_call','tool_call','wait_human','sleep','compaction','run_status')),
  phase           text NOT NULL CHECK (phase IN ('started','reissued','finished','failed')),
  idempotency_key text,                                  -- tool_call only
  payload         jsonb NOT NULL,
  cost_usd        numeric(14,8) NOT NULL DEFAULT 0,
  lease_epoch     bigint NOT NULL,                       -- epoch of the writer, for audit
  worker_id       text NOT NULL DEFAULT '',              -- worker that wrote the row; the run graph's worker bands read this
  created_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (run_id, step_no, phase, lease_epoch)           -- one row per phase per lease epoch
);
-- Whatever epoch wrote it, a step has at most one terminal row.
CREATE UNIQUE INDEX run_steps_terminal ON run_steps (run_id, step_no) WHERE phase IN ('finished','failed');
CREATE INDEX run_steps_by_run ON run_steps (run_id, id);

CREATE TABLE tools (
  id                uuid PRIMARY KEY,
  name              text UNIQUE NOT NULL,                -- exposed to the model
  kind              text NOT NULL CHECK (kind IN ('mcp','http')),
  endpoint          text NOT NULL,
  headers_enc       bytea,                               -- AES-GCM encrypted JSON of auth headers
  description       text,
  input_schema      jsonb,                               -- http: supplied; mcp: discovered
  timeout_ms        int NOT NULL DEFAULT 10000,
  requires_approval boolean NOT NULL DEFAULT false,
  created_at        timestamptz NOT NULL DEFAULT now()
);

-- usage.run_id was a bare column until the runs table existed.
ALTER TABLE usage ADD CONSTRAINT usage_run_fk FOREIGN KEY (run_id) REFERENCES runs(id);
CREATE INDEX usage_by_run ON usage (run_id) WHERE run_id IS NOT NULL;
