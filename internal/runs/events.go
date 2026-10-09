package runs

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NotifyChannel is the Postgres channel every step insert notifies, with the payload "<run_id>:<step row id>".
const NotifyChannel = "run_steps"

// Broker turns Postgres NOTIFY into wake-ups for the connections watching a run. The notification only says "something
// was written": a subscriber reads the rows themselves from the table, in order, from the last id it sent. That keeps
// the stream correct if a notification is lost, delivered twice, or arrives before a reconnect.
type Broker struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	mu    sync.Mutex
	subs  map[uuid.UUID]map[chan struct{}]struct{}
	ready chan struct{}
	once  sync.Once
}

func NewBroker(pool *pgxpool.Pool, log *slog.Logger) *Broker {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Broker{pool: pool, log: log, subs: map[uuid.UUID]map[chan struct{}]struct{}{}, ready: make(chan struct{})}
}

// Ready is closed once the broker is listening. A watcher that subscribes earlier still sees every row, because it
// reads the table itself first and on its poll.
func (b *Broker) Ready() <-chan struct{} { return b.ready }

// Subscribe returns a channel that receives a signal when rows are written for the run, and a function to stop.
func (b *Broker) Subscribe(run uuid.UUID) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	if b.subs[run] == nil {
		b.subs[run] = map[chan struct{}]struct{}{}
	}
	b.subs[run][ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs[run], ch)
		if len(b.subs[run]) == 0 {
			delete(b.subs, run)
		}
		b.mu.Unlock()
	}
}

func (b *Broker) wake(run uuid.UUID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[run] {
		select {
		case ch <- struct{}{}:
		default: // a signal is already waiting
		}
	}
}

func (b *Broker) wakeAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, set := range b.subs {
		for ch := range set {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}

// Run holds one connection that LISTENs, and reconnects with a pause if it drops, until ctx ends. After a reconnect it
// wakes every subscriber, since notifications may have been missed in between.
func (b *Broker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := b.listen(ctx); err != nil && ctx.Err() == nil {
			b.log.Warn("run events listener lost its connection, reconnecting", "error", err)
		}
		b.wakeAll()
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

func (b *Broker) listen(ctx context.Context) error {
	conn, err := b.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+NotifyChannel); err != nil {
		return err
	}
	b.wakeAll() // anything written before the LISTEN started
	b.once.Do(func() { close(b.ready) })
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		id, _, ok := strings.Cut(n.Payload, ":")
		if !ok {
			continue
		}
		if run, err := uuid.Parse(id); err == nil {
			b.wake(run)
		}
	}
}
