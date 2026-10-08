package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/provider/faults"
	"github.com/abdullah-9211/spillway/internal/usage"
)

type ExecConfig struct {
	RequestTimeout   time.Duration // per non-streaming attempt
	FirstByteTimeout time.Duration // per streaming attempt, until the first chunk
	IdleTimeout      time.Duration // between chunks of a stream
	MaxTotal         time.Duration // retries plus fallbacks, request level
	MaxRetries       int           // per candidate
	BackoffBase      time.Duration
	BackoffCap       time.Duration
}

func execConfigFrom(g gatewayConfig) ExecConfig {
	c := ExecConfig{
		RequestTimeout: g.RequestTimeout, FirstByteTimeout: g.FirstByteTimeout, IdleTimeout: g.IdleTimeout,
		MaxTotal: time.Duration(g.MaxTotalMs) * time.Millisecond, MaxRetries: 2,
		BackoffBase: 200 * time.Millisecond, BackoffCap: 5 * time.Second,
	}
	if g.MaxRetries != nil {
		c.MaxRetries = *g.MaxRetries
	}
	return c.withDefaults()
}

func (c ExecConfig) withDefaults() ExecConfig {
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 60 * time.Second
	}
	if c.FirstByteTimeout <= 0 {
		c.FirstByteTimeout = 15 * time.Second
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 30 * time.Second
	}
	if c.MaxTotal <= 0 {
		c.MaxTotal = 120 * time.Second
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = 200 * time.Millisecond
	}
	if c.BackoffCap <= 0 {
		c.BackoffCap = 5 * time.Second
	}
	return c
}

// Backoff is min(cap, base*2^n) scaled by jitter, which callers draw from [0.5, 1.0].
func Backoff(n int, base, cap time.Duration, jitter float64) time.Duration {
	d := base
	for i := 0; i < n && d < cap; i++ {
		d *= 2
	}
	if d > cap {
		d = cap
	}
	return time.Duration(float64(d) * jitter)
}

// Executor runs a Plan: it tries candidates in order, retries retryable failures with jittered backoff,
// consults the circuit breakers, fails over, and optionally hedges. It never exceeds MaxTotal.
type Executor struct {
	cfg       ExecConfig
	providers map[string]provider.Provider
	missing   map[string]string
	breakers  *Breakers
	log       *slog.Logger
	obs       Observer
	tracer    trace.Tracer

	now    func() time.Time
	sleep  func(ctx context.Context, d time.Duration) error
	jitter func() float64
}

func NewExecutor(cfg ExecConfig, providers map[string]provider.Provider, missing map[string]string, b *Breakers, log *slog.Logger) *Executor {
	return &Executor{
		cfg: cfg.withDefaults(), providers: providers, missing: missing, breakers: b, log: log,
		obs: noopObserver{}, tracer: otel.Tracer("spillway/gateway"),
		now:    time.Now,
		sleep:  sleepCtx,
		jitter: func() float64 { return 0.5 + rand.Float64()*0.5 },
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ExecResult is a successful execution.
type ExecResult struct {
	Value    any // *provider.ChatResponse or *primedStream
	Winner   Model
	Attempts []usage.Attempt
}

// ExecError says why no candidate answered.
type ExecError struct {
	Err        error // the last provider error
	Attempts   []usage.Attempt
	Candidates int
	BadRequest bool // the provider rejected what the client sent; no point trying others
	Canceled   bool // the client went away
	Deadline   bool // MaxTotal ran out
}

func (e *ExecError) Error() string {
	switch {
	case e.Canceled:
		return "request cancelled"
	case e.Deadline:
		return "request deadline exceeded"
	case e.Err != nil:
		return e.Err.Error()
	}
	return "no provider available"
}

func (e *ExecError) Unwrap() error { return e.Err }

type tryFunc func(ctx context.Context, p provider.Provider, m Model) (any, error)

type candResult struct {
	idx      int
	val      any
	attempts []usage.Attempt
	err      error
	fatal    bool
}

func (e *Executor) run(parent context.Context, plan Plan, try tryFunc) (*ExecResult, error) {
	ctx, cancel := context.WithTimeout(parent, e.cfg.MaxTotal)
	defer cancel()

	n := len(plan.Candidates)
	results := make(chan candResult, n)
	cancels := make([]context.CancelFunc, n)
	perCand := make([][]usage.Attempt, n)
	next, running := 0, 0

	// After a "prompt too long" failure only models with a larger window are worth trying.
	needWindow := 0
	launch := func(kind string) {
		for next < n && needWindow > 0 {
			m := plan.Candidates[next]
			if m.ContextWindow == 0 || m.ContextWindow > needWindow {
				break
			}
			perCand[next] = []usage.Attempt{{Provider: m.Provider, Model: m.ID, Kind: "skipped",
				Error: fmt.Sprintf("context window %d is not larger than the %d that was too small", m.ContextWindow, needWindow)}}
			next++
		}
		if next >= n || ctx.Err() != nil {
			return
		}
		i := next
		next++
		running++
		cctx, cc := context.WithCancel(ctx)
		cancels[i] = cc
		go func() {
			v, atts, err, fatal := e.runCandidate(cctx, plan.Candidates[i], kind, try)
			results <- candResult{idx: i, val: v, attempts: atts, err: err, fatal: fatal}
		}()
	}
	cancelOthers := func(except int) {
		for j, c := range cancels {
			if c != nil && j != except {
				c()
			}
		}
	}

	var hedge <-chan time.Time
	if plan.HedgeAfter > 0 && n > 1 {
		t := time.NewTimer(plan.HedgeAfter)
		defer t.Stop()
		hedge = t.C
	}

	launch("primary")
	var winner *candResult
	var lastErr error
	var fatalErr error
	for running > 0 {
		select {
		case r := <-results:
			running--
			perCand[r.idx] = r.attempts
			switch {
			case r.err == nil:
				if winner == nil {
					w := r
					winner = &w
					cancelOthers(r.idx) // a hedge loser stops here
				}
			case winner != nil || fatalErr != nil:
				// already decided; this is a loser reporting in
			case r.fatal:
				fatalErr = r.err
				cancelOthers(r.idx)
			default:
				lastErr = r.err
				if provider.KindOf(r.err) == provider.KindContextLength {
					if w := plan.Candidates[r.idx].ContextWindow; w > needWindow {
						needWindow = w
					}
				}
				if running == 0 {
					launch("fallback")
				}
			}
		case <-hedge:
			hedge = nil
			if winner == nil && fatalErr == nil {
				launch("hedge")
			}
		}
	}

	var attempts []usage.Attempt
	for _, a := range perCand {
		attempts = append(attempts, a...)
	}
	switch {
	case winner != nil:
		return &ExecResult{Value: winner.val, Winner: plan.Candidates[winner.idx], Attempts: attempts}, nil
	case fatalErr != nil:
		return nil, &ExecError{Err: fatalErr, Attempts: attempts, Candidates: n, BadRequest: true}
	case parent.Err() != nil:
		return nil, &ExecError{Err: parent.Err(), Attempts: attempts, Candidates: n, Canceled: true}
	}
	return nil, &ExecError{Err: lastErr, Attempts: attempts, Candidates: n, Deadline: errors.Is(ctx.Err(), context.DeadlineExceeded)}
}

// runCandidate makes up to 1+MaxRetries attempts against one model. fatal means the provider rejected
// the request itself, so neither retrying nor failing over can help.
func (e *Executor) runCandidate(ctx context.Context, m Model, firstKind string, try tryFunc) (val any, attempts []usage.Attempt, err error, fatal bool) {
	skip := func(why string) {
		attempts = append(attempts, usage.Attempt{Provider: m.Provider, Model: m.ID, Kind: "skipped", Error: why})
		e.obs.ObserveAttempt(m.Provider, "skipped", "")
	}
	p, ok := e.providers[m.Provider]
	if !ok {
		skip("provider not available: " + e.missing[m.Provider])
		return nil, attempts, errors.New("provider not available"), false
	}
	br := e.breakers.For(m.Provider)

	// A fault asked for by the playground replaces the provider for this request only. It never touches the breaker
	// or the provider-health metrics, because a failure made on purpose says nothing about the provider.
	injected := false
	failsOutright := false
	quiet := hasFaults(ctx) // a request with faults stays out of the provider metrics, for its real attempts too
	if f, set, ok := faultFor(ctx, m.Provider); ok {
		p, injected = faults.Wrap(p, f, set), true
		failsOutright = f.Kind == faults.RateLimit || f.Kind == faults.ServerError
	}

	for n := 0; ; n++ {
		if ctx.Err() != nil {
			return nil, attempts, ctx.Err(), false
		}
		admitted := false
		if !failsOutright { // an outright failure makes no real call, so an open breaker has nothing to protect
			if !br.Allow() {
				skip("circuit breaker open")
				return nil, attempts, errors.New("circuit breaker open"), false
			}
			admitted = true
		}
		kind := firstKind
		if n > 0 {
			kind = "retry"
		}
		t0 := e.now()
		actx, span := e.tracer.Start(ctx, "gateway.attempt", trace.WithAttributes(
			attribute.String("spillway.provider", m.Provider), attribute.String("spillway.model", m.ID), attribute.String("spillway.attempt_kind", kind)))
		v, err := try(actx, p, m)
		a := usage.Attempt{Provider: m.Provider, Model: m.ID, Kind: kind, LatencyMs: int(e.now().Sub(t0).Milliseconds()), Injected: injected}
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
		if err == nil {
			switch {
			case injected && admitted:
				br.Release()
			case !injected:
				br.Record(true)
				if !quiet {
					e.obs.ObserveAttempt(m.Provider, kind, "")
				}
			}
			return v, append(attempts, a), nil, false
		}
		k := provider.KindOf(err)
		a.Error, a.ErrorKind = err.Error(), k.String()
		var pe *provider.ProviderError
		if errors.As(err, &pe) {
			a.Status = pe.Status
		}
		attempts = append(attempts, a)
		if pe != nil && pe.Injected {
			// Made on purpose: not counted anywhere, not retried (the point is to see the fallback), and the
			// candidate is given up on at once.
			if admitted {
				br.Release()
			}
			return nil, attempts, err, false
		}
		if !quiet {
			e.obs.ObserveAttempt(m.Provider, kind, a.ErrorKind)
		}

		switch k {
		case provider.KindBadRequest:
			br.Release()
			return nil, attempts, err, true
		case provider.KindCanceled:
			br.Release()
			return nil, attempts, err, false
		case provider.KindContextLength:
			br.Release() // says nothing about the provider's health
			return nil, attempts, err, false
		case provider.KindAuth:
			br.Record(false)
			e.log.Error("upstream rejected our credentials", "provider", m.Provider, "model", m.ID)
			return nil, attempts, err, false
		}
		// Rate limited, server error or timeout: retry with backoff while there is budget.
		br.Record(false)
		if n >= e.cfg.MaxRetries || ctx.Err() != nil {
			return nil, attempts, err, false
		}
		delay := Backoff(n, e.cfg.BackoffBase, e.cfg.BackoffCap, e.jitter())
		if pe != nil && pe.RetryAfter > 0 {
			delay = pe.RetryAfter
		}
		if dl, ok := ctx.Deadline(); ok && delay >= time.Until(dl) {
			return nil, attempts, err, false // waiting would use up the request's whole budget
		}
		if err := e.sleep(ctx, delay); err != nil {
			return nil, attempts, err, false
		}
	}
}

// --- try functions ---

func (e *Executor) tryChat(req *provider.ChatRequest) tryFunc {
	return func(ctx context.Context, p provider.Provider, m Model) (any, error) {
		up := *req
		up.Model, up.Stream, up.StreamOptions = m.Upstream, false, nil
		actx, cancel := context.WithTimeout(ctx, e.cfg.RequestTimeout)
		defer cancel()
		return p.Chat(actx, &up)
	}
}

// tryStream opens the stream and reads its first chunk, so a failure before the first byte can still be
// retried or failed over. reqCtx, not the executor's deadline, bounds the stream's life afterwards.
func (e *Executor) tryStream(reqCtx context.Context, req *provider.ChatRequest) tryFunc {
	return func(ctx context.Context, p provider.Provider, m Model) (any, error) {
		up := *req
		up.Model, up.Stream = m.Upstream, true
		up.StreamOptions = &provider.StreamOptions{IncludeUsage: true}

		actx, acancel := context.WithCancel(reqCtx)
		var timedOut atomic.Bool
		timer := time.AfterFunc(e.cfg.FirstByteTimeout, func() { timedOut.Store(true); acancel() })
		stopWatch := context.AfterFunc(ctx, acancel) // the request-level deadline or a hedge cancel
		fail := func(err error) (any, error) {
			timer.Stop()
			stopWatch()
			acancel()
			if timedOut.Load() {
				return nil, &provider.ProviderError{Kind: provider.KindTimeout, Err: fmt.Errorf("no data within %s", e.cfg.FirstByteTimeout)}
			}
			if ctx.Err() != nil && provider.KindOf(err) == provider.KindCanceled {
				return nil, err
			}
			return nil, err
		}

		rd, err := p.ChatStream(actx, &up)
		if err != nil {
			return fail(err)
		}
		first, err := rd.Next()
		if errors.Is(err, io.EOF) {
			err = &provider.ProviderError{Kind: provider.KindServer, Err: errors.New("empty stream")}
		}
		if err != nil {
			_ = rd.Close()
			return fail(err)
		}
		timer.Stop()
		stopWatch()
		if timedOut.Load() {
			_ = rd.Close()
			return fail(nil)
		}
		return newPrimedStream(rd, first, acancel, e.cfg.IdleTimeout), nil
	}
}

// primedStream hands out the already-read first chunk, then the rest, enforcing the idle timeout.
type primedStream struct {
	rd     provider.StreamReader
	first  *provider.ChatChunk
	cancel context.CancelFunc
	idle   time.Duration
	timer  *time.Timer
	fired  atomic.Bool
}

func newPrimedStream(rd provider.StreamReader, first *provider.ChatChunk, cancel context.CancelFunc, idle time.Duration) *primedStream {
	s := &primedStream{rd: rd, first: first, cancel: cancel, idle: idle}
	s.timer = time.AfterFunc(time.Hour, func() { s.fired.Store(true); cancel() })
	s.timer.Stop()
	return s
}

func (s *primedStream) Next() (*provider.ChatChunk, error) {
	if s.first != nil {
		c := s.first
		s.first = nil
		return c, nil
	}
	s.fired.Store(false)
	s.timer.Reset(s.idle)
	c, err := s.rd.Next()
	s.timer.Stop()
	if err != nil && s.fired.Load() {
		return nil, &provider.ProviderError{Kind: provider.KindTimeout, Err: fmt.Errorf("stream idle for %s", s.idle)}
	}
	return c, err
}

func (s *primedStream) Close() error {
	s.timer.Stop()
	s.cancel()
	return s.rd.Close()
}
