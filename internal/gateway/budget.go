package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/abdullah-9211/spillway/internal/db"
	"github.com/abdullah-9211/spillway/internal/db/sqlcgen"
	"github.com/abdullah-9211/spillway/internal/money"
)

// SpendReader returns what a key has spent since the start of the month containing `now` (UTC).
type SpendReader interface {
	MonthToDate(ctx context.Context, keyID uuid.UUID, now time.Time) (money.Micros, error)
}

// PGSpend reads the usage_daily rollup.
type PGSpend struct{ q *sqlcgen.Queries }

func NewPGSpend(q *sqlcgen.Queries) *PGSpend { return &PGSpend{q: q} }

func monthStart(now time.Time) time.Time {
	n := now.UTC()
	return time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func (s *PGSpend) MonthToDate(ctx context.Context, keyID uuid.UUID, now time.Time) (money.Micros, error) {
	n, err := s.q.SumUsageDailySince(ctx, sqlcgen.SumUsageDailySinceParams{
		ApiKeyID: keyID, Since: pgtype.Date{Time: monthStart(now), Valid: true},
	})
	if err != nil {
		return 0, err
	}
	return db.MicrosFromNumeric(n)
}

// Budgets enforces monthly budgets. Spend is read from usage_daily and cached per key for a few seconds, and
// this process adds its own new spend to the cached figure at once. The check runs before a call and the
// cost is known after it, so concurrent requests can overshoot a budget by what they cost: budgets are
// approximate, not hard caps.
type Budgets struct {
	src SpendReader
	ttl time.Duration
	now func() time.Time

	mu    sync.Mutex
	cache map[uuid.UUID]spendEntry
}

type spendEntry struct {
	spend money.Micros
	month time.Time
	at    time.Time
}

func NewBudgets(src SpendReader, ttl time.Duration) *Budgets {
	if ttl <= 0 {
		ttl = 5 * time.Second
	}
	return &Budgets{src: src, ttl: ttl, now: time.Now, cache: map[uuid.UUID]spendEntry{}}
}

// Exceeded reports whether the key has spent its whole budget. Spending exactly the budget counts as spent.
func (b *Budgets) Exceeded(ctx context.Context, keyID uuid.UUID, budget money.Micros) (bool, money.Micros, error) {
	now := b.now()
	b.mu.Lock()
	e, ok := b.cache[keyID]
	b.mu.Unlock()
	if !ok || now.Sub(e.at) >= b.ttl || !e.month.Equal(monthStart(now)) {
		spend, err := b.src.MonthToDate(ctx, keyID, now)
		if err != nil {
			return false, 0, err
		}
		e = spendEntry{spend: spend, month: monthStart(now), at: now}
		b.mu.Lock()
		b.cache[keyID] = e
		b.mu.Unlock()
	}
	return e.spend >= budget, e.spend, nil
}

// Charge adds spend this process just recorded to the cached figure.
func (b *Budgets) Charge(keyID uuid.UUID, cost money.Micros) {
	if cost <= 0 {
		return
	}
	b.mu.Lock()
	if e, ok := b.cache[keyID]; ok {
		e.spend += cost
		b.cache[keyID] = e
	}
	b.mu.Unlock()
}
