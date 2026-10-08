-- What was asked in the playground and what came back. The id is the gateway request id, so a row here lines up
-- with the request's row in usage. Prompts are kept because the playground exists to look at them again.
CREATE TABLE playground_history (
  id         uuid PRIMARY KEY,
  policy     text NOT NULL,
  prompt     text NOT NULL,
  system     text NOT NULL DEFAULT '',
  answer     text NOT NULL DEFAULT '',
  stream     boolean NOT NULL DEFAULT false,
  faults     jsonb NOT NULL DEFAULT '[]',
  result     jsonb NOT NULL,                 -- the response the dashboard got: route, tokens, cost, timing
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX playground_history_recent ON playground_history (created_at DESC, id DESC);
