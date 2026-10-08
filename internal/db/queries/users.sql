-- name: GetUserByUsername :one
SELECT id, username, password_hash, role, created_at FROM users WHERE username = @username;

-- name: CreateUserIfMissing :one
INSERT INTO users (id, username, password_hash, role)
VALUES (@id, @username, @password_hash, @role)
ON CONFLICT (username) DO NOTHING
RETURNING id;

-- name: SetUserPassword :execrows
UPDATE users SET password_hash = @password_hash, role = @role WHERE username = @username;
