// Package usage records one row per gateway request, off the response path.
package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/abdullah-9211/spillway/internal/db"
	"github.com/abdullah-9211/spillway/internal/db/sqlcgen"
	"github.com/abdullah-9211/spillway/internal/money"
)

type CacheStatus string

const (
	CacheMiss        CacheStatus = "miss"
	CacheHitExact    CacheStatus = "hit_exact"
	CacheHitSemantic CacheStatus = "hit_semantic"
	CacheBypass      CacheStatus = "bypass"
)

type Outcome string

const (
	OutcomeOK              Outcome = "ok"
	OutcomeClientCancelled Outcome = "client_cancelled"
	OutcomeUpstreamError   Outcome = "upstream_error"
	OutcomeAllFailed       Outcome = "all_providers_failed"
	OutcomeRateLimited     Outcome = "rate_limited"
	OutcomeOverBudget      Outcome = "over_budget"
	OutcomeInvalid         Outcome = "invalid"
)

// Attempt is one call to a provider. Kind is primary, retry, fallback, hedge, or skipped (breaker open or provider unavailable).
type Attempt struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Kind      string `json:"kind"`
	LatencyMs int    `json:"latency_ms"`
	Status    int    `json:"status,omitempty"`
	Error     string `json:"error,omitempty"`
	ErrorKind string `json:"error_kind,omitempty"` // rate_limited, server, timeout, auth, ...
	Injected  bool   `json:"injected,omitempty"`   // reserved for playground fault injection
	Estimated bool   `json:"estimated,omitempty"`  // token counts were estimated, not reported by the provider
}

type Row struct {
	ID           uuid.UUID
	KeyID        uuid.UUID
	RunID        *uuid.UUID
	Policy       string
	Provider     string
	Model        string
	InputTokens  int
	OutputTokens int
	Cost         money.Micros
	Saved        money.Micros
	LatencyMs    int
	TTFBMs       *int
	CacheStatus  CacheStatus
	Outcome      Outcome
	Attempts     []Attempt
	CreatedAt    time.Time
}

// Recorder accepts finished requests. Record must not block the caller.
type Recorder interface {
	Record(Row)
}

// Writer buffers rows in memory and writes them in batches from a background goroutine, so a slow
// database never adds latency to a request. Close drains the buffer.
type Writer struct {
	q    *sqlcgen.Queries
	pool *pgxpool.Pool
	log  *slog.Logger

	batchSize int
	interval  time.Duration

	mu     sync.RWMutex
	closed bool
	ch     chan Row
	done   chan struct{}

	dropped atomic.Int64
	written atomic.Int64
}

type WriterOptions struct {
	Buffer    int           // rows held in memory; default 4096
	BatchSize int           // default 100
	Interval  time.Duration // flush interval; default 200ms
}

func NewWriter(pool *pgxpool.Pool, log *slog.Logger, opt WriterOptions) *Writer {
	if opt.Buffer <= 0 {
		opt.Buffer = 4096
	}
	if opt.BatchSize <= 0 {
		opt.BatchSize = 100
	}
	if opt.Interval <= 0 {
		opt.Interval = 200 * time.Millisecond
	}
	w := &Writer{
		q: sqlcgen.New(pool), pool: pool, log: log,
		batchSize: opt.BatchSize, interval: opt.Interval,
		ch: make(chan Row, opt.Buffer), done: make(chan struct{}),
	}
	go w.loop()
	return w
}

// Record queues a row. If the buffer is full the row is dropped and counted: accounting is allowed to lose
// rows under overload, a request is not allowed to wait for it.
func (w *Writer) Record(r Row) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		w.dropped.Add(1)
		w.log.Error("usage row recorded after shutdown, dropped", "request_id", r.ID)
		return
	}
	select {
	case w.ch <- r:
	default:
		w.dropped.Add(1)
		w.log.Error("usage buffer full, row dropped", "request_id", r.ID)
	}
}

func (w *Writer) Dropped() int64 { return w.dropped.Load() }
func (w *Writer) Written() int64 { return w.written.Load() }

// Close stops accepting rows, writes everything still buffered and waits for it, or for ctx.
func (w *Writer) Close(ctx context.Context) error {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.ch)
	}
	w.mu.Unlock()
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("usage writer did not drain: %w", ctx.Err())
	}
}

func (w *Writer) loop() {
	defer close(w.done)
	tick := time.NewTicker(w.interval)
	defer tick.Stop()
	batch := make([]Row, 0, w.batchSize)
	flush := func() {
		if len(batch) > 0 {
			w.flush(batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case r, ok := <-w.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, r)
			if len(batch) >= w.batchSize {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

// flush writes a batch in one transaction, retrying a few times if the database is briefly unavailable.
func (w *Writer) flush(batch []Row) {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 250 * time.Millisecond)
		}
		// Not tied to a request context: a shutdown drain must still be able to finish.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = w.write(ctx, batch)
		cancel()
		if err == nil {
			w.written.Add(int64(len(batch)))
			return
		}
	}
	w.dropped.Add(int64(len(batch)))
	w.log.Error("usage batch lost after retries", "rows", len(batch), "error", err)
}

func (w *Writer) write(ctx context.Context, batch []Row) error {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op
	q := w.q.WithTx(tx)
	for _, r := range batch {
		attempts, err := json.Marshal(r.Attempts)
		if err != nil {
			return err
		}
		if r.Attempts == nil {
			attempts = []byte("[]")
		}
		p := sqlcgen.InsertUsageParams{
			ID: r.ID, ApiKeyID: uuid.NullUUID{UUID: r.KeyID, Valid: true},
			Policy: r.Policy, Provider: pgtype.Text{String: r.Provider, Valid: r.Provider != ""},
			Model:       pgtype.Text{String: r.Model, Valid: r.Model != ""},
			InputTokens: int32(r.InputTokens), OutputTokens: int32(r.OutputTokens),
			CostUsd: db.NumericFromMicros(r.Cost), SavedUsd: db.NumericFromMicros(r.Saved),
			LatencyMs: int32(r.LatencyMs), CacheStatus: string(r.CacheStatus), Outcome: string(r.Outcome),
			Attempts: attempts, CreatedAt: r.CreatedAt.UTC(),
		}
		if r.RunID != nil {
			p.RunID = uuid.NullUUID{UUID: *r.RunID, Valid: true}
		}
		if r.TTFBMs != nil {
			p.TtfbMs = pgtype.Int4{Int32: int32(*r.TTFBMs), Valid: true}
		}
		if err := q.InsertUsage(ctx, p); err != nil {
			return fmt.Errorf("insert usage: %w", err)
		}
		hits := int64(0)
		if r.CacheStatus == CacheHitExact || r.CacheStatus == CacheHitSemantic {
			hits = 1
		}
		day := r.CreatedAt.UTC().Truncate(24 * time.Hour)
		if err := q.UpsertUsageDaily(ctx, sqlcgen.UpsertUsageDailyParams{
			ApiKeyID: r.KeyID, Day: pgtype.Date{Time: day, Valid: true}, Model: r.Model,
			InputTokens: int64(r.InputTokens), OutputTokens: int64(r.OutputTokens),
			CostUsd: db.NumericFromMicros(r.Cost), SavedUsd: db.NumericFromMicros(r.Saved), CacheHits: hits,
		}); err != nil {
			return fmt.Errorf("upsert usage_daily: %w", err)
		}
	}
	return tx.Commit(ctx)
}
