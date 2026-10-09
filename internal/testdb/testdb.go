// Package testdb gives an integration test a database of its own. Runs are claimed by whichever worker looks first, so
// tests that create runs cannot share a database with other packages' tests, which run in parallel.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/abdullah-9211/spillway/internal/db"
)

// New creates a fresh, migrated database next to the one DATABASE_URL names, and drops it when the test ends.
// It returns a pool and the database's URL.
func New(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	base := os.Getenv("DATABASE_URL")
	if base == "" {
		t.Fatal("DATABASE_URL is not set; run `make up` and use `make test-integration`")
	}
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	name := "spw_t_" + hex.EncodeToString(b)

	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	own := u.String()
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = admin.Exec(c, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		admin.Close(c)
	})
	if err := db.Migrate(own); err != nil {
		t.Fatal(err)
	}
	pg, err := db.OpenPostgres(ctx, own)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pg.Close)
	return pg.Pool, own
}
