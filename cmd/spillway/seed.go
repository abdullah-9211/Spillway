package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/abdullah-9211/spillway/internal/auth"
	"github.com/abdullah-9211/spillway/internal/db"
)

// seedUsers creates the two dashboard accounts from SEED_ADMIN_* and SEED_VIEWER_* when they are missing.
// With reset it also overwrites an existing account's password. Accounts whose variables are unset are skipped.
func seedUsers(ctx context.Context, pool *pgxpool.Pool, getenv func(string) string, reset bool, log *slog.Logger) error {
	users := auth.NewUsers(pool, auth.DefaultParams)
	var errs []error
	for _, s := range []struct {
		prefix string
		role   auth.Role
	}{{"SEED_ADMIN", auth.RoleAdmin}, {"SEED_VIEWER", auth.RoleViewer}} {
		name, pw := getenv(s.prefix+"_USER"), getenv(s.prefix+"_PASSWORD")
		if name == "" && pw == "" {
			continue
		}
		if name == "" || pw == "" {
			errs = append(errs, fmt.Errorf("set both %s_USER and %s_PASSWORD, or neither", s.prefix, s.prefix))
			continue
		}
		changed, err := users.Seed(ctx, name, pw, s.role, reset)
		switch {
		case err != nil:
			errs = append(errs, err)
		case changed:
			log.Info("seeded user", "username", name, "role", s.role)
		default:
			log.Debug("user already exists, left unchanged", "username", name)
		}
	}
	return errors.Join(errs...)
}

func seedCmd(ctx context.Context, args []string, getenv func(string) string, stdout, errOut io.Writer) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	fs.SetOutput(errOut)
	dbURL := fs.String("database-url", getenv("DATABASE_URL"), "Postgres URL")
	reset := fs.Bool("reset", false, "overwrite the password of accounts that already exist")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dbURL == "" {
		return errors.New("DATABASE_URL or --database-url is required")
	}
	if getenv("SEED_ADMIN_USER") == "" && getenv("SEED_VIEWER_USER") == "" {
		return errors.New("nothing to seed: set SEED_ADMIN_USER and SEED_ADMIN_PASSWORD, and/or SEED_VIEWER_USER and SEED_VIEWER_PASSWORD")
	}
	pg, err := db.OpenPostgres(ctx, *dbURL)
	if err != nil {
		return err
	}
	defer pg.Close()
	log := slog.New(slog.NewTextHandler(stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return seedUsers(ctx, pg.Pool, getenv, *reset, log)
}
