package gateway

import (
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestBreaker() (*Breaker, *clock) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	return NewBreaker(BreakerConfig{ConsecutiveFailures: 5, ErrorRate: 0.5, Window: 20, OpenFor: 30 * time.Second}, c.now), c
}

func fail(b *Breaker, n int) {
	for i := 0; i < n; i++ {
		b.Allow()
		b.Record(false)
	}
}

func TestBreakerTransitions(t *testing.T) {
	type step func(b *Breaker, c *clock)
	failN := func(n int) step { return func(b *Breaker, _ *clock) { fail(b, n) } }
	succN := func(n int) step {
		return func(b *Breaker, _ *clock) {
			for i := 0; i < n; i++ {
				b.Allow()
				b.Record(true)
			}
		}
	}
	wait := func(d time.Duration) step { return func(_ *Breaker, c *clock) { c.advance(d) } }
	probe := func(ok bool) step { return func(b *Breaker, _ *clock) { b.Allow(); b.Record(ok) } }

	tests := []struct {
		name  string
		steps []step
		want  BreakerState
	}{
		{"starts closed", nil, BreakerClosed},
		{"four failures stay closed", []step{failN(4)}, BreakerClosed},
		{"five consecutive failures open it", []step{failN(5)}, BreakerOpen},
		{"a success resets the consecutive count", []step{failN(4), succN(1), failN(4)}, BreakerClosed},
		{"stays open before the wait is over", []step{failN(5), wait(29 * time.Second)}, BreakerOpen},
		{"half-open after the wait", []step{failN(5), wait(30 * time.Second)}, BreakerHalfOpen},
		{"successful probe closes it", []step{failN(5), wait(30 * time.Second), probe(true)}, BreakerClosed},
		{"failed probe re-opens it", []step{failN(5), wait(30 * time.Second), probe(false)}, BreakerOpen},
		{"a re-opened breaker waits the full time again", []step{failN(5), wait(30 * time.Second), probe(false), wait(29 * time.Second)}, BreakerOpen},
		// 50% over a full window of 20, without 5 in a row: alternate fail/success for 20 attempts.
		{"error rate opens a full window", []step{func(b *Breaker, _ *clock) {
			for i := 0; i < 10; i++ {
				b.Allow()
				b.Record(true)
				fail(b, 1)
			}
		}}, BreakerOpen},
		{"a half-full window never trips on rate", []step{func(b *Breaker, _ *clock) {
			for i := 0; i < 4; i++ {
				fail(b, 1)
				b.Allow()
				b.Record(true)
			}
		}}, BreakerClosed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, c := newTestBreaker()
			for _, s := range tc.steps {
				s(b, c)
			}
			if got := b.State(); got != tc.want {
				t.Errorf("state = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestBreakerAllowsOneProbe(t *testing.T) {
	b, c := newTestBreaker()
	fail(b, 5)
	if b.Allow() {
		t.Fatal("an open breaker must not allow")
	}
	c.advance(30 * time.Second)
	if !b.Allow() {
		t.Fatal("half-open admits one probe")
	}
	if b.Allow() {
		t.Fatal("half-open admits only one probe at a time")
	}
	b.Release() // the probe was cancelled and says nothing about health
	if !b.Allow() {
		t.Fatal("a released probe slot can be used again")
	}
}

func TestBreakerIgnoresStragglersWhileOpen(t *testing.T) {
	b, _ := newTestBreaker()
	fail(b, 5)
	b.Record(true) // a request that started before the breaker opened finishes late
	if b.State() != BreakerOpen {
		t.Error("late results must not close an open breaker")
	}
}

func TestBreakersAreIndependent(t *testing.T) {
	c := &clock{t: time.Unix(1, 0)}
	s := NewBreakers(BreakerConfig{}, c.now)
	fail(s.For("a"), 5)
	if s.Available("a") || !s.Available("b") {
		t.Errorf("a should be unavailable and b available: %v", s.States())
	}
}
