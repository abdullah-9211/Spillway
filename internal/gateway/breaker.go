package gateway

import (
	"sync"
	"time"
)

type BreakerState int

const (
	BreakerClosed BreakerState = iota
	BreakerOpen
	BreakerHalfOpen
)

func (s BreakerState) String() string {
	return [...]string{"closed", "open", "half_open"}[s]
}

type BreakerConfig struct {
	ConsecutiveFailures int           // open after this many failures in a row
	ErrorRate           float64       // open when this share of a full window failed
	Window              int           // attempts in the sliding window
	OpenFor             time.Duration // stay open this long before admitting a probe
}

func (c BreakerConfig) withDefaults() BreakerConfig {
	if c.ConsecutiveFailures <= 0 {
		c.ConsecutiveFailures = 5
	}
	if c.ErrorRate <= 0 {
		c.ErrorRate = 0.5
	}
	if c.Window <= 0 {
		c.Window = 20
	}
	if c.OpenFor <= 0 {
		c.OpenFor = 30 * time.Second
	}
	return c
}

// Breaker protects one provider. Closed it lets everything through; open it lets nothing through; after
// OpenFor it is half-open and admits a single probe whose result closes or re-opens it.
type Breaker struct {
	cfg BreakerConfig
	now func() time.Time

	mu          sync.Mutex
	state       BreakerState
	consecutive int
	window      []bool // true = failure; ring of the last cfg.Window outcomes
	next        int
	filled      int
	openedAt    time.Time
	probing     bool
}

func NewBreaker(cfg BreakerConfig, now func() time.Time) *Breaker {
	cfg = cfg.withDefaults()
	return &Breaker{cfg: cfg, now: now, window: make([]bool, cfg.Window)}
}

// State reports the current state, moving open to half-open when the wait is over.
func (b *Breaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tick()
	return b.state
}

func (b *Breaker) tick() {
	if b.state == BreakerOpen && b.now().Sub(b.openedAt) >= b.cfg.OpenFor {
		b.state, b.probing = BreakerHalfOpen, false
	}
}

// Allow says whether an attempt may go to this provider. In the half-open state it admits one probe at
// a time; the caller must then call Record or Release.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tick()
	switch b.state {
	case BreakerClosed:
		return true
	case BreakerHalfOpen:
		if b.probing {
			return false
		}
		b.probing = true
		return true
	}
	return false
}

// Available is a read-only hint for policies: would Allow say yes right now?
func (b *Breaker) Available() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tick()
	return b.state == BreakerClosed || (b.state == BreakerHalfOpen && !b.probing)
}

// Record reports the outcome of an attempt that Allow admitted.
func (b *Breaker) Record(success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tick()
	switch b.state {
	case BreakerHalfOpen:
		b.probing = false
		if success {
			b.reset()
		} else {
			b.open()
		}
	case BreakerClosed:
		b.window[b.next] = !success
		b.next = (b.next + 1) % len(b.window)
		if b.filled < len(b.window) {
			b.filled++
		}
		if success {
			b.consecutive = 0
			return
		}
		b.consecutive++
		if b.consecutive >= b.cfg.ConsecutiveFailures || b.errorRateExceeded() {
			b.open()
		}
	}
	// While open, results of requests that were already in flight are ignored.
}

// Release gives back a probe slot when the attempt ended without saying anything about the provider's
// health (the client cancelled, or the request itself was bad).
func (b *Breaker) Release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == BreakerHalfOpen {
		b.probing = false
	}
}

func (b *Breaker) errorRateExceeded() bool {
	if b.filled < len(b.window) {
		return false
	}
	failures := 0
	for _, f := range b.window {
		if f {
			failures++
		}
	}
	return float64(failures)/float64(len(b.window)) >= b.cfg.ErrorRate
}

func (b *Breaker) open() {
	b.state, b.openedAt, b.probing = BreakerOpen, b.now(), false
}

func (b *Breaker) reset() {
	b.state, b.consecutive, b.probing = BreakerClosed, 0, false
	b.next, b.filled = 0, 0
	for i := range b.window {
		b.window[i] = false
	}
}

// Breakers holds one breaker per provider.
type Breakers struct {
	cfg BreakerConfig
	now func() time.Time

	mu sync.Mutex
	m  map[string]*Breaker
}

func NewBreakers(cfg BreakerConfig, now func() time.Time) *Breakers {
	return &Breakers{cfg: cfg, now: now, m: map[string]*Breaker{}}
}

func (s *Breakers) For(provider string) *Breaker {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.m[provider]
	if !ok {
		b = NewBreaker(s.cfg, s.now)
		s.m[provider] = b
	}
	return b
}

// Available implements Health for policies.
func (s *Breakers) Available(provider string) bool { return s.For(provider).Available() }

// States returns every known provider's state, for metrics and the dashboard.
func (s *Breakers) States() map[string]BreakerState {
	s.mu.Lock()
	names := make([]string, 0, len(s.m))
	for n := range s.m {
		names = append(names, n)
	}
	s.mu.Unlock()
	out := make(map[string]BreakerState, len(names))
	for _, n := range names {
		out[n] = s.For(n).State()
	}
	return out
}
