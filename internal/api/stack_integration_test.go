//go:build integration

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/db"
	"github.com/abdullah-9211/spillway/internal/db/sqlcgen"
	"github.com/abdullah-9211/spillway/internal/gateway"
	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/provider/fake"
	"github.com/abdullah-9211/spillway/internal/usage"
)

// stack is the real thing end to end except the provider: real Postgres, real key store, real usage writer.
type stack struct {
	gw     *gateway.Gateway
	srv    *httptest.Server
	prov   *fake.Provider
	prov2  *fake.Provider
	writer *usage.Writer
	keys   *keys.Store
	q      *sqlcgen.Queries
	pool   *pgxpool.Pool
}

func newStack(t *testing.T) *stack { return newStackWith(t, nil) }

// newStackWith lets a test switch on the Redis, pgvector and metrics features.
func newStackWith(t *testing.T, build func(pg *db.Postgres) gateway.Options) *stack {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Fatal("DATABASE_URL is not set; run `make up` and use `make test`")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatal(err)
	}
	pg, err := db.OpenPostgres(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pg.Close)

	cat, err := gateway.ParseCatalog([]byte(apiCatalog))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, p2 := fake.New("fake"), fake.New("fake2")
	writer := usage.NewWriter(pg.Pool, log, usage.WriterOptions{Interval: 20 * time.Millisecond})
	store := keys.NewStore(pg.Pool)
	gw := gateway.New(cat, map[string]provider.Provider{"fake": p, "fake2": p2}, nil, writer, log)
	var metrics http.Handler
	if build != nil {
		opts := build(pg)
		gw.Use(opts)
		if m, ok := opts.Observer.(interface{ Handler() http.Handler }); ok {
			metrics = m.Handler()
		}
	}
	srv := httptest.NewServer(NewHandler(Options{Gateway: gw, Auth: store, Log: log, Deps: []Dependency{{pg, true}}, Metrics: metrics}))
	t.Cleanup(func() { srv.Close(); gw.WaitForFills(); _ = writer.Close(context.Background()) })
	return &stack{pool: pg.Pool, gw: gw, srv: srv, prov: p, prov2: p2, writer: writer, keys: store, q: sqlcgen.New(pg.Pool)}
}

func (s *stack) newKey(t *testing.T, name string) (keys.Key, string) {
	t.Helper()
	k, full, err := s.keys.Create(context.Background(), keys.CreateParams{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return k, full
}

func (s *stack) do(t *testing.T, token, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, s.srv.URL+"/v1/chat/completions", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// usageRow waits for the buffered writer to flush the row for this request.
func (s *stack) usageRow(t *testing.T, requestID string) sqlcgen.Usage {
	t.Helper()
	id := uuid.MustParse(requestID)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if row, err := s.q.GetUsage(context.Background(), id); err == nil {
			return row
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no usage row for request %s", requestID)
	return sqlcgen.Usage{}
}

func TestAuthAgainstRealKeys(t *testing.T) {
	s := newStack(t)
	k, full := s.newKey(t, "auth-test")

	if resp := s.do(t, full, simple); resp.StatusCode != 200 {
		t.Fatalf("valid key: %d", resp.StatusCode)
	}
	if _, err := s.keys.Revoke(context.Background(), k.ID.String()); err != nil {
		t.Fatal(err)
	}
	for name, token := range map[string]string{
		"missing": "", "unknown": "spw_" + strings.Repeat("0", 48), "revoked": full, "wrong prefix": "sk-abc",
	} {
		if resp := s.do(t, token, simple); resp.StatusCode != 401 {
			t.Errorf("%s: status %d, want 401", name, resp.StatusCode)
		}
	}
	// Only the first call reached the provider.
	if n := len(s.prov.Requests()); n != 1 {
		t.Errorf("provider saw %d requests, want 1", n)
	}
}

func TestUsageRowForNonStreaming(t *testing.T) {
	s := newStack(t)
	k, full := s.newKey(t, "usage-nonstream")
	s.prov.Default = fake.Behavior{Text: "ok", InputTokens: 1000, OutputTokens: 100}

	resp := s.do(t, full, simple)
	row := s.usageRow(t, resp.Header.Get("X-Spillway-Request-Id"))

	cost, _ := db.MicrosFromNumeric(row.CostUsd)
	if row.ApiKeyID.UUID != k.ID || row.Model.String != "m" || row.Provider.String != "fake" ||
		row.InputTokens != 1000 || row.OutputTokens != 100 || cost != 4500 ||
		row.Outcome != "ok" || row.CacheStatus != "bypass" || row.Policy != "m" {
		t.Errorf("row: %+v cost=%d", row, cost)
	}
	if got := resp.Header.Get("X-Spillway-Cost-Usd"); got != cost.String() {
		t.Errorf("header cost %s != stored cost %s", got, cost)
	}

	day, err := s.q.GetUsageDaily(context.Background(), sqlcgen.GetUsageDailyParams{
		ApiKeyID: k.ID, Day: pgtype.Date{Time: time.Now().UTC().Truncate(24 * time.Hour), Valid: true}, Model: "m"})
	if err != nil || day.Requests != 1 || day.InputTokens != 1000 {
		t.Errorf("daily rollup: %+v %v", day, err)
	}
}

func TestUsageRowForStreaming(t *testing.T) {
	s := newStack(t)
	k, full := s.newKey(t, "usage-stream")
	s.prov.Default = fake.Behavior{Text: "a b c", InputTokens: 2000, OutputTokens: 200}

	resp := s.do(t, full, `{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	_, _ = io.Copy(io.Discard, resp.Body)
	row := s.usageRow(t, resp.Header.Get("X-Spillway-Request-Id"))

	cost, _ := db.MicrosFromNumeric(row.CostUsd)
	if row.ApiKeyID.UUID != k.ID || row.Policy != "default" || row.Model.String != "m" ||
		row.InputTokens != 2000 || row.OutputTokens != 200 || cost != 9000 || row.Outcome != "ok" || !row.TtfbMs.Valid {
		t.Errorf("row: %+v cost=%d", row, cost)
	}
}

func TestDisconnectedStreamIsRecordedAsCancelled(t *testing.T) {
	s := newStack(t)
	_, full := s.newKey(t, "usage-disconnect")
	s.prov.Default = fake.Behavior{Text: "hello", Hang: true}

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+full)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	id := resp.Header.Get("X-Spillway-Request-Id")
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close()

	row := s.usageRow(t, id)
	if row.Outcome != "client_cancelled" {
		t.Errorf("outcome = %s", row.Outcome)
	}
	deadline := time.Now().Add(3 * time.Second)
	for s.prov.Canceled() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.prov.Canceled() != 1 {
		t.Errorf("upstream cancelled %d times, want 1", s.prov.Canceled())
	}
}

func TestFailedUpstreamIsRecorded(t *testing.T) {
	s := newStack(t)
	_, full := s.newKey(t, "usage-fail")
	s.prov.Default = fake.Behavior{Status: 500}

	resp := s.do(t, full, simple)
	if resp.StatusCode != 502 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	row := s.usageRow(t, resp.Header.Get("X-Spillway-Request-Id"))
	cost, _ := db.MicrosFromNumeric(row.CostUsd)
	if row.Outcome != "upstream_error" || cost != 0 {
		t.Errorf("row: %+v", row)
	}
}

func TestAttemptsArePersistedAsJSON(t *testing.T) {
	s := newStack(t)
	_, full := s.newKey(t, "attempts")
	s.prov.Default = fake.Behavior{Status: 500}
	s.prov2.Default = fake.Behavior{Text: "ok"}

	resp := s.do(t, full, `{"model":"chain","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	row := s.usageRow(t, resp.Header.Get("X-Spillway-Request-Id"))
	var attempts []usage.Attempt
	if err := json.Unmarshal(row.Attempts, &attempts); err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 || attempts[0].Provider != "fake" || attempts[0].Status != 500 || attempts[0].ErrorKind != "server" ||
		attempts[1].Provider != "fake2" || attempts[1].Kind != "fallback" || attempts[1].Error != "" {
		t.Errorf("attempts = %+v", attempts)
	}
	if row.Policy != "chain" || row.Model.String != "m2" || row.Provider.String != "fake2" {
		t.Errorf("row: policy=%s provider=%s model=%s", row.Policy, row.Provider.String, row.Model.String)
	}
	if attempts[0].Injected {
		t.Error("injected is reserved for playground faults")
	}
}

// memCache is an in-process exact cache, so tests that want a cache hit do not need Redis.
type memCache struct {
	mu sync.Mutex
	m  map[string]*gateway.Entry
}

func newMemCache() *memCache { return &memCache{m: map[string]*gateway.Entry{}} }

func (c *memCache) Get(_ context.Context, k string) (*gateway.Entry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[k], nil
}

func (c *memCache) Set(_ context.Context, k string, e *gateway.Entry, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[k] = e
	return nil
}
