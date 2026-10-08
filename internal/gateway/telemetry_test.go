package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/provider/fake"
	"github.com/abdullah-9211/spillway/internal/telemetry"
	"github.com/abdullah-9211/spillway/internal/usage"
)

func scrape(t *testing.T, m *telemetry.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	return string(body)
}

func TestMetrics(t *testing.T) {
	r := newExecRig(t)
	m := telemetry.NewMetrics()
	ex := newMemExact()
	r.gw.Use(Options{Observer: m, Exact: ex})
	m.SetBreakerSource(func() map[string]int {
		out := map[string]int{}
		for p, s := range r.gw.BreakerStates() {
			out[p] = int(s)
		}
		return out
	})

	r.p["p1"].Default = fake.Behavior{Status: 500}
	req := func() { // temperature 0 so the second one is a cache hit
		q := userReq("chain")
		z := 0.0
		q.Temperature = &z
		r.gw.Chat(context.Background(), testKey, uuid.New(), "", q)
	}
	req() // p1 fails 3 times, p2 answers
	r.gw.WaitForFills()
	req() // exact hit

	out := scrape(t, m)
	for _, want := range []string{
		`spillway_gateway_requests_total{cache="miss",model="m2",outcome="ok",policy="chain",provider="p2"} 1`,
		`spillway_gateway_requests_total{cache="hit_exact",model="m2",outcome="ok",policy="chain",provider="p2"} 1`,
		`spillway_provider_attempts_total{error="server",kind="primary",provider="p1"} 1`,
		`spillway_provider_attempts_total{error="server",kind="retry",provider="p1"} 2`,
		`spillway_provider_attempts_total{error="",kind="fallback",provider="p2"} 1`,
		`spillway_cache_hits_total{kind="exact"} 1`,
		`spillway_gateway_overhead_seconds_count 1`, // only the cache hit: the miss had retries, so its figure would include backoff
		`spillway_gateway_latency_seconds_count 2`,
		`spillway_breaker_state{provider="p1"} 0`,
		`spillway_cost_usd_total{key="`,
		`# TYPE spillway_gateway_overhead_seconds histogram`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics output is missing %q", want)
		}
	}
}

func TestBreakerStateIsExported(t *testing.T) {
	r := newExecRig(t, func(c *Catalog) { c.Exec.MaxRetries = 0 })
	r.gw.exec.cfg.MaxRetries = 0
	m := telemetry.NewMetrics()
	m.SetBreakerSource(func() map[string]int {
		out := map[string]int{}
		for p, s := range r.gw.BreakerStates() {
			out[p] = int(s)
		}
		return out
	})
	r.p["p1"].Default = fake.Behavior{Status: 500}
	for i := 0; i < 5; i++ {
		r.gw.Chat(context.Background(), testKey, uuid.New(), "", userReq("chain"))
	}
	if out := scrape(t, m); !strings.Contains(out, `spillway_breaker_state{provider="p1"} 1`) {
		t.Errorf("an open breaker must read 1:\n%s", out)
	}
}

func TestSpans(t *testing.T) {
	r := newExecRig(t)
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	r.gw.Use(Options{Tracer: tp.Tracer("test")})
	r.p["p1"].Default = fake.Behavior{Status: 500}

	if _, err := r.gw.Chat(context.Background(), testKey, uuid.New(), "", userReq("chain")); err != nil {
		t.Fatal(err)
	}
	spans := sr.Ended()
	var names []string
	var root sdktrace.ReadOnlySpan
	for _, s := range spans {
		names = append(names, s.Name())
		if s.Name() == "gateway.request" {
			root = s
		}
	}
	sort.Strings(names)
	// One request span, and one span per provider call: p1 three times (original plus two retries), p2 once.
	if got := strings.Join(names, ","); got != "gateway.attempt,gateway.attempt,gateway.attempt,gateway.attempt,gateway.request" {
		t.Fatalf("spans = %s", got)
	}
	for _, s := range spans {
		if s.Name() == "gateway.attempt" && s.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Error("attempt spans must be children of the request span")
		}
	}
	attr := func(s sdktrace.ReadOnlySpan, key string) string {
		for _, a := range s.Attributes() {
			if string(a.Key) == key {
				return a.Value.String()
			}
		}
		return ""
	}
	if attr(root, "spillway.outcome") != "ok" || attr(root, "spillway.model") != "m2" || attr(root, "spillway.policy") != "chain" {
		t.Errorf("request span attributes: %v", root.Attributes())
	}
}

// Guards the cost of Spillway's own code on the request path. The provider answers instantly, so the whole
// request time is Spillway's. The budget is deliberately loose (about 20 times what a laptop sees) so it
// catches regressions like an accidental lock or a synchronous write, not machine noise. The real
// numbers come from the k6 load test in Phase 13.
func TestGatewayOverheadStaysWithinBudget(t *testing.T) {
	gw, p, _ := newGW(t)
	p.Default = fake.Behavior{Text: "ok"}
	gw.Use(Options{Exact: newMemExact(), Observer: telemetry.NewMetrics()})
	rec := &discardRecorder{}
	gw.rec = rec

	const n = 2000
	durs := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		if _, err := gw.Chat(context.Background(), testKey, uuid.New(), "", userReq("m")); err != nil {
			t.Fatal(err)
		}
		durs = append(durs, time.Since(start))
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	var sum time.Duration
	for _, d := range durs {
		sum += d
	}
	mean, p99 := sum/n, durs[n*99/100]
	t.Logf("overhead per request: mean %v, p50 %v, p99 %v", mean, durs[n/2], p99)
	if mean > 2*time.Millisecond || p99 > 10*time.Millisecond {
		t.Errorf("gateway overhead regressed: mean %v (budget 2ms), p99 %v (budget 10ms)", mean, p99)
	}
}

type discardRecorder struct{}

func (*discardRecorder) Record(usage.Row) {}

func BenchmarkGatewayChat(b *testing.B) {
	cat, _ := ParseCatalog([]byte(testCatalog))
	p := fake.New("fake")
	p.Default = fake.Behavior{Text: "ok"}
	gw := New(cat, map[string]provider.Provider{"fake": p}, nil, &discardRecorder{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := userReq("m")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := gw.Chat(context.Background(), testKey, uuid.New(), "", req); err != nil {
			b.Fatal(err)
		}
	}
}
