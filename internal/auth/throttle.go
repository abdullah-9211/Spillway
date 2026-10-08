package auth

import (
	"strings"
	"sync"
	"time"
)

// Throttle limits failed sign-in attempts: at most Max per Window for any one client address and any one
// username. It counts failures only, so a user who signs in correctly is never locked out by their own
// logins. State is in memory and per process, which is enough for the single dashboard in front of it.
type Throttle struct {
	Max    int
	Window time.Duration
	now    func() time.Time

	mu       sync.Mutex
	failures map[string][]time.Time
	calls    int
}

func NewThrottle(max int, window time.Duration) *Throttle {
	return &Throttle{Max: max, Window: window, now: time.Now, failures: map[string][]time.Time{}}
}

func keys(ip, username string) []string {
	return []string{"ip:" + ip, "user:" + strings.ToLower(username)}
}

func (t *Throttle) prune(k string, now time.Time) []time.Time {
	list := t.failures[k]
	cut := 0
	for cut < len(list) && now.Sub(list[cut]) >= t.Window {
		cut++
	}
	if cut > 0 {
		list = list[cut:]
		if len(list) == 0 {
			delete(t.failures, k)
		} else {
			t.failures[k] = list
		}
	}
	return list
}

// Blocked says whether an attempt from this address for this username must be refused now, and for how long.
func (t *Throttle) Blocked(ip, username string) (bool, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	var wait time.Duration
	for _, k := range keys(ip, username) {
		list := t.prune(k, now)
		if len(list) >= t.Max {
			if w := t.Window - now.Sub(list[len(list)-t.Max]); w > wait {
				wait = w
			}
		}
	}
	return wait > 0, wait
}

// Fail records a failed attempt.
func (t *Throttle) Fail(ip, username string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for _, k := range keys(ip, username) {
		t.failures[k] = append(t.prune(k, now), now)
	}
	if t.calls++; t.calls%256 == 0 { // keep the map from growing with one-off addresses
		for k := range t.failures {
			t.prune(k, now)
		}
	}
}

// Reset forgets failures for a username after a successful sign-in.
func (t *Throttle) Reset(username string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.failures, "user:"+strings.ToLower(username))
}
