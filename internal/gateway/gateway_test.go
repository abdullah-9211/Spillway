package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/money"
	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/provider/fake"
	_ "github.com/abdullah-9211/spillway/internal/provider/openai"
	"github.com/abdullah-9211/spillway/internal/usage"
)

type memRecorder struct {
	mu   sync.Mutex
	rows []usage.Row
}

func (m *memRecorder) Record(r usage.Row) { m.mu.Lock(); m.rows = append(m.rows, r); m.mu.Unlock() }
func (m *memRecorder) only(t *testing.T) usage.Row {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.rows) != 1 {
		t.Fatalf("want exactly one usage row, got %d", len(m.rows))
	}
	return m.rows[0]
}

const testCatalog = `
providers:
  fake: { type: openai, base_url: "http://unused" }
  down: { type: openai, api_key_env: DOWN_KEY }
models:
  - { id: m, provider: fake, upstream: up-model, input_usd_per_mtok: 3, output_usd_per_mtok: 15 }
  - { id: lost, provider: down, upstream: x }
policies:
  - { name: fixedm, type: fixed, model: m }
gateway: { max_retries: 0 }
`

func newGW(t *testing.T) (*Gateway, *fake.Provider, *memRecorder) {
	t.Helper()
	cat, err := ParseCatalog([]byte(testCatalog))
	if err != nil {
		t.Fatal(err)
	}
	p := fake.New("fake")
	rec := &memRecorder{}
	gw := New(cat, map[string]provider.Provider{"fake": p}, map[string]string{"down": "DOWN_KEY is not set"}, rec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return gw, p, rec
}

func userReq(model string) *provider.ChatRequest {
	return &provider.ChatRequest{Model: model, Messages: []provider.Message{{Role: "user", Content: provider.TextContent("hi there")}}}
}

var testKey = keys.Key{ID: uuid.New(), Name: "t"}

func TestChatRecordsUsageAndCost(t *testing.T) {
	gw, p, rec := newGW(t)
	p.Default = fake.Behavior{Text: "hello", InputTokens: 1000, OutputTokens: 500}
	id := uuid.New()

	res, err := gw.Chat(context.Background(), testKey, id, "", userReq("m"))
	if err != nil {
		t.Fatal(err)
	}
	// 1000 * $3/M + 500 * $15/M = $0.003 + $0.0075 = $0.0105
	if res.Cost != 10_500 || res.Cost.String() != "0.010500" {
		t.Errorf("cost = %s", res.Cost)
	}
	if res.Response.Model != "m" || res.Response.Object != "chat.completion" {
		t.Errorf("response must carry the catalog id: %+v", res.Response)
	}
	if got := p.Requests()[0].Model; got != "up-model" {
		t.Errorf("upstream model = %q, want the catalog's upstream id", got)
	}
	row := rec.only(t)
	if row.ID != id || row.KeyID != testKey.ID || row.Policy != "m" || row.Provider != "fake" || row.Model != "m" ||
		row.InputTokens != 1000 || row.OutputTokens != 500 || row.Cost != 10_500 ||
		row.Outcome != usage.OutcomeOK || row.CacheStatus != usage.CacheBypass || row.TTFBMs == nil {
		t.Errorf("usage row: %+v", row)
	}
	if len(row.Attempts) != 1 || row.Attempts[0].Kind != "primary" || row.Attempts[0].Estimated {
		t.Errorf("attempts: %+v", row.Attempts)
	}
}

func TestPolicyNameIsRecordedAsAsked(t *testing.T) {
	gw, _, rec := newGW(t)
	if _, err := gw.Chat(context.Background(), testKey, uuid.New(), "", userReq("fixedm")); err != nil {
		t.Fatal(err)
	}
	if row := rec.only(t); row.Policy != "fixedm" || row.Model != "m" {
		t.Errorf("policy=%q model=%q", row.Policy, row.Model)
	}
}

func TestChatErrors(t *testing.T) {
	tests := []struct {
		name       string
		behavior   fake.Behavior
		model      string
		wantStatus int
		wantCode   string
		wantRow    bool
		wantOut    usage.Outcome
	}{
		{"upstream 500", fake.Behavior{Status: 500}, "m", 502, "upstream_error", true, usage.OutcomeUpstreamError},
		{"upstream 429", fake.Behavior{Status: 429}, "m", 502, "upstream_error", true, usage.OutcomeUpstreamError},
		{"upstream auth", fake.Behavior{Status: 401}, "m", 502, "upstream_error", true, usage.OutcomeUpstreamError},
		{"provider rejects request", fake.Behavior{Status: 400}, "m", 400, "invalid_request", true, usage.OutcomeUpstreamError},
		{"unknown model", fake.Behavior{}, "nope", 400, "invalid_request", false, ""},
		{"provider not configured", fake.Behavior{}, "lost", 502, "upstream_error", true, usage.OutcomeUpstreamError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gw, p, rec := newGW(t)
			p.Default = tc.behavior
			_, err := gw.Chat(context.Background(), testKey, uuid.New(), "", userReq(tc.model))
			var ge *Error
			if !errors.As(err, &ge) || ge.Status != tc.wantStatus || ge.Code != tc.wantCode {
				t.Fatalf("err = %v, want %d %s", err, tc.wantStatus, tc.wantCode)
			}
			if tc.wantRow {
				row := rec.only(t)
				if row.Outcome != tc.wantOut || row.Cost != 0 || row.Attempts[0].Error == "" {
					t.Errorf("row: %+v", row)
				}
			} else if len(rec.rows) != 0 {
				t.Errorf("a request that never reached a provider must not record usage: %+v", rec.rows)
			}
		})
	}
}

func TestChatCancelledByClient(t *testing.T) {
	gw, p, rec := newGW(t)
	p.Default = fake.Behavior{Delay: 5e9}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := gw.Chat(ctx, testKey, uuid.New(), "", userReq("m")); err == nil {
		t.Fatal("expected an error")
	}
	if row := rec.only(t); row.Outcome != usage.OutcomeClientCancelled {
		t.Errorf("outcome = %s", row.Outcome)
	}
}

func TestValidateRequest(t *testing.T) {
	two, hot := 2, 3.0
	msgs := []provider.Message{{Role: "user", Content: provider.TextContent("x")}}
	tests := []struct {
		name  string
		req   provider.ChatRequest
		param string
	}{
		{"no model", provider.ChatRequest{Messages: msgs}, "model"},
		{"no messages", provider.ChatRequest{Model: "m"}, "messages"},
		{"bad role", provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "wizard"}}}, "messages[0].role"},
		{"tool without id", provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "tool"}}}, "messages[0].tool_call_id"},
		{"n=2", provider.ChatRequest{Model: "m", Messages: msgs, N: &two}, "n"},
		{"temperature", provider.ChatRequest{Model: "m", Messages: msgs, Temperature: &hot}, "temperature"},
		{"bad tool", provider.ChatRequest{Model: "m", Messages: msgs, Tools: []provider.Tool{{Type: "retrieval"}}}, "tools[0]"},
	}
	for _, tc := range tests {
		err := ValidateRequest(&tc.req)
		var ge *Error
		if !errors.As(err, &ge) || ge.Status != 400 || ge.Param != tc.param {
			t.Errorf("%s: got %v, want 400 with param %q", tc.name, err, tc.param)
		}
	}
	if err := ValidateRequest(&provider.ChatRequest{Model: "m", Messages: msgs}); err != nil {
		t.Errorf("valid request rejected: %v", err)
	}
}

func drainStream(t *testing.T, s *Stream) (chunks []*provider.ChatChunk, err error) {
	t.Helper()
	for {
		c, err := s.Next()
		if errors.Is(err, io.EOF) {
			return chunks, nil
		}
		if err != nil {
			return chunks, err
		}
		chunks = append(chunks, c)
	}
}

func TestStreamUsageChunkOnlyWhenAsked(t *testing.T) {
	for _, want := range []bool{false, true} {
		gw, p, rec := newGW(t)
		p.Default = fake.Behavior{Text: "a b", InputTokens: 7, OutputTokens: 3}
		req := userReq("m")
		req.Stream = true
		req.StreamOptions = &provider.StreamOptions{IncludeUsage: want}

		st, err := gw.ChatStream(context.Background(), testKey, uuid.New(), "", req)
		if err != nil {
			t.Fatal(err)
		}
		chunks, err := drainStream(t, st)
		if err != nil {
			t.Fatal(err)
		}
		st.Finish(context.Background(), nil)

		var withUsage, empty int
		for _, c := range chunks {
			if c.Usage != nil {
				withUsage++
			}
			if len(c.Choices) == 0 {
				empty++
			}
			if c.Model != "m" || c.Object != "chat.completion.chunk" {
				t.Errorf("chunk not normalised: %+v", c)
			}
		}
		if want && (withUsage != 1 || empty != 1) || !want && (withUsage != 0 || empty != 0) {
			t.Errorf("include_usage=%v: usage chunks=%d empty-choice chunks=%d", want, withUsage, empty)
		}
		// Usage is recorded from the upstream either way.
		row := rec.only(t)
		if row.InputTokens != 7 || row.OutputTokens != 3 || row.Outcome != usage.OutcomeOK || row.Attempts[0].Estimated {
			t.Errorf("include_usage=%v: row %+v", want, row)
		}
		if row.Cost != money.TokenCost(7, 3_000_000, 3, 15_000_000) {
			t.Errorf("cost = %d", row.Cost)
		}
	}
}

func TestStreamMidFailureAndEstimate(t *testing.T) {
	gw, p, rec := newGW(t)
	p.Default = fake.Behavior{Text: "one two three four", BreakAfter: 2}
	req := userReq("m")
	req.Stream = true
	st, err := gw.ChatStream(context.Background(), testKey, uuid.New(), "", req)
	if err != nil {
		t.Fatal(err)
	}
	_, derr := drainStream(t, st)
	if derr == nil {
		t.Fatal("stream should have failed")
	}
	st.Finish(context.Background(), derr)
	st.Finish(context.Background(), derr) // a second call must not double-record

	row := rec.only(t)
	if row.Outcome != usage.OutcomeUpstreamError || !row.Attempts[0].Estimated || row.OutputTokens == 0 {
		t.Errorf("a broken stream records upstream_error with estimated tokens so far: %+v", row)
	}
}

func TestStreamClientCancel(t *testing.T) {
	gw, p, rec := newGW(t)
	p.Default = fake.Behavior{Text: "hello", Hang: true}
	req := userReq("m")
	req.Stream = true
	ctx, cancel := context.WithCancel(context.Background())
	st, err := gw.ChatStream(ctx, testKey, uuid.New(), "", req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Next(); err != nil {
		t.Fatal(err)
	}
	cancel()
	st.Finish(ctx, ctx.Err())
	if row := rec.only(t); row.Outcome != usage.OutcomeClientCancelled {
		t.Errorf("outcome = %s", row.Outcome)
	}
	if p.Canceled() != 1 {
		t.Errorf("upstream cancelled %d times, want 1", p.Canceled())
	}
}

func TestStreamFailureBeforeFirstChunk(t *testing.T) {
	gw, p, rec := newGW(t)
	p.Default = fake.Behavior{Status: 503}
	req := userReq("m")
	req.Stream = true
	_, err := gw.ChatStream(context.Background(), testKey, uuid.New(), "", req)
	var ge *Error
	if !errors.As(err, &ge) || ge.Status != 502 {
		t.Fatalf("err = %v", err)
	}
	if row := rec.only(t); row.Outcome != usage.OutcomeUpstreamError {
		t.Errorf("outcome = %s", row.Outcome)
	}
}

func TestBuildProviders(t *testing.T) {
	cat, _ := ParseCatalog([]byte(`
providers:
  openai:  { api_key_env: OPENAI_API_KEY }
  keyless: { type: openai, api_key_env: KL_KEY, base_url: "http://localhost:1/v1" }
  unused:  { api_key_env: NOPE }
models:
  - { id: a, provider: openai,  upstream: u }
  - { id: b, provider: keyless, upstream: u }
`))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	provs, missing, err := BuildProviders(cat, func(string) string { return "" }, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := provs["openai"]; ok || missing["openai"] == "" {
		t.Errorf("openai without a key must be reported missing: %v %v", provs, missing)
	}
	if _, ok := provs["keyless"]; !ok {
		t.Error("a provider with a custom base_url starts without a key (a local or fake server)")
	}
	if _, ok := provs["unused"]; ok || missing["unused"] != "" {
		t.Error("providers no model uses are ignored")
	}

	provs, _, _ = BuildProviders(cat, func(k string) string {
		if k == "OPENAI_API_KEY" {
			return "sk-x"
		}
		return ""
	}, log)
	if provs["openai"] == nil {
		t.Error("openai should be built when its key is set")
	}

	bad, _ := ParseCatalog([]byte("providers: { google: {} }\nmodels: [{id: g, provider: google, upstream: u}]\n"))
	if _, _, err := BuildProviders(bad, func(string) string { return "" }, log); err == nil {
		t.Error("a provider with no adapter is a startup error")
	}
}
