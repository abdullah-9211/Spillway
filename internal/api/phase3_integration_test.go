//go:build integration

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/abdullah-9211/spillway/internal/db"
	"github.com/abdullah-9211/spillway/internal/db/sqlcgen"
	"github.com/abdullah-9211/spillway/internal/gateway"
	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/money"
	"github.com/abdullah-9211/spillway/internal/provider/fake"
	"github.com/abdullah-9211/spillway/internal/telemetry"
)

// unit768 is a unit vector in the first two dimensions, padded to the 768 the table expects. The cosine
// similarity of two of them is cos(angle between).
func unit768(deg float64) []float32 {
	v := make([]float32, 768)
	r := deg * math.Pi / 180
	v[0], v[1] = float32(math.Cos(r)), float32(math.Sin(r))
	return v
}

type tableEmbedder struct {
	mu   sync.Mutex
	vecs map[string][]float32
	fail bool
}

func (e *tableEmbedder) Embed(_ context.Context, _ string, in []string) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fail {
		return nil, errors.New("ollama is down")
	}
	v, ok := e.vecs[in[0]]
	if !ok {
		return nil, fmt.Errorf("no vector for %q", in[0])
	}
	return [][]float32{v}, nil
}

func redisClient(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Fatal("REDIS_URL is not set; run `make up` and use `make test`")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	c := redis.NewClient(opts)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

type full struct {
	*stack
	emb *tableEmbedder
}

// newFull is the gateway with every Phase 3 feature on, against real Redis and Postgres (with pgvector).
func newFull(t *testing.T) *full {
	t.Helper()
	emb := &tableEmbedder{vecs: map[string][]float32{}}
	rc := redisClient(t)
	s := newStackWith(t, func(pg *db.Postgres) gateway.Options {
		return gateway.Options{
			Limiter:    gateway.NewRedisLimiter(rc),
			Exact:      gateway.NewRedisCache(rc),
			Spend:      gateway.NewPGSpend(sqlcgen.New(pg.Pool)),
			Semantic:   gateway.NewPGSemantic(pg.Pool),
			Embedder:   emb,
			EmbedModel: "test-embed",
			Observer:   telemetry.NewMetrics(),
		}
	})
	return &full{stack: s, emb: emb}
}

func (f *full) newKeyWith(t *testing.T, name string, p keys.CreateParams) (keys.Key, string) {
	t.Helper()
	p.Name = name
	k, tok, err := f.keys.Create(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return k, tok
}

func askBody(model, q string) string {
	b, _ := json.Marshal(map[string]any{"model": model, "temperature": 0, "messages": []map[string]string{{"role": "user", "content": q}}})
	return string(b)
}

func TestReplayWorkloadTwiceHitsExactCache(t *testing.T) {
	f := newFull(t)
	k, tok := f.newKeyWith(t, "replay", keys.CreateParams{})
	f.prov.Default = fake.Behavior{Text: "answer", InputTokens: 1000, OutputTokens: 100} // $0.0045 each
	prompts := []string{"alpha", "bravo", "charlie", "delta", "echo"}

	var firstIDs []string
	for _, q := range prompts {
		resp := f.do(t, tok, askBody("m", q))
		if resp.StatusCode != 200 || resp.Header.Get("X-Spillway-Cache") != "miss" {
			t.Fatalf("pass 1 %q: %d cache=%s", q, resp.StatusCode, resp.Header.Get("X-Spillway-Cache"))
		}
		firstIDs = append(firstIDs, resp.Header.Get("X-Spillway-Request-Id"))
	}
	f.gw.WaitForFills()

	var hitIDs []string
	for _, q := range prompts {
		resp := f.do(t, tok, askBody("m", q))
		if resp.Header.Get("X-Spillway-Cache") != "hit-exact" || resp.Header.Get("X-Spillway-Cost-Usd") != "0.000000" {
			t.Fatalf("pass 2 %q: cache=%s cost=%s", q, resp.Header.Get("X-Spillway-Cache"), resp.Header.Get("X-Spillway-Cost-Usd"))
		}
		hitIDs = append(hitIDs, resp.Header.Get("X-Spillway-Request-Id"))
	}
	if n := len(f.prov.Requests()); n != 5 {
		t.Errorf("provider saw %d requests over two passes, want 5", n)
	}

	for _, id := range hitIDs {
		row := f.usageRow(t, id)
		cost, _ := db.MicrosFromNumeric(row.CostUsd)
		saved, _ := db.MicrosFromNumeric(row.SavedUsd)
		if row.CacheStatus != "hit_exact" || cost != 0 || saved != 4500 || row.InputTokens != 0 {
			t.Errorf("hit row: cache=%s cost=%d saved=%d tokens=%d", row.CacheStatus, cost, saved, row.InputTokens)
		}
	}
	f.usageRow(t, firstIDs[0])
	day, err := f.q.GetUsageDaily(context.Background(), sqlcgen.GetUsageDailyParams{
		ApiKeyID: k.ID, Day: pgtype.Date{Time: time.Now().UTC().Truncate(24 * time.Hour), Valid: true}, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	dc, _ := db.MicrosFromNumeric(day.CostUsd)
	ds, _ := db.MicrosFromNumeric(day.SavedUsd)
	if day.Requests != 10 || day.CacheHits != 5 || dc != 22500 || ds != 22500 {
		t.Errorf("rollup: requests=%d hits=%d cost=%d saved=%d (want 10, 5, 22500, 22500)", day.Requests, day.CacheHits, dc, ds)
	}
}

func TestSemanticParaphraseHitAgainstPgvector(t *testing.T) {
	f := newFull(t)
	_, tok := f.newKeyWith(t, "semantic", keys.CreateParams{})
	// semantic_cache is a per-key flag; the Phase 5 admin API will set it. Set it directly for now.
	if _, err := f.keys.SetSemanticCache(context.Background(), tok, true); err != nil {
		t.Fatal(err)
	}
	f.emb.vecs = map[string][]float32{
		"what is 2+2": unit768(0), "whats two plus two": unit768(10), // cosine 0.985
		"just inside":       unit768(18), // 0.951
		"just outside":      unit768(19), // 0.945
		"capital of France": unit768(90),
	}
	f.prov.Default = fake.Behavior{Text: "four", InputTokens: 1000, OutputTokens: 100}

	if resp := f.do(t, tok, askBody("m", "what is 2+2")); resp.Header.Get("X-Spillway-Cache") != "miss" {
		t.Fatalf("first request cache=%s", resp.Header.Get("X-Spillway-Cache"))
	}
	f.gw.WaitForFills()

	tests := map[string]string{
		"what is 2+2": "hit-exact", "whats two plus two": "hit-semantic", "just inside": "hit-semantic",
		"just outside": "miss", "capital of France": "miss",
	}
	for q, want := range tests {
		resp := f.do(t, tok, askBody("m", q))
		if got := resp.Header.Get("X-Spillway-Cache"); got != want {
			t.Errorf("%q: cache=%s, want %s", q, got, want)
		}
		if want == "hit-semantic" {
			row := f.usageRow(t, resp.Header.Get("X-Spillway-Request-Id"))
			saved, _ := db.MicrosFromNumeric(row.SavedUsd)
			if row.CacheStatus != "hit_semantic" || saved != 4500 {
				t.Errorf("%q row: cache=%s saved=%d", q, row.CacheStatus, saved)
			}
		}
	}
}

func TestBudgetEnforcedFromRealUsage(t *testing.T) {
	f := newFull(t)
	budget := money.Micros(4500) // exactly one request's cost
	_, tok := f.newKeyWith(t, "budget", keys.CreateParams{MonthlyBudget: &budget})
	f.prov.Default = fake.Behavior{Text: "ok", InputTokens: 1000, OutputTokens: 100}

	if resp := f.do(t, tok, simple2("m")); resp.StatusCode != 200 {
		t.Fatalf("first request: %d", resp.StatusCode)
	}
	// This process charged its own spend straight away, so no waiting for the database.
	resp := f.do(t, tok, simple2("m"))
	e := errorOf(t, resp)
	if resp.StatusCode != 402 || e["code"] != "budget_exceeded" || e["type"] != "insufficient_quota" {
		t.Fatalf("second request: %d %v", resp.StatusCode, e)
	}
	if n := len(f.prov.Requests()); n != 1 {
		t.Errorf("an over-budget request reached the provider (%d calls)", n)
	}
	f.usageRow(t, resp.Header.Get("X-Spillway-Request-Id"))

	// A fresh process knows nothing in memory and must read usage_daily to reach the same answer.
	g := newFull(t)
	g.prov.Default = f.prov.Default
	deadline := time.Now().Add(5 * time.Second)
	var last *http.Response
	for time.Now().Before(deadline) {
		last = g.do(t, tok, simple2("m"))
		if last.StatusCode == 402 {
			return
		}
		time.Sleep(100 * time.Millisecond) // the first request's usage row is flushed in the background
	}
	t.Fatalf("a new instance should see the spend in usage_daily and reply 402, last status %d", last.StatusCode)
}

func simple2(model string) string {
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)
}

func errorOf(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var body struct {
		Error map[string]any `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Error
}

func TestRateLimitWithRealRedis(t *testing.T) {
	f := newFull(t)
	rpm := 2
	_, tok := f.newKeyWith(t, "ratelimit", keys.CreateParams{RateLimitRPM: &rpm})

	for i := 1; i <= 2; i++ {
		if resp := f.do(t, tok, simple2("m")); resp.StatusCode != 200 {
			t.Fatalf("request %d: %d", i, resp.StatusCode)
		}
	}
	resp := f.do(t, tok, simple2("m"))
	e := errorOf(t, resp)
	if resp.StatusCode != 429 || e["code"] != "rate_limit_exceeded" {
		t.Fatalf("third request: %d %v", resp.StatusCode, e)
	}
	// 2 requests per minute is one token per 30 seconds.
	if ra := resp.Header.Get("Retry-After"); ra == "" || ra == "0" {
		t.Errorf("Retry-After = %q", ra)
	}
	if n := len(f.prov.Requests()); n != 2 {
		t.Errorf("provider calls = %d, want 2", n)
	}
	row := f.usageRow(t, resp.Header.Get("X-Spillway-Request-Id"))
	if row.Outcome != "rate_limited" {
		t.Errorf("outcome = %s", row.Outcome)
	}

	// Streaming requests are limited the same way.
	if resp := f.do(t, tok, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`); resp.StatusCode != 429 {
		t.Errorf("streaming request: %d", resp.StatusCode)
	}
}

func TestRedisDownDoesNotBreakTheGateway(t *testing.T) {
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = dead.Close() })
	s := newStackWith(t, func(pg *db.Postgres) gateway.Options {
		return gateway.Options{Limiter: gateway.NewRedisLimiter(dead), Exact: gateway.NewRedisCache(dead)}
	})
	rpm := 1
	_, tok := (&full{stack: s}).newKeyWith(t, "redis-down", keys.CreateParams{RateLimitRPM: &rpm})
	for i := 0; i < 3; i++ {
		resp := s.do(t, tok, askBody("m", "q"))
		if resp.StatusCode != 200 {
			t.Fatalf("request %d with Redis down: %d", i+1, resp.StatusCode)
		}
		if resp.Header.Get("X-Spillway-Cache") != "bypass" {
			t.Errorf("cache header = %s", resp.Header.Get("X-Spillway-Cache"))
		}
	}
}

func TestMetricsEndpoint(t *testing.T) {
	f := newFull(t)
	_, tok := f.newKeyWith(t, "metrics", keys.CreateParams{})
	f.do(t, tok, askBody("m", "metrics please"))

	resp, err := http.Get(f.srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{
		"spillway_gateway_overhead_seconds_bucket", "spillway_gateway_latency_seconds_bucket",
		"spillway_gateway_requests_total", "spillway_provider_attempts_total", "go_goroutines",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics is missing %s", want)
		}
	}
}
