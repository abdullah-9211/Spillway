CREATE TABLE users (
  id            uuid PRIMARY KEY,
  username      text UNIQUE NOT NULL,
  password_hash text NOT NULL,                       -- argon2id, PHC string
  role          text NOT NULL CHECK (role IN ('admin','viewer')),
  created_at    timestamptz NOT NULL DEFAULT now()
);
