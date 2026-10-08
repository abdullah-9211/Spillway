package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/abdullah-9211/spillway/internal/db/sqlcgen"
)

// MinPasswordLen is enforced when seeding, so the two accounts cannot start with a trivial password.
const MinPasswordLen = 8

type User struct {
	ID       uuid.UUID
	Username string
	Hash     string
	Role     Role
}

type Users struct {
	q      *sqlcgen.Queries
	params Params
}

func NewUsers(pool *pgxpool.Pool, params Params) *Users {
	return &Users{q: sqlcgen.New(pool), params: params}
}

var ErrNoUser = errors.New("no such user")

func (u *Users) Get(ctx context.Context, username string) (User, error) {
	row, err := u.q.GetUserByUsername(ctx, username)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNoUser
	}
	if err != nil {
		return User{}, fmt.Errorf("look up user: %w", err)
	}
	return User{ID: row.ID, Username: row.Username, Hash: row.PasswordHash, Role: Role(row.Role)}, nil
}

// Seed creates the user if it does not exist. An existing user is left alone unless reset is true, so a
// restart never silently changes a password someone chose. It reports whether it created or changed a row.
func (u *Users) Seed(ctx context.Context, username, password string, role Role, reset bool) (bool, error) {
	switch {
	case username == "":
		return false, errors.New("username is empty")
	case len(password) < MinPasswordLen:
		return false, fmt.Errorf("password for %q must be at least %d characters", username, MinPasswordLen)
	case !role.Valid():
		return false, fmt.Errorf("unknown role %q", role)
	}
	hash, err := Hash(password, u.params)
	if err != nil {
		return false, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return false, err
	}
	if _, err := u.q.CreateUserIfMissing(ctx, sqlcgen.CreateUserIfMissingParams{
		ID: id, Username: username, PasswordHash: hash, Role: string(role)}); err == nil {
		return true, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("create user: %w", err)
	}
	if !reset {
		return false, nil
	}
	if _, err := u.q.SetUserPassword(ctx, sqlcgen.SetUserPasswordParams{Username: username, PasswordHash: hash, Role: string(role)}); err != nil {
		return false, fmt.Errorf("reset user: %w", err)
	}
	return true, nil
}
