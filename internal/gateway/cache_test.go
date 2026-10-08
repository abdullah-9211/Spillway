package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/money"
	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/provider/fake"
	"github.com/abdullah-9211/spillway/internal/telemetry"
	"github.com/abdullah-9211/spillway/internal/usage"
)

// --- in-memory fakes for the things that normally need Redis, Postgres and Ollama ---

type memExact struct {
	mu      sync.Mutex
	m       map[string]*Entry
	gets    int
	sets    int
	failing bool
}

func newMemExact() *memExact { return &memExact{m: map[string]*Entry{}} }

func (c *memExact) Get(_ context.Context, k string) (*Entry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets++
	if c.failing {
		return nil, errors.New("redis is down")
	}
	return c.m[k], nil
}

func (c *memExact) Set(_ context.Context, k string, e *Entry, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sets++
	if c.failing {
		return errors.New("redis is down")
	}
	c.m[k] = e
	return nil
}

type memSemantic struct {
	mu      sync.Mutex
	entries []semEntry
}

type semEntry struct {
	scope uuid.UUID
	group string
	vec   []float32
	e     *Entry
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func (s *memSemantic) Lookup(_ context.Context, scope uuid.UUID, group string, v []float32, threshold float64) (*Entry, float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *Entry
	bestSim := -1.0
	for _, x := range s.entries {
		if x.scope != scope || x.group != group {
			continue
		}
		if sim := cosine(v, x.vec); sim > bestSim {
			best, bestSim = x.e, sim
		}
	}
	if best != nil && IsHit(bestSim, threshold) {
		return best, bestSim, nil
	}
	return nil, bestSim, nil
}

func (s *memSemantic) Store(_ context.Context, scope uuid.UUID, group string, v []float32, e *Entry, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, semEntry{scope, group, v, e})
	return nil
}

type tableEmbedder struct {
	vecs  map[string][]float32
	calls int
	fail  bool
}

func (t *tableEmbedder) Embed(_ context.Context, _ string, in []string) ([][]float32, error) {
	t.calls++
	if t.fail {
		return nil, errors.New("ollama is down")
	}
	v, ok := t.vecs[in[0]]
	if !ok {
		return nil, errors.New("no vector for " + in[0])
	}
	return [][]float32{v}, nil
}

type recObserver struct {
	mu       sync.Mutex
	requests []telemetry.RequestEvent
	attempts []string
	deps     []string
}

func (o *recObserver) ObserveRequest(e telemetry.RequestEvent) {
	o.mu.Lock()
	o.requests = append(o.requests, e)
	o.mu.Unlock()
}
func (o *recObserver) ObserveAttempt(p, k, e string) {
	o.mu.Lock()
	o.attempts = append(o.attempts, p+"/"+k+"/"+e)
	o.mu.Unlock()
}
func (o *recObserver) DependencyError(d string) {
	o.mu.Lock()
	o.deps = append(o.deps, d)
	o.mu.Unlock()
}

type stubLimiter struct {
	ok    bool
	retry time.Duration
	err   error
	calls int
}

func (l *stubLimiter) Allow(context.Context, uuid.UUID, int) (bool, time.Duration, error) {
	l.calls++
	return l.ok, l.retry, l.err
}

type stubSpend struct {
	spend money.Micros
	err   error
	calls int
}

func (s *stubSpend) MonthToDate(context.Context, uuid.UUID, time.Time) (money.Micros, error) {
	s.calls++
	return s.spend, s.err
}

// --- helpers ---

func f64(v float64) *float64 { return &v }

func tempReq(model string, temp *float64, text string) *provider.ChatRequest {
	r := userReq(model)
	r.Messages[0].Content = provider.TextContent(text)
	r.Temperature = temp
	return r
}

func newCacheGW(t *testing.T) (*Gateway, *fake.Provider, *memRecorder, *memExact) {
	t.Helper()
	gw, p, rec := newGW(t)
	p.Default = fake.Behavior{Text: "four", InputTokens: 1000, OutputTokens: 100}
	ex := newMemExact()
	gw.Use(Options{Exact: ex})
	return gw, p, rec, ex
}

func chatKey(gw *Gateway, key keys.Key, req *provider.ChatRequest) (*Result, error) {
	return gw.Chat(context.Background(), key, uuid.New(), "", req)
}

// --- cache key canonicalisation ---

func TestCacheKeyCanonicalisation(t *testing.T) {
	base := func() *provider.ChatRequest {
		return &provider.ChatRequest{Model: "p", Temperature: f64(0), Messages: []provider.Message{
			{Role: "system", Content: provider.TextContent("be brief")}, {Role: "user", Content: provider.TextContent("hi")}}}
	}
	k := func(r *provider.ChatRequest) string {
		got, err := CacheKey("scope", r)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	ref := k(base())

	same := map[string]func(*provider.ChatRequest){
		"stream is ignored":         func(r *provider.ChatRequest) { r.Stream = true },
		"stream_options is ignored": func(r *provider.ChatRequest) { r.StreamOptions = &provider.StreamOptions{IncludeUsage: true} },
		"user is ignored":           func(r *provider.ChatRequest) { r.User = "alice" },
		"n is ignored":              func(r *provider.ChatRequest) { n := 1; r.N = &n },
		"a string and a single text part are the same": func(r *provider.ChatRequest) {
			r.Messages[1].Content = provider.Content{Parts: []provider.ContentPart{{Type: "text", Text: "hi"}}}
		},
	}
	for name, mutate := range same {
		r := base()
		mutate(r)
		if k(r) != ref {
			t.Errorf("%s: key changed", name)
		}
	}

	differ := map[string]func(*provider.ChatRequest){
		"policy":        func(r *provider.ChatRequest) { r.Model = "q" },
		"message text":  func(r *provider.ChatRequest) { r.Messages[1].Content = provider.TextContent("hello") },
		"message order": func(r *provider.ChatRequest) { r.Messages[0], r.Messages[1] = r.Messages[1], r.Messages[0] },
		"role":          func(r *provider.ChatRequest) { r.Messages[0].Role = "user" },
		"temperature":   func(r *provider.ChatRequest) { r.Temperature = f64(0.5) },
		"top_p":         func(r *provider.ChatRequest) { r.TopP = f64(0.9) },
		"max_tokens":    func(r *provider.ChatRequest) { n := 10; r.MaxTokens = &n },
		"stop":          func(r *provider.ChatRequest) { r.Stop = provider.StringList{"x"} },
		"seed":          func(r *provider.ChatRequest) { n := 1; r.Seed = &n },
		"tools": func(r *provider.ChatRequest) {
			r.Tools = []provider.Tool{{Type: "function", Function: provider.ToolFunction{Name: "f"}}}
		},
		"response_format": func(r *provider.ChatRequest) { r.ResponseFormat = json.RawMessage(`{"type":"json_object"}`) },
	}
	for name, mutate := range differ {
		r := base()
		mutate(r)
		if k(r) == ref {
			t.Errorf("%s: key must change", name)
		}
	}

	if got, _ := CacheKey("other-scope", base()); got == ref {
		t.Error("the scope is part of the key")
	}
}

func TestCacheKeyIgnoresJSONKeyOrderInSchemas(t *testing.T) {
	mk := func(schema string) *provider.ChatRequest {
		return &provider.ChatRequest{Model: "p", Messages: []provider.Message{{Role: "user", Content: provider.TextContent("x")}},
			Tools: []provider.Tool{{Type: "function", Function: provider.ToolFunction{Name: "f", Parameters: json.RawMessage(schema)}}}}
	}
	a, _ := CacheKey("s", mk(`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"number"}}}`))
	b, _ := CacheKey("s", mk(`{"properties":{"b":{"type":"number"},"a":{"type":"string"}},"type":"object"}`))
	if a != b {
		t.Error("the same schema with keys in another order must hash the same")
	}
}

func TestCacheable(t *testing.T) {
	zero := 0.0
	tests := []struct {
		name string
		temp *float64
		key  keys.Key
		want bool
	}{
		{"temperature 0", &zero, keys.Key{}, true},
		{"temperature unset is not assumed to be 0", nil, keys.Key{}, false},
		{"temperature 0.7", f64(0.7), keys.Key{}, false},
		{"temperature 0.7 with the key's opt-in", f64(0.7), keys.Key{CacheNonzeroTemp: true}, true},
		{"unset with the key's opt-in", nil, keys.Key{CacheNonzeroTemp: true}, true},
	}
	for _, tc := range tests {
		if got := Cacheable(&provider.ChatRequest{Temperature: tc.temp}, tc.key); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// --- exact cache through the gateway ---

func TestExactCacheMissThenHitRecordsSavings(t *testing.T) {
	gw, p, rec, ex := newCacheGW(t)
	req := func() *provider.ChatRequest { return tempReq("m", f64(0), "what is 2+2") }

	first, err := chatKey(gw, testKey, req())
	if err != nil {
		t.Fatal(err)
	}
	gw.WaitForFills()
	if first.Cache != usage.CacheMiss || ex.sets != 1 {
		t.Fatalf("first: cache=%s sets=%d", first.Cache, ex.sets)
	}

	second, err := chatKey(gw, testKey, req())
	if err != nil {
		t.Fatal(err)
	}
	if second.Cache != usage.CacheHitExact || len(p.Requests()) != 1 {
		t.Fatalf("second: cache=%s provider calls=%d", second.Cache, len(p.Requests()))
	}
	// 1000 in * $3/M + 100 out * $15/M = $0.0045
	if first.Cost != 4500 || second.Cost != 0 || second.Saved != 4500 {
		t.Errorf("first cost %d; hit cost %d saved %d", first.Cost, second.Cost, second.Saved)
	}
	if second.Response.Choices[0].Message.Content.PlainText() != "four" || second.Response.ID == first.Response.ID {
		t.Errorf("hit body: %+v", second.Response)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	hit := rec.rows[1]
	if hit.CacheStatus != usage.CacheHitExact || hit.Cost != 0 || hit.Saved != 4500 || hit.InputTokens != 0 ||
		hit.Model != "m" || hit.Provider != "fake" || len(hit.Attempts) != 0 || hit.Outcome != usage.OutcomeOK {
		t.Errorf("hit row: %+v", hit)
	}
	if rec.rows[0].CacheStatus != usage.CacheMiss {
		t.Errorf("miss row: %+v", rec.rows[0])
	}
}

func TestWhatIsNotCached(t *testing.T) {
	gw, p, _, ex := newCacheGW(t)
	for _, req := range []*provider.ChatRequest{tempReq("m", nil, "q"), tempReq("m", f64(0.8), "q")} {
		for i := 0; i < 2; i++ {
			res, err := chatKey(gw, testKey, req)
			if err != nil {
				t.Fatal(err)
			}
			if res.Cache != usage.CacheBypass {
				t.Errorf("cache = %s, want bypass", res.Cache)
			}
		}
	}
	gw.WaitForFills()
	if len(p.Requests()) != 4 || ex.sets != 0 || ex.gets != 0 {
		t.Errorf("provider calls %d (want 4), cache gets %d sets %d (want 0, 0)", len(p.Requests()), ex.gets, ex.sets)
	}

	opted := keys.Key{ID: uuid.New(), Prefix: "spw_opt1", CacheNonzeroTemp: true}
	chatKey(gw, opted, tempReq("m", f64(0.8), "q"))
	gw.WaitForFills()
	if res, _ := chatKey(gw, opted, tempReq("m", f64(0.8), "q")); res.Cache != usage.CacheHitExact {
		t.Errorf("a key that opts in caches any temperature, got %s", res.Cache)
	}
}

func TestCacheIsPerKeyUnlessGlobal(t *testing.T) {
	a := keys.Key{ID: uuid.New(), Prefix: "spw_aaaa"}
	b := keys.Key{ID: uuid.New(), Prefix: "spw_bbbb"}
	req := func() *provider.ChatRequest { return tempReq("m", f64(0), "shared question") }

	gw, _, _, _ := newCacheGW(t)
	chatKey(gw, a, req())
	gw.WaitForFills()
	if res, _ := chatKey(gw, b, req()); res.Cache != usage.CacheMiss {
		t.Errorf("another key must not see a's answer, got %s", res.Cache)
	}

	gw, _, _, _ = newCacheGW(t)
	gw.cat.Cache.Global = true
	chatKey(gw, a, req())
	gw.WaitForFills()
	if res, _ := chatKey(gw, b, req()); res.Cache != usage.CacheHitExact {
		t.Errorf("cache.scope global shares entries, got %s", res.Cache)
	}
}

func TestStreamFillsAndReplays(t *testing.T) {
	gw, p, rec, _ := newCacheGW(t)
	p.Default = fake.Behavior{Text: "one two three", InputTokens: 50, OutputTokens: 7}
	mk := func() *provider.ChatRequest {
		r := tempReq("m", f64(0), "count")
		r.Stream = true
		r.StreamOptions = &provider.StreamOptions{IncludeUsage: true}
		return r
	}

	st, err := gw.ChatStream(context.Background(), testKey, uuid.New(), "", mk())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := drainStream(t, st); err != nil {
		t.Fatal(err)
	}
	st.Finish(context.Background(), nil)
	gw.WaitForFills()

	// A second stream is replayed from the cache: same text, the usage chunk, and nothing sent upstream.
	st2, err := gw.ChatStream(context.Background(), testKey, uuid.New(), "", mk())
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := drainStream(t, st2)
	if err != nil {
		t.Fatal(err)
	}
	st2.Finish(context.Background(), nil)
	var text string
	var usageSeen bool
	for _, c := range chunks {
		for _, ch := range c.Choices {
			if ch.Delta.Content != nil {
				text += *ch.Delta.Content
			}
		}
		if c.Usage != nil {
			usageSeen = true
		}
	}
	if text != "one two three" || !usageSeen || st2.Cache() != usage.CacheHitExact || st2.Attempts() != 0 {
		t.Errorf("replay: text=%q usage=%v cache=%s attempts=%d", text, usageSeen, st2.Cache(), st2.Attempts())
	}
	if len(p.Requests()) != 1 {
		t.Errorf("provider called %d times, want 1", len(p.Requests()))
	}

	// A non-streaming request is served from the entry the stream created.
	if res, _ := chatKey(gw, testKey, tempReq("m", f64(0), "count")); res.Cache != usage.CacheHitExact {
		t.Errorf("stream and non-stream share entries, got %s", res.Cache)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if r := rec.rows[1]; r.CacheStatus != usage.CacheHitExact || r.Cost != 0 || r.Saved == 0 {
		t.Errorf("replay row: %+v", r)
	}
}

func TestBrokenStreamIsNeverCached(t *testing.T) {
	gw, p, _, ex := newCacheGW(t)
	p.Default = fake.Behavior{Text: "one two three four", BreakAfter: 2}
	r := tempReq("m", f64(0), "count")
	r.Stream = true
	st, err := gw.ChatStream(context.Background(), testKey, uuid.New(), "", r)
	if err != nil {
		t.Fatal(err)
	}
	_, derr := drainStream(t, st)
	st.Finish(context.Background(), derr)
	gw.WaitForFills()
	if ex.sets != 0 {
		t.Errorf("a partial answer was cached (%d sets)", ex.sets)
	}
}

func TestFailuresAreNotCached(t *testing.T) {
	gw, p, _, ex := newCacheGW(t)
	p.Default = fake.Behavior{Status: 500}
	chatKey(gw, testKey, tempReq("m", f64(0), "q"))
	gw.WaitForFills()
	if ex.sets != 0 {
		t.Error("an error must not be cached")
	}
}

// --- Redis down ---

func TestRedisDownFailsOpen(t *testing.T) {
	gw, p, _, ex := newCacheGW(t)
	ex.failing = true
	obs := &recObserver{}
	lim := &stubLimiter{err: errors.New("redis is down")}
	gw.Use(Options{Exact: ex, Limiter: lim, Observer: obs})
	rpm := 10
	k := keys.Key{ID: uuid.New(), Prefix: "spw_down", RateLimitRPM: &rpm}

	for i := 0; i < 2; i++ {
		res, err := chatKey(gw, k, tempReq("m", f64(0), "q"))
		if err != nil {
			t.Fatalf("a Redis outage must not fail requests: %v", err)
		}
		if res.Response.Choices[0].Message.Content.PlainText() != "four" {
			t.Errorf("response: %+v", res.Response)
		}
	}
	gw.WaitForFills()
	if len(p.Requests()) != 2 {
		t.Errorf("without a cache every request goes upstream, got %d", len(p.Requests()))
	}
	redisErrs := 0
	for _, d := range obs.deps {
		if d == "redis" {
			redisErrs++
		}
	}
	if redisErrs < 4 { // limiter + cache get on both requests, plus fills
		t.Errorf("dependency errors should be counted, got %v", obs.deps)
	}
}

func TestNoRedisAtAllMeansNoLimitNoCache(t *testing.T) {
	gw, p, _ := newGW(t) // no Options: this is the gateway as it runs without REDIS_URL
	rpm := 1
	k := keys.Key{ID: uuid.New(), Prefix: "spw_none", RateLimitRPM: &rpm}
	for i := 0; i < 3; i++ {
		res, err := chatKey(gw, k, tempReq("m", f64(0), "q"))
		if err != nil || res.Cache != usage.CacheBypass {
			t.Fatalf("%v %+v", err, res)
		}
	}
	if len(p.Requests()) != 3 {
		t.Errorf("provider calls = %d", len(p.Requests()))
	}
}

// --- rate limit ---

func TestRateLimitedRequest(t *testing.T) {
	gw, p, rec := newGW(t)
	lim := &stubLimiter{ok: false, retry: 1500 * time.Millisecond}
	gw.Use(Options{Limiter: lim})
	rpm := 30
	k := keys.Key{ID: uuid.New(), Prefix: "spw_rate", RateLimitRPM: &rpm}

	_, err := chatKey(gw, k, userReq("m"))
	var ge *Error
	if !errors.As(err, &ge) || ge.Status != 429 || ge.Code != "rate_limit_exceeded" {
		t.Fatalf("err = %v", err)
	}
	if ge.RetryAfter != 2*time.Second {
		t.Errorf("Retry-After rounds up to whole seconds, got %v", ge.RetryAfter)
	}
	if len(p.Requests()) != 0 {
		t.Error("a rate-limited request must not reach a provider")
	}
	if row := rec.only(t); row.Outcome != usage.OutcomeRateLimited || row.Cost != 0 {
		t.Errorf("row: %+v", row)
	}

	// A key without a limit never consults the limiter.
	lim.calls = 0
	if _, err := chatKey(gw, testKey, userReq("m")); err != nil || lim.calls != 0 {
		t.Errorf("unlimited key: err=%v limiter calls=%d", err, lim.calls)
	}
}

func TestRateLimitedStreamingRequest(t *testing.T) {
	gw, _, _ := newGW(t)
	gw.Use(Options{Limiter: &stubLimiter{ok: false, retry: time.Second}})
	rpm := 1
	r := userReq("m")
	r.Stream = true
	_, err := gw.ChatStream(context.Background(), keys.Key{ID: uuid.New(), RateLimitRPM: &rpm}, uuid.New(), "", r)
	var ge *Error
	if !errors.As(err, &ge) || ge.Status != 429 {
		t.Fatalf("err = %v", err)
	}
}

// --- budgets ---

func budgetKey(micros money.Micros) keys.Key {
	return keys.Key{ID: uuid.New(), Prefix: "spw_budg", MonthlyBudget: &micros}
}

func TestBudgetBoundary(t *testing.T) {
	const budget = money.Micros(1_000_000) // $1.00
	tests := []struct {
		name    string
		spend   money.Micros
		blocked bool
	}{
		{"nothing spent", 0, false},
		{"one micro-dollar under", budget - 1, false},
		{"exactly at the budget", budget, true},
		{"one micro-dollar over", budget + 1, true},
		{"far over", budget * 10, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gw, p, rec := newGW(t)
			gw.Use(Options{Spend: &stubSpend{spend: tc.spend}})
			_, err := chatKey(gw, budgetKey(budget), userReq("m"))
			var ge *Error
			if tc.blocked {
				if !errors.As(err, &ge) || ge.Status != 402 || ge.Code != "budget_exceeded" {
					t.Fatalf("err = %v", err)
				}
				if len(p.Requests()) != 0 {
					t.Error("an over-budget request must not reach a provider")
				}
				if row := rec.only(t); row.Outcome != usage.OutcomeOverBudget {
					t.Errorf("outcome = %s", row.Outcome)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestBudgetEdgeCases(t *testing.T) {
	gw, _, _ := newGW(t)
	sp := &stubSpend{spend: 5_000_000}
	gw.Use(Options{Spend: sp})

	if _, err := chatKey(gw, testKey, userReq("m")); err != nil || sp.calls != 0 {
		t.Errorf("a key with no budget is never checked: err=%v reads=%d", err, sp.calls)
	}
	if _, err := chatKey(gw, budgetKey(0), userReq("m")); err == nil {
		t.Error("a budget of $0 blocks everything")
	}

	sp.err = errors.New("db down")
	obs := &recObserver{}
	gw.Use(Options{Spend: sp, Observer: obs})
	if _, err := chatKey(gw, budgetKey(1), userReq("m")); err != nil {
		t.Errorf("an unreadable budget fails open, got %v", err)
	}
	if len(obs.deps) != 1 || obs.deps[0] != "postgres" {
		t.Errorf("dependency errors = %v", obs.deps)
	}
}

func TestBudgetSpendIsCachedAndCharged(t *testing.T) {
	gw, p, _ := newGW(t)
	p.Default = fake.Behavior{InputTokens: 1000, OutputTokens: 100} // costs 4500 micro-dollars
	sp := &stubSpend{spend: 0}
	gw.Use(Options{Spend: sp})
	k := budgetKey(10_000)

	// Each request costs 4500. The check happens before the call, so a request is let through whenever the
	// spend so far is under the budget: 0, 4500 and 9000 all pass, and 13500 does not. That overshoot by one
	// request is the documented "approximate" part of budgets.
	for i := 1; i <= 3; i++ {
		if _, err := chatKey(gw, k, userReq("m")); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	var ge *Error
	if _, err := chatKey(gw, k, userReq("m")); !errors.As(err, &ge) || ge.Status != 402 {
		t.Fatalf("request 4 should be over budget, got %v", err)
	}
	// Spend was charged locally after each call, so the database was read once, not four times.
	if sp.calls != 1 {
		t.Errorf("spend read %d times within the cache window, want 1", sp.calls)
	}
}

func TestBudgetMonthRollover(t *testing.T) {
	sp := &stubSpend{spend: 5_000_000}
	b := NewBudgets(sp, time.Hour)
	now := time.Date(2026, 1, 31, 23, 59, 0, 0, time.UTC)
	b.now = func() time.Time { return now }
	id := uuid.New()
	if ex, _, _ := b.Exceeded(context.Background(), id, 1_000_000); !ex {
		t.Fatal("over budget in January")
	}
	sp.spend = 0
	now = time.Date(2026, 2, 1, 0, 0, 1, 0, time.UTC) // a new month, well within the cache TTL
	if ex, _, _ := b.Exceeded(context.Background(), id, 1_000_000); ex {
		t.Error("budget must reset on the first of the month (UTC), even if the cache is fresh")
	}
}

// --- semantic cache ---

func vec(angleDeg float64) []float32 { // unit vectors in a plane: cosine similarity is cos(angle between)
	r := angleDeg * math.Pi / 180
	return []float32{float32(math.Cos(r)), float32(math.Sin(r)), 0}
}

func newSemanticGW(t *testing.T) (*Gateway, *fake.Provider, *tableEmbedder, *recObserver) {
	t.Helper()
	gw, p, _ := newGW(t)
	p.Default = fake.Behavior{Text: "four", InputTokens: 1000, OutputTokens: 100}
	emb := &tableEmbedder{vecs: map[string][]float32{
		"what is 2+2":        vec(0),
		"whats two plus two": vec(10), // cos 0.985: a paraphrase
		"what is 2 + 2?":     vec(18), // cos 0.951: just inside 0.95
		"two and two":        vec(19), // cos 0.945: just outside
		"capital of France":  vec(90), // unrelated
	}}
	obs := &recObserver{}
	gw.Use(Options{Semantic: &memSemantic{}, Embedder: emb, EmbedModel: "e", Observer: obs})
	return gw, p, emb, obs
}

func semKey() keys.Key { return keys.Key{ID: uuid.New(), Prefix: "spw_semc", SemanticCache: true} }

func TestSemanticThresholdEdges(t *testing.T) {
	gw, p, _, _ := newSemanticGW(t)
	k := semKey()
	chatKey(gw, k, tempReq("m", f64(0), "what is 2+2"))
	gw.WaitForFills()

	tests := []struct {
		text string
		want usage.CacheStatus
	}{
		{"what is 2+2", usage.CacheHitSemantic}, // identical (similarity 1)
		{"whats two plus two", usage.CacheHitSemantic},
		{"what is 2 + 2?", usage.CacheHitSemantic}, // 0.951 >= 0.95
		{"two and two", usage.CacheMiss},           // 0.945 < 0.95
		{"capital of France", usage.CacheMiss},
	}
	for _, tc := range tests {
		res, err := chatKey(gw, k, tempReq("m", f64(0), tc.text))
		if err != nil || res.Cache != tc.want {
			t.Errorf("%q: cache=%v err=%v, want %s", tc.text, res.Cache, err, tc.want)
		}
	}
	// The first request plus the two misses went upstream.
	if n := len(p.Requests()); n != 3 {
		t.Errorf("provider calls = %d, want 3", n)
	}
}

func TestIsHitRule(t *testing.T) {
	tests := []struct {
		sim, thr float64
		want     bool
	}{
		{0.95, 0.95, true}, {0.9500001, 0.95, true}, {0.9499999, 0.95, false}, {1, 0.95, true}, {0, 0.95, false}, {-1, 0.95, false},
	}
	for _, tc := range tests {
		if got := IsHit(tc.sim, tc.thr); got != tc.want {
			t.Errorf("IsHit(%v, %v) = %v", tc.sim, tc.thr, got)
		}
	}
}

func TestSemanticHitRecordsSavings(t *testing.T) {
	gw, _, _, _ := newSemanticGW(t)
	k := semKey()
	chatKey(gw, k, tempReq("m", f64(0), "what is 2+2"))
	gw.WaitForFills()
	res, _ := chatKey(gw, k, tempReq("m", f64(0), "whats two plus two"))
	if res.Cache != usage.CacheHitSemantic || res.Cost != 0 || res.Saved != 4500 {
		t.Errorf("%+v", res)
	}
}

func TestSemanticOnlyMatchesSameContext(t *testing.T) {
	gw, _, _, _ := newSemanticGW(t)
	k := semKey()
	withSystem := func(sys, q string) *provider.ChatRequest {
		r := tempReq("m", f64(0), q)
		r.Messages = append([]provider.Message{{Role: "system", Content: provider.TextContent(sys)}}, r.Messages...)
		return r
	}
	chatKey(gw, k, withSystem("answer in French", "what is 2+2"))
	gw.WaitForFills()
	if res, _ := chatKey(gw, k, withSystem("answer in French", "whats two plus two")); res.Cache != usage.CacheHitSemantic {
		t.Errorf("same system prompt should hit, got %s", res.Cache)
	}
	if res, _ := chatKey(gw, k, withSystem("answer in German", "whats two plus two")); res.Cache != usage.CacheMiss {
		t.Errorf("a different system prompt must not match, got %s", res.Cache)
	}
	if res, _ := chatKey(gw, k, tempReq("other", f64(0), "whats two plus two")); res != nil && res.Cache == usage.CacheHitSemantic {
		t.Error("a different policy must not match")
	}
}

func TestSemanticIsOptInAndPerKey(t *testing.T) {
	gw, p, emb, _ := newSemanticGW(t)
	off := keys.Key{ID: uuid.New(), Prefix: "spw_off0"} // SemanticCache false
	chatKey(gw, off, tempReq("m", f64(0), "what is 2+2"))
	gw.WaitForFills()
	if emb.calls != 0 {
		t.Error("a key without semantic_cache must never be embedded")
	}

	a, b := semKey(), semKey()
	chatKey(gw, a, tempReq("m", f64(0), "what is 2+2"))
	gw.WaitForFills()
	if res, _ := chatKey(gw, b, tempReq("m", f64(0), "whats two plus two")); res.Cache != usage.CacheMiss {
		t.Errorf("semantic entries are per key, got %s", res.Cache)
	}
	_ = p
}

func TestSemanticSkipsToolConversations(t *testing.T) {
	gw, _, emb, _ := newSemanticGW(t)
	r := tempReq("m", f64(0), "what is 2+2")
	r.Messages = append(r.Messages,
		provider.Message{Role: "assistant", Content: provider.Content{Null: true}, ToolCalls: []provider.ToolCall{{ID: "c", Function: provider.FunctionCall{Name: "f", Arguments: "{}"}}}},
		provider.Message{Role: "tool", ToolCallID: "c", Content: provider.TextContent("r")},
		provider.Message{Role: "user", Content: provider.TextContent("what is 2+2")})
	res, err := chatKey(gw, semKey(), r)
	if err != nil || res.Cache != usage.CacheBypass || emb.calls != 0 {
		t.Errorf("cache=%v embed calls=%d err=%v; tool history is not eligible", res.Cache, emb.calls, err)
	}
}

func TestSemanticEmbedderDownFailsOpen(t *testing.T) {
	gw, _, emb, obs := newSemanticGW(t)
	emb.fail = true
	res, err := chatKey(gw, semKey(), tempReq("m", f64(0), "what is 2+2"))
	if err != nil || res.Response.Choices[0].Message.Content.PlainText() != "four" {
		t.Fatalf("an embedder outage must not fail requests: %v", err)
	}
	if len(obs.deps) != 1 || obs.deps[0] != "embedder" {
		t.Errorf("dependency errors = %v", obs.deps)
	}
}

func TestSemanticTextAndGroup(t *testing.T) {
	r := tempReq("m", f64(0), "  hello  ")
	if text, ok := SemanticText(r); !ok || text != "hello" {
		t.Errorf("text=%q ok=%v", text, ok)
	}
	r.Messages = append(r.Messages, provider.Message{Role: "assistant", Content: provider.TextContent("hi")})
	if _, ok := SemanticText(r); ok {
		t.Error("the last message must be from the user")
	}
	a, b := tempReq("m", f64(0), "x"), tempReq("m", f64(0), "y")
	if SemanticGroup(a) != SemanticGroup(b) {
		t.Error("the question itself is not part of the group")
	}
	b.Tools = []provider.Tool{{Type: "function", Function: provider.ToolFunction{Name: "f"}}}
	if SemanticGroup(a) == SemanticGroup(b) {
		t.Error("the tool list is part of the group")
	}
}
