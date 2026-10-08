//go:build integration

package usage_test

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/abdullah-9211/spillway/internal/db"
	"github.com/abdullah-9211/spillway/internal/db/sqlcgen"
	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/money"
	"github.com/abdullah-9211/spillway/internal/usage"
)

type env struct {
	pg  *db.Postgres
	key keys.Key
	q   *sqlcgen.Queries
}

func setup(t *testing.T) env {
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
	k, _, err := keys.NewStore(pg.Pool).Create(context.Background(), keys.CreateParams{Name: "usage-test"})
	if err != nil {
		t.Fatal(err)
	}
	return env{pg: pg, key: k, q: sqlcgen.New(pg.Pool)}
}

func row(keyID uuid.UUID, model string, cost money.Micros, cache usage.CacheStatus) usage.Row {
	id, _ := uuid.NewV7()
	ttfb := 40
	return usage.Row{
		ID: id, KeyID: keyID, Policy: model, Provider: "openai", Model: model,
		InputTokens: 100, OutputTokens: 50, Cost: cost, LatencyMs: 120, TTFBMs: &ttfb,
		CacheStatus: cache, Outcome: usage.OutcomeOK, CreatedAt: time.Now().UTC(),
		Attempts: []usage.Attempt{{Provider: "openai", Model: model, Kind: "primary", LatencyMs: 118}},
	}
}

func TestCloseDrainsBuffer(t *testing.T) {
	e := setup(t)
	// A long interval and big batch so nothing is flushed until Close is called.
	w := usage.NewWriter(e.pg.Pool, slog.Default(), usage.WriterOptions{BatchSize: 1000, Interval: time.Hour})

	var ids []uuid.UUID
	for i := 0; i < 25; i++ {
		r := row(e.key.ID, "drain-model", 1000, usage.CacheMiss)
		ids = append(ids, r.ID)
		w.Record(r)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.Written() != 25 || w.Dropped() != 0 {
		t.Fatalf("written=%d dropped=%d, want 25/0", w.Written(), w.Dropped())
	}
	for _, id := range ids {
		if _, err := e.q.GetUsage(context.Background(), id); err != nil {
			t.Fatalf("row %s missing after Close: %v", id, err)
		}
	}
}

func TestRowAndDailyRollup(t *testing.T) {
	e := setup(t)
	w := usage.NewWriter(e.pg.Pool, slog.Default(), usage.WriterOptions{})

	miss := row(e.key.ID, "rollup-model", 1234, usage.CacheMiss)
	hit := row(e.key.ID, "rollup-model", 0, usage.CacheHitExact)
	hit.Saved = 1234
	w.Record(miss)
	w.Record(hit)
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, err := e.q.GetUsage(context.Background(), miss.ID)
	if err != nil {
		t.Fatal(err)
	}
	cost, _ := db.MicrosFromNumeric(got.CostUsd)
	if cost != 1234 || got.InputTokens != 100 || got.OutputTokens != 50 || got.Outcome != "ok" || got.CacheStatus != "miss" {
		t.Errorf("row: %+v cost=%d", got, cost)
	}
	if !got.TtfbMs.Valid || got.TtfbMs.Int32 != 40 {
		t.Errorf("ttfb = %+v", got.TtfbMs)
	}
	if string(got.Attempts) == "" || string(got.Attempts) == "[]" {
		t.Errorf("attempts not stored: %s", got.Attempts)
	}

	day, err := e.q.GetUsageDaily(context.Background(), sqlcgen.GetUsageDailyParams{
		ApiKeyID: e.key.ID, Day: pgtype.Date{Time: time.Now().UTC().Truncate(24 * time.Hour), Valid: true}, Model: "rollup-model"})
	if err != nil {
		t.Fatal(err)
	}
	dc, _ := db.MicrosFromNumeric(day.CostUsd)
	ds, _ := db.MicrosFromNumeric(day.SavedUsd)
	if day.Requests != 2 || day.InputTokens != 200 || day.OutputTokens != 100 || dc != 1234 || ds != 1234 || day.CacheHits != 1 {
		t.Errorf("daily: %+v cost=%d saved=%d", day, dc, ds)
	}
}

func TestRecordAfterCloseIsDroppedNotPanicking(t *testing.T) {
	e := setup(t)
	w := usage.NewWriter(e.pg.Pool, slog.Default(), usage.WriterOptions{})
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.Record(row(e.key.ID, "late", 1, usage.CacheMiss))
	if w.Dropped() != 1 {
		t.Errorf("dropped = %d, want 1", w.Dropped())
	}
	if err := w.Close(context.Background()); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestFullBufferDropsInsteadOfBlocking(t *testing.T) {
	e := setup(t)
	w := usage.NewWriter(e.pg.Pool, slog.Default(), usage.WriterOptions{Buffer: 1, BatchSize: 1000, Interval: time.Hour})
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			w.Record(row(e.key.ID, "overflow", 1, usage.CacheMiss))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Record blocked on a full buffer")
	}
	_ = w.Close(context.Background())
	if w.Dropped() == 0 {
		t.Error("expected drops with a one-row buffer")
	}
}
