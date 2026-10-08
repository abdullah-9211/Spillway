package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/abdullah-9211/spillway/internal/db"
)

func migrateCmd(args []string, getenv func(string) string, stdout, errOut io.Writer) error {
	cfg, err := parseConfig("migrate", args, getenv, errOut)
	if err != nil {
		return err
	}
	if cfg.DatabaseURL == "" {
		return errors.New("DATABASE_URL or --database-url is required")
	}
	if err := db.Migrate(cfg.DatabaseURL); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "migrations up to date")
	return nil
}
