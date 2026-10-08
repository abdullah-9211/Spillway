package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/provider/fake"
	"github.com/abdullah-9211/spillway/internal/usage"
)

const execCatalog = `
providers:
  p1: { type: openai, base_url: "http://p1" }
  p2: { type: openai, base_url: "http://p2" }
  p3: { type: openai, base_url: "http://p3" }
models:
  - { id: m1, provider: p1, upstream: u1, input_usd_per_mtok: 1, output_usd_per_mtok: 1, context_window: 8000 }
  - { id: m2, provider: p2, upstream: u2, input_usd_per_mtok: 1, output_usd_per_mtok: 1, context_window: 8000 }
  - { id: m3, provider: p3, upstream: u3, input_usd_per_mtok: 1, output_usd_per_mtok: 1, context_window: 100000 }
policies:
  - { name: chain,  type: fallback, models: [m1, m2] }
  - { name: chain3, type: fallback, models: [m1, m2, m3] }
  - { name: hedged, type: fallback, models: [m1, m2], hedge_after_ms: 30 }
gateway:
  request_timeout: 2s
  first_byte_timeout: 300ms
  idle_timeout: 300ms
  max_total_ms: 5000
  max_retries: 2
breaker: { consecutive_failures: 5, window: 20, error_rate: 0.5, open_for: 30s }
`

type rig struct {
	gw    *Gateway
	p     map[string]*fake.Provider
	rec   *memRecorder
	clk   *clock
	sleep []time.Duration
	mu    sync.Mutex
}

func newExecRig(t *testing.T, mutate ...func(*Catalog)) *rig {
	t.Helper()
	cat, err := ParseCatalog([]byte(execCatalog))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range mutate {
		m(cat)
	}
	r := &rig{p: map[string]*fake.Provider{}, rec: &memRecorder{}, clk: &clock{t: time.Now()}}
	provs := map[string]provider.Provider{}
	for _, n := range []string{"p1", "p2", "p3"} {
		r.p[n] = fake.New(n)
		provs[n] = r.p[n]
	}
	r.gw = New(cat, provs, nil, r.rec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Fake clock for the breakers, and a sleep that records instead of waiting.
	r.gw.breakers = NewBreakers(cat.Breaker, r.clk.now)
	r.gw.exec.breakers = r.gw.breakers
	r.gw.exec.sleep = func(ctx context.Context, d time.Duration) error {
		r.mu.Lock()
		r.sleep = append(r.sleep, d)
		r.mu.Unlock()
		return ctx.Err()
	}
	r.gw.exec.jitter = func() float64 { return 1 }
	return r
}

func (r *rig) chat(t *testing.T, model string) (*Result, error) {
	t.Helper()
	return r.gw.Chat(context.Background(), testKey, uuid.New(), "", userReq(model))
}

func attemptKinds(as []usage.Attempt) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.Provider+":"+a.Kind)
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBackoffBounds(t *testing.T) {
	base, cap := 200*time.Millisecond, 5*time.Second
	tests := []struct {
		n      int
		jitter float64
		want   time.Duration
	}{
		{0, 1, 200 * time.Millisecond}, {0, 0.5, 100 * time.Millisecond},
		{1, 1, 400 * time.Millisecond}, {2, 1, 800 * time.Millisecond},
		{4, 1, 3200 * time.Millisecond}, {5, 1, 5 * time.Second}, {40, 1, 5 * time.Second}, {40, 0.5, 2500 * time.Millisecond},
	}
	for _, tc := range tests {
		if got := Backoff(tc.n, base, cap, tc.jitter); got != tc.want {
			t.Errorf("Backoff(%d, jitter %.1f) = %v, want %v", tc.n, tc.jitter, got, tc.want)
		}
	}
	for n := 0; n < 20; n++ {
		for _, j := range []float64{0.5, 0.75, 1} {
			d := Backoff(n, base, cap, j)
			if d <= 0 || d > cap {
				t.Errorf("Backoff(%d, %.2f) = %v outside (0, %v]", n, j, d, cap)
			}
		}
	}
}

func TestRetryOn429ThenSuccess(t *testing.T) {
	r := newExecRig(t)
	r.p["p1"].Script(fake.Behavior{Status: 429}, fake.Behavior{Status: 429}, fake.Behavior{Text: "finally"})
	res, err := r.chat(t, "chain")
	if err != nil {
		t.Fatal(err)
	}
	row := r.rec.only(t)
	if got := attemptKinds(row.Attempts); !eq(got, []string{"p1:primary", "p1:retry", "p1:retry"}) {
		t.Errorf("attempts = %v", got)
	}
	if res.Provider != "p1" || res.Attempts != 3 || row.Outcome != usage.OutcomeOK {
		t.Errorf("result %+v row %+v", res, row)
	}
	// Backoff doubles from the base with jitter pinned at 1: 200ms then 400ms.
	if len(r.sleep) != 2 || r.sleep[0] != 200*time.Millisecond || r.sleep[1] != 400*time.Millisecond {
		t.Errorf("sleeps = %v", r.sleep)
	}
	if row.Attempts[0].ErrorKind != "rate_limited" || row.Attempts[0].Status != 429 {
		t.Errorf("failed attempt should carry its kind and status: %+v", row.Attempts[0])
	}
}

func TestRetryAfterOverridesBackoff(t *testing.T) {
	r := newExecRig(t)
	r.p["p1"].Script(fake.Behavior{Status: 429, RetryAfter: 3 * time.Second}, fake.Behavior{Text: "ok"})
	if _, err := r.chat(t, "chain"); err != nil {
		t.Fatal(err)
	}
	if len(r.sleep) != 1 || r.sleep[0] != 3*time.Second {
		t.Errorf("sleeps = %v, want [3s]", r.sleep)
	}
}

func TestRetryAfterBeyondBudgetSkipsTheCandidate(t *testing.T) {
	r := newExecRig(t)
	r.p["p1"].Script(fake.Behavior{Status: 429, RetryAfter: time.Hour})
	res, err := r.chat(t, "chain")
	if err != nil {
		t.Fatal(err)
	}
	if res.Provider != "p2" || len(r.sleep) != 0 {
		t.Errorf("should fall over to p2 without waiting an hour: provider=%s sleeps=%v", res.Provider, r.sleep)
	}
}

func TestPersistentFailureFallsBack(t *testing.T) {
	r := newExecRig(t)
	r.p["p1"].Default = fake.Behavior{Status: 500}
	res, err := r.chat(t, "chain")
	if err != nil {
		t.Fatal(err)
	}
	row := r.rec.only(t)
	want := []string{"p1:primary", "p1:retry", "p1:retry", "p2:fallback"}
	if got := attemptKinds(row.Attempts); !eq(got, want) {
		t.Errorf("attempts = %v, want %v", got, want)
	}
	if res.Provider != "p2" || row.Provider != "p2" || row.Model != "m2" || row.Policy != "chain" {
		t.Errorf("the model that answered is recorded, the policy as asked: %+v", row)
	}
	if got := r.p["p1"].Requests()[0].Model; got != "u1" {
		t.Errorf("p1 saw model %q", got)
	}
}

func TestAllProvidersFailed(t *testing.T) {
	r := newExecRig(t)
	r.p["p1"].Default = fake.Behavior{Status: 500}
	r.p["p2"].Default = fake.Behavior{Status: 503}
	_, err := r.chat(t, "chain")
	var ge *Error
	if !errors.As(err, &ge) || ge.Status != 503 || ge.Code != "all_providers_failed" || len(ge.Attempts) != 6 {
		t.Fatalf("err = %v (%+v)", err, ge)
	}
	if row := r.rec.only(t); row.Outcome != usage.OutcomeAllFailed || row.Cost != 0 {
		t.Errorf("row: %+v", row)
	}
}

func TestBadRequestIsNeverRetriedOrFailedOver(t *testing.T) {
	r := newExecRig(t)
	r.p["p1"].Default = fake.Behavior{Status: 400}
	_, err := r.chat(t, "chain")
	var ge *Error
	if !errors.As(err, &ge) || ge.Status != 400 {
		t.Fatalf("err = %v", err)
	}
	if n := len(r.p["p1"].Requests()); n != 1 {
		t.Errorf("p1 called %d times, want 1", n)
	}
	if n := len(r.p["p2"].Requests()); n != 0 {
		t.Errorf("a bad request must not fail over, p2 called %d times", n)
	}
	if br := r.gw.breakers.For("p1").State(); br != BreakerClosed {
		t.Errorf("bad requests do not count against the provider: %s", br)
	}
}

func TestAuthFailureFailsOverWithoutRetry(t *testing.T) {
	r := newExecRig(t)
	r.p["p1"].Default = fake.Behavior{Status: 401}
	res, err := r.chat(t, "chain")
	if err != nil || res.Provider != "p2" {
		t.Fatalf("%v %v", res, err)
	}
	if n := len(r.p["p1"].Requests()); n != 1 {
		t.Errorf("auth failures are not retried, p1 called %d times", n)
	}
}

func TestNoFailoverToSmallerContextWindow(t *testing.T) {
	// p1 reports the prompt as too long. m2 has the same window, so it is skipped; m3 is larger and is used.
	r := newExecRig(t)
	r.gw.exec.providers["p1"] = ctxLenProvider{r.p["p1"]}
	res, err := r.chat(t, "chain3")
	if err != nil {
		t.Fatal(err)
	}
	if res.Provider != "p3" {
		t.Fatalf("answered by %s, want p3", res.Provider)
	}
	if n := len(r.p["p2"].Requests()); n != 0 {
		t.Errorf("p2 has no larger window and must not be tried, got %d calls", n)
	}
	if got := attemptKinds(r.rec.only(t).Attempts); !eq(got, []string{"p1:primary", "p2:skipped", "p3:fallback"}) {
		t.Errorf("attempts = %v", got)
	}
}

type ctxLenProvider struct{ *fake.Provider }

func (c ctxLenProvider) Chat(context.Context, *provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, &provider.ProviderError{Kind: provider.KindContextLength, Status: 400, Err: errors.New("too long")}
}

func TestBreakerOpensAndSkipsDeadProvider(t *testing.T) {
	r := newExecRig(t, func(c *Catalog) { c.Exec.MaxRetries = 0 })
	r.gw.exec.cfg.MaxRetries = 0
	r.p["p1"].Default = fake.Behavior{Status: 500}

	for i := 0; i < 5; i++ { // five consecutive failures open p1's breaker; each request still succeeds on p2
		if res, err := r.chat(t, "chain"); err != nil || res.Provider != "p2" {
			t.Fatalf("request %d: %v %v", i, res, err)
		}
	}
	if st := r.gw.breakers.For("p1").State(); st != BreakerOpen {
		t.Fatalf("breaker = %s, want open", st)
	}

	before := len(r.p["p1"].Requests())
	r.rec.rows = nil
	if _, err := r.chat(t, "chain"); err != nil {
		t.Fatal(err)
	}
	if got := attemptKinds(r.rec.only(t).Attempts); !eq(got, []string{"p1:skipped", "p2:fallback"}) {
		t.Errorf("attempts = %v", got)
	}
	if len(r.p["p1"].Requests()) != before {
		t.Error("an open breaker must not call the provider")
	}

	// After the wait, one probe goes through; p1 has recovered, so the breaker closes.
	r.clk.advance(31 * time.Second)
	r.p["p1"].Default = fake.Behavior{}
	res, err := r.chat(t, "chain")
	if err != nil || res.Provider != "p1" {
		t.Fatalf("probe: %v %v", res, err)
	}
	if st := r.gw.breakers.For("p1").State(); st != BreakerClosed {
		t.Errorf("breaker = %s, want closed after a good probe", st)
	}
}

func TestHedgeBeatsASlowFirstAttempt(t *testing.T) {
	r := newExecRig(t)
	r.p["p1"].Default = fake.Behavior{Delay: 2 * time.Second, Text: "slow"}
	r.p["p2"].Default = fake.Behavior{Text: "fast"}

	start := time.Now()
	res, err := r.chat(t, "hedged")
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("hedge should answer in ~30ms, took %v", took)
	}
	if res.Provider != "p2" || res.Response.Choices[0].Message.Content.PlainText() != "fast" {
		t.Errorf("winner = %s", res.Provider)
	}
	row := r.rec.only(t)
	if got := attemptKinds(row.Attempts); !eq(got, []string{"p1:primary", "p2:hedge"}) {
		t.Errorf("attempts = %v", got)
	}
	if loser := row.Attempts[0]; loser.ErrorKind != "canceled" {
		t.Errorf("the slow loser should be recorded as cancelled: %+v", loser)
	}
	deadline := time.Now().Add(time.Second)
	for r.p["p1"].Canceled() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if r.p["p1"].Canceled() != 1 {
		t.Error("the loser's upstream request must be cancelled")
	}
}

func TestHedgeNotFiredWhenFirstIsFastEnough(t *testing.T) {
	r := newExecRig(t)
	if _, err := r.chat(t, "hedged"); err != nil {
		t.Fatal(err)
	}
	if n := len(r.p["p2"].Requests()); n != 0 {
		t.Errorf("p2 called %d times, want 0", n)
	}
}

func TestHedgedRequestStillFailsOverIfBothFail(t *testing.T) {
	r := newExecRig(t, func(c *Catalog) { c.Exec.MaxRetries = 0 })
	r.gw.exec.cfg.MaxRetries = 0
	r.p["p1"].Default = fake.Behavior{Delay: 100 * time.Millisecond, Status: 500}
	r.p["p2"].Default = fake.Behavior{Status: 500}
	_, err := r.chat(t, "hedged")
	var ge *Error
	if !errors.As(err, &ge) || ge.Code != "all_providers_failed" {
		t.Fatalf("err = %v", err)
	}
}

// The executor must never run past the request-level deadline, whatever the providers do.
func TestNeverExceedsRequestDeadline(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 60; i++ {
		r := newExecRig(t, func(c *Catalog) {
			c.Exec.MaxTotal = 150 * time.Millisecond
			c.Exec.RequestTimeout = time.Duration(20+rng.IntN(300)) * time.Millisecond
			c.Exec.MaxRetries = rng.IntN(4)
		})
		r.gw.exec.cfg = r.gw.cat.Exec
		r.gw.exec.sleep = sleepCtx // real sleeps: the deadline must cut them short
		r.gw.exec.jitter = func() float64 { return 1 }
		for _, n := range []string{"p1", "p2"} {
			b := fake.Behavior{Delay: time.Duration(rng.IntN(400)) * time.Millisecond}
			switch rng.IntN(4) {
			case 0:
				b.Status = 429
				b.RetryAfter = time.Duration(rng.IntN(500)) * time.Millisecond
			case 1:
				b.Status = 500
			case 2:
				b.Status = 503
			}
			r.p[n].Default = b
		}
		start := time.Now()
		_, _ = r.chat(t, []string{"chain", "hedged"}[rng.IntN(2)])
		if took := time.Since(start); took > 150*time.Millisecond+120*time.Millisecond {
			t.Fatalf("iteration %d took %v with a 150ms deadline", i, took)
		}
	}
}

func TestClientCancelStopsRetrying(t *testing.T) {
	r := newExecRig(t)
	r.p["p1"].Default = fake.Behavior{Delay: time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := r.gw.Chat(ctx, testKey, uuid.New(), "", userReq("chain"))
	var ge *Error
	if !errors.As(err, &ge) || ge.Status != 499 {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Error("cancel must stop work promptly")
	}
	if n := len(r.p["p2"].Requests()); n != 0 {
		t.Errorf("a cancelled request must not fail over, p2 called %d times", n)
	}
	if row := r.rec.only(t); row.Outcome != usage.OutcomeClientCancelled {
		t.Errorf("outcome = %s", row.Outcome)
	}
}

// --- streaming: the rows of the section 3.5 table that live in the gateway ---

func (r *rig) stream(t *testing.T, model string) (*Stream, error) {
	t.Helper()
	req := userReq(model)
	req.Stream = true
	return r.gw.ChatStream(context.Background(), testKey, uuid.New(), "", req)
}

func TestStreamFailureBeforeFirstByteFailsOverTransparently(t *testing.T) {
	r := newExecRig(t)
	r.p["p1"].Default = fake.Behavior{Status: 500}
	r.p["p2"].Default = fake.Behavior{Text: "from p2"}
	st, err := r.stream(t, "chain")
	if err != nil {
		t.Fatal(err)
	}
	chunks, derr := drainStream(t, st)
	if derr != nil || len(chunks) == 0 {
		t.Fatalf("client should see a normal stream: %v", derr)
	}
	st.Finish(context.Background(), nil)
	row := r.rec.only(t)
	if got := attemptKinds(row.Attempts); !eq(got, []string{"p1:primary", "p1:retry", "p1:retry", "p2:fallback"}) || row.Provider != "p2" {
		t.Errorf("attempts = %v provider=%s", got, row.Provider)
	}
	if st.Provider() != "p2" {
		t.Errorf("headers should name the provider that answers: %s", st.Provider())
	}
}

func TestStreamFirstByteTimeoutFailsOver(t *testing.T) {
	r := newExecRig(t, func(c *Catalog) { c.Exec.MaxRetries = 0 })
	r.gw.exec.cfg = r.gw.cat.Exec
	r.p["p1"].Default = fake.Behavior{Delay: 2 * time.Second}
	start := time.Now()
	st, err := r.stream(t, "chain")
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("first-byte timeout is 300ms, took %v", took)
	}
	st.Finish(context.Background(), nil)
	row := r.rec.only(t)
	if row.Attempts[0].ErrorKind != "timeout" || row.Provider != "p2" {
		t.Errorf("row: %+v", row)
	}
}

func TestStreamIdleTimeoutEndsAStalledStream(t *testing.T) {
	r := newExecRig(t)
	r.p["p1"].Default = fake.Behavior{Text: "hello", Hang: true}
	st, err := r.stream(t, "chain")
	if err != nil {
		t.Fatal(err)
	}
	_, derr := drainStream(t, st)
	if provider.KindOf(derr) != provider.KindTimeout {
		t.Fatalf("a stalled stream must end with a timeout, got %v", derr)
	}
	st.Finish(context.Background(), derr)
	if row := r.rec.only(t); row.Outcome != usage.OutcomeUpstreamError {
		t.Errorf("outcome = %s", row.Outcome)
	}
	// No failover once bytes were sent.
	if n := len(r.p["p2"].Requests()); n != 0 {
		t.Errorf("p2 called %d times", n)
	}
}

func TestStreamAllProvidersFailedBeforeFirstByte(t *testing.T) {
	r := newExecRig(t, func(c *Catalog) { c.Exec.MaxRetries = 0 })
	r.gw.exec.cfg = r.gw.cat.Exec
	r.p["p1"].Default = fake.Behavior{Status: 500}
	r.p["p2"].Default = fake.Behavior{Status: 500}
	_, err := r.stream(t, "chain")
	var ge *Error
	if !errors.As(err, &ge) || ge.Code != "all_providers_failed" || len(ge.Attempts) != 2 {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamsAreNotHedged(t *testing.T) {
	r := newExecRig(t)
	r.p["p1"].Default = fake.Behavior{Delay: 100 * time.Millisecond}
	st, err := r.stream(t, "hedged")
	if err != nil {
		t.Fatal(err)
	}
	st.Finish(context.Background(), nil)
	if n := len(r.p["p2"].Requests()); n != 0 {
		t.Errorf("streams are never hedged, p2 called %d times", n)
	}
}
