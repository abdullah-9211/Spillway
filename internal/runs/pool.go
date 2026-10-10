package runs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"time"
)

type PoolOptions struct {
	Workers   int           // concurrent runs; default 8
	LeaseTTL  time.Duration // default 30s
	Heartbeat time.Duration // default 10s
	Poll      time.Duration // how long an idle worker waits before looking again; default 1s
	Grace     time.Duration // how long a shutdown lets in-flight steps finish; default 20s
	Owner     string        // this process's worker id; default a random "w-xxxx"
	Log       *slog.Logger
	Now       func() time.Time
}

// Pool is the worker side of the engine: N goroutines that claim runs, hold the lease with a heartbeat, and execute.
type Pool struct {
	store *Store
	eng   *Engine
	o     PoolOptions
}

// NewWorkerID is a short id for this process. Every row a process writes carries it, which is how the run graph can
// show which worker did what.
func NewWorkerID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return "w-" + hex.EncodeToString(b)
}

func NewPool(store *Store, eng *Engine, o PoolOptions) *Pool {
	if o.Workers <= 0 {
		o.Workers = 8
	}
	if o.LeaseTTL <= 0 {
		o.LeaseTTL = 30 * time.Second
	}
	if o.Heartbeat <= 0 {
		o.Heartbeat = 10 * time.Second
	}
	if o.Poll <= 0 {
		o.Poll = time.Second
	}
	if o.Grace <= 0 {
		o.Grace = 20 * time.Second
	}
	if o.Owner == "" {
		o.Owner = NewWorkerID()
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if eng.Now == nil {
		eng.Now = o.Now
	}
	return &Pool{store: store, eng: eng, o: o}
}

func (p *Pool) Owner() string { return p.o.Owner }

// Run works until ctx is cancelled, then shuts down gracefully: it stops claiming, gives in-flight steps the grace
// period to finish, then abandons what is left and releases those leases so other workers resume the runs at once.
func (p *Pool) Run(ctx context.Context) {
	drain, cancelDrain := context.WithCancelCause(context.Background())
	defer cancelDrain(nil)
	var wg sync.WaitGroup
	for i := 0; i < p.o.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.loop(ctx, drain)
		}()
	}
	p.o.Log.Info("run workers started", "owner", p.o.Owner, "workers", p.o.Workers)
	<-ctx.Done()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(p.o.Grace):
		p.o.Log.Warn("shutdown grace period over, abandoning in-flight runs", "grace", p.o.Grace)
		cancelDrain(ErrShutdown)
		<-done
	}
	p.o.Log.Info("run workers stopped", "owner", p.o.Owner)
}

func (p *Pool) loop(stop, drain context.Context) {
	for stop.Err() == nil {
		claimed, err := p.ClaimAndWork(stop, drain)
		if err != nil {
			p.o.Log.Error("run worker error", "error", err)
		}
		if claimed && err == nil {
			continue // look again at once: there may be more waiting
		}
		t := time.NewTimer(p.o.Poll)
		select {
		case <-t.C:
		case <-stop.Done():
		}
		t.Stop()
	}
}

// ClaimAndWork claims one run and works it to the end (or until this worker has to stop). It reports whether it
// found one. Tests call it directly.
func (p *Pool) ClaimAndWork(stop, drain context.Context) (bool, error) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(stop), 10*time.Second)
	r, err := p.store.Claim(cctx, p.o.Owner, p.o.LeaseTTL, p.o.Now())
	cancel()
	if err != nil || r == nil {
		return false, err
	}
	return true, p.work(drain, r)
}

func (p *Pool) work(drain context.Context, r *Run) error {
	lease := Lease{RunID: r.ID, Owner: p.o.Owner, Epoch: r.LeaseEpoch}
	log := p.o.Log.With("run_id", r.ID, "epoch", lease.Epoch, "worker", p.o.Owner)
	log.Info("run claimed", "status", r.Status)

	runCtx, cancel := context.WithCancelCause(drain)
	defer cancel(nil)

	hbDone := make(chan struct{})
	hbCtx, stopHB := context.WithCancel(context.Background())
	go func() {
		defer close(hbDone)
		t := time.NewTicker(p.o.Heartbeat)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
			}
			hctx, c := context.WithTimeout(hbCtx, p.o.Heartbeat)
			ok, cancelReq, err := p.store.Heartbeat(hctx, lease, p.o.LeaseTTL, p.o.Now())
			c()
			switch {
			case hbCtx.Err() != nil:
				return
			case err != nil:
				log.Warn("heartbeat failed", "error", err) // keep trying: the lease lasts several heartbeats
			case !ok:
				log.Warn("lease lost, abandoning the run")
				cancel(ErrLeaseLost)
				return
			case cancelReq:
				cancel(ErrCancelRequested)
				return
			}
		}
	}()

	sctx, c := context.WithTimeout(runCtx, 30*time.Second)
	steps, err := p.store.Steps(sctx, r.ID, 0, 0)
	c()
	if err == nil {
		err = p.eng.Execute(runCtx, *r, p.store.Log(lease, p.o.Now), steps)
	}
	stopHB()
	<-hbDone

	switch {
	case err == nil:
		log.Info("run finished")
	case errors.Is(err, ErrParked):
		// Waiting for a person or for a wake time. Give the lease up so a wake or a decision is picked up at once.
		rctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		if rerr := p.store.Release(rctx, lease, p.o.Now()); rerr != nil {
			log.Warn("could not release the lease of a parked run", "error", rerr)
		}
		log.Info("run parked")
	case errors.Is(err, ErrAbandoned):
		if errors.Is(context.Cause(runCtx), ErrShutdown) {
			rctx, c := context.WithTimeout(context.Background(), 5*time.Second)
			defer c()
			if rerr := p.store.Release(rctx, lease, p.o.Now()); rerr != nil {
				log.Warn("could not release the lease", "error", rerr)
			} else {
				log.Info("shutdown: lease released, another worker will resume the run")
			}
		}
	default:
		log.Error("run worker failed, leaving the lease to expire so another worker retries", "error", err)
	}
	return nil
}
