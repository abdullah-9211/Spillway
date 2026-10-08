CREATE TABLE semantic_cache (
  id          uuid PRIMARY KEY,
  scope       uuid NOT NULL,           -- api_key_id (the cache is per key by default)
  model_group text NOT NULL,           -- policy the entry answers plus a hash of its system prompt and tools
  embedding   vector(768) NOT NULL,    -- dimension fixed by the configured embedding model
  response    jsonb NOT NULL,
  cost_usd    numeric(14,8) NOT NULL,  -- cost of the original call, for "saved"
  created_at  timestamptz NOT NULL DEFAULT now(),
  expires_at  timestamptz NOT NULL
);
CREATE INDEX semantic_cache_ann ON semantic_cache USING hnsw (embedding vector_cosine_ops);
CREATE INDEX semantic_cache_scope ON semantic_cache (scope, model_group);
CREATE INDEX semantic_cache_expiry ON semantic_cache (expires_at);
