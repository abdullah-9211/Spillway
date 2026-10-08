//go:build integration

package keys_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/abdullah-9211/spillway/internal/db"
	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/money"
)

func store(t *testing.T) *keys.Store {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Fatal("DATABASE_URL is not set; run `make up` and use `make test`")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatal(err)
	}
	pg, err := db.OpenPostgres(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pg.Close)
	return keys.NewStore(pg.Pool)
}

func TestKeyLifecycle(t *testing.T) {
	ctx := context.Background()
	s := store(t)
	rpm, budget := 60, money.Micros(5_000_000)

	k, full, err := s.Create(ctx, keys.CreateParams{Name: "lifecycle", RateLimitRPM: &rpm, MonthlyBudget: &budget})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Authenticate(ctx, full)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != k.ID || got.Name != "lifecycle" || got.Prefix != full[:8] {
		t.Errorf("authenticated key: %+v", got)
	}
	if got.RateLimitRPM == nil || *got.RateLimitRPM != 60 || got.MonthlyBudget == nil || *got.MonthlyBudget != budget {
		t.Errorf("limits did not round trip: rpm=%v budget=%v", got.RateLimitRPM, got.MonthlyBudget)
	}

	if _, err := s.Authenticate(ctx, "spw_"+full[4:len(full)-1]+"x"); !errors.Is(err, keys.ErrInvalidKey) {
		t.Errorf("unknown key: %v", err)
	}
	if _, err := s.Authenticate(ctx, ""); !errors.Is(err, keys.ErrInvalidKey) {
		t.Errorf("empty key: %v", err)
	}

	if _, err := s.Revoke(ctx, k.ID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, full); !errors.Is(err, keys.ErrInvalidKey) {
		t.Errorf("revoked key must not authenticate: %v", err)
	}
	if _, err := s.Revoke(ctx, k.ID.String()); err != nil {
		t.Errorf("revoking twice should be a no-op: %v", err)
	}
	if _, err := s.Revoke(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, keys.ErrNotFound) {
		t.Errorf("unknown id: %v", err)
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, l := range list {
		if l.ID == k.ID {
			found = true
			if l.RevokedAt == nil {
				t.Error("list should show the key as revoked")
			}
		}
	}
	if !found {
		t.Error("created key missing from list")
	}
}

func TestUnlimitedKey(t *testing.T) {
	ctx := context.Background()
	s := store(t)
	_, full, err := s.Create(ctx, keys.CreateParams{Name: "unlimited"})
	if err != nil {
		t.Fatal(err)
	}
	k, err := s.Authenticate(ctx, full)
	if err != nil || k.RateLimitRPM != nil || k.MonthlyBudget != nil {
		t.Fatalf("a key with no limits must have nil limits: %+v %v", k, err)
	}
}

func TestCreateValidation(t *testing.T) {
	s := store(t)
	zero, neg := 0, money.Micros(-1)
	for name, p := range map[string]keys.CreateParams{
		"no name": {Name: " "}, "zero rpm": {Name: "x", RateLimitRPM: &zero}, "negative budget": {Name: "x", MonthlyBudget: &neg},
	} {
		if _, _, err := s.Create(context.Background(), p); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
