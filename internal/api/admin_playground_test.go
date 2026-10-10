package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/gateway"
	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/money"
	"github.com/abdullah-9211/spillway/internal/playground"
	"github.com/abdullah-9211/spillway/internal/provider/faults"
	"github.com/abdullah-9211/spillway/internal/usage"
)

type fakePlayground struct {
	last   playground.Input
	err    error
	faults bool
}

func (f *fakePlayground) Run(_ context.Context, in playground.Input) (*playground.Output, error) {
	f.last = in
	if f.err != nil {
		return nil, f.err
	}
	return &playground.Output{ID: uuid.New(), CreatedAt: time.Now(), Policy: in.Policy, Answer: "an answer", FinishReason: "stop", Provider: "openai", Model: "mini", Cache: "miss",
		InputTokens: 10, OutputTokens: 5, CostUSD: "0.000105", SavedUSD: "0.000000", LatencyMs: 120, OverheadMs: 3, Outcome: "ok",
		Attempts: []usage.Attempt{{Provider: "anthropic", Model: "sonnet", Kind: "primary", Status: 429, ErrorKind: "rate_limited", Error: "injected", Injected: true, LatencyMs: 1}, {Provider: "openai", Model: "mini", Kind: "fallback", LatencyMs: 110}},
		Faults:   in.Faults, UnusedFaults: []faults.Fault{}}, nil
}

func (f *fakePlayground) History(context.Context, int, string) ([]playground.HistoryItem, string, error) {
	if f.err != nil && errors.Is(f.err, playground.ErrBadCursor) {
		return nil, "", f.err
	}
	return []playground.HistoryItem{{Prompt: "p", System: "", Output: playground.Output{ID: uuid.New(), CreatedAt: time.Now(), Policy: "default", Answer: "a", Provider: "openai", Model: "mini", Cache: "miss",
		CostUSD: "0.000100", SavedUSD: "0.000000", Outcome: "ok", Attempts: []usage.Attempt{}, Faults: []faults.Fault{}, UnusedFaults: []faults.Fault{}}}}, "NEXT", nil
}

func (f *fakePlayground) FaultInjection() bool { return !f.faults }

func playgroundCatalog(t *testing.T) *gateway.Catalog {
	t.Helper()
	c, err := gateway.ParseCatalog([]byte(`
providers:
  anthropic: { api_key_env: A }
  openai: { api_key_env: O }
models:
  - { id: sonnet, provider: anthropic, upstream: s, input_usd_per_mtok: 3, output_usd_per_mtok: 15, tags: [reasoning] }
  - { id: mini, provider: openai, upstream: m, input_usd_per_mtok: 0.15, output_usd_per_mtok: 0.6, tags: [fast] }
policies:
  - { name: default, type: fallback, models: [sonnet, mini] }
  - { name: cheap-fast, type: cheapest, tag: fast }
  - { name: ab-test, type: weighted, arms: [{ model: sonnet, weight: 50 }, { model: mini, weight: 50 }] }
  - { name: hedged, type: fallback, models: [mini, sonnet], hedge_after_ms: 2000 }
`))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func newPlaygroundRig(t *testing.T) (*adminRig, *fakePlayground) {
	t.Helper()
	r := newAdminRig(t)
	fp := &fakePlayground{}
	budget := money.Micros(5_000_000)
	cat := playgroundCatalog(t)
	r.admin.d.Playground = &PlaygroundDeps{Service: fp, Catalog: func() *gateway.Catalog { return cat },
		Key: func(context.Context, time.Time) (keys.Stats, error) {
			rpm := 20
			return keys.Stats{Key: keys.Key{Prefix: "spw_play", MonthlyBudget: &budget, RateLimitRPM: &rpm}, Spend: 840_000}, nil
		}}
	r.admin.registerPlayground()
	r.admin.d.Runs = sampleRuns()
	r.admin.d.RunStarter = &fakeStarter{}
	r.admin.d.Events = &fakeEvents{}
	r.admin.d.Now = func() time.Time { return runsNow }
	r.admin.registerRuns()
	r.admin.d.Tools = newFakeTools()
	r.admin.registerTools()
	return r, fp
}

func TestModelsAndPolicies(t *testing.T) {
	r, _ := newPlaygroundRig(t)
	code, body := r.json(t, "GET", "/admin/models", r.token(t, "viewer", "viewer-password"), "")
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	models := body["models"].([]any)
	if len(models) != 2 || models[0].(map[string]any)["id"] != "mini" || models[1].(map[string]any)["input_usd_per_mtok"] != "3.000000" {
		t.Errorf("models = %v", models)
	}
	pol := map[string]map[string]any{}
	for _, p := range body["policies"].([]any) {
		pm := p.(map[string]any)
		pol[pm["name"].(string)] = pm
	}
	if pol["default"]["description"] != "Fallback order: sonnet, mini" || pol["cheap-fast"]["description"] != "Cheapest model tagged fast" ||
		pol["ab-test"]["description"] != "Split: sonnet 50%, mini 50%" || pol["hedged"]["description"] != "mini first; sonnet joins after 2s" {
		t.Errorf("descriptions = %v / %v / %v / %v", pol["default"]["description"], pol["cheap-fast"]["description"], pol["ab-test"]["description"], pol["hedged"]["description"])
	}
	if provs := pol["default"]["providers"].([]any); len(provs) != 2 || provs[0] != "anthropic" || provs[1] != "openai" {
		t.Errorf("the providers are listed in the order they are tried: %v", provs)
	}
	if provs := pol["hedged"]["providers"].([]any); provs[0] != "openai" {
		t.Errorf("hedged tries openai first: %v", provs)
	}
}

func TestPlaygroundState(t *testing.T) {
	r, _ := newPlaygroundRig(t)
	code, body := r.json(t, "GET", "/admin/playground", r.token(t, "viewer", "viewer-password"), "")
	key := body["key"].(map[string]any)
	if code != 200 || key["spend_usd"] != "0.840000" || key["monthly_budget_usd"] != "5.000000" || key["rate_limit_rpm"].(float64) != 20 || body["fault_injection"] != true || len(body["policies"].([]any)) != 4 {
		t.Errorf("%d %v", code, body)
	}
}

func TestPlaygroundChatPassesTheRequestThrough(t *testing.T) {
	r, fp := newPlaygroundRig(t)
	admin := r.token(t, "admin", "admin-password")
	code, body := r.json(t, "POST", "/admin/playground/chat", admin, `{"policy":"hedged","prompt":"hi","system":"s","temperature":0.5,"max_tokens":100,"stream":true,"faults":[{"provider":"anthropic","kind":"rate_limit"}]}`)
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	in := fp.last
	if in.Policy != "hedged" || in.Prompt != "hi" || in.System != "s" || *in.Temperature != 0.5 || *in.MaxTokens != 100 || !in.Stream || len(in.Faults) != 1 || in.Faults[0].Provider != "anthropic" || in.Faults[0].Kind != faults.RateLimit {
		t.Errorf("service saw %+v", in)
	}
	a := body["attempts"].([]any)
	if body["answer"] != "an answer" || len(a) != 2 || a[0].(map[string]any)["injected"] != true || body["overhead_ms"].(float64) != 3 {
		t.Errorf("body = %v", body)
	}
	// The policy defaults to "default".
	r.json(t, "POST", "/admin/playground/chat", admin, `{"prompt":"hi"}`)
	if fp.last.Policy != "default" {
		t.Errorf("policy = %q", fp.last.Policy)
	}
}

func TestPlaygroundChatIsAdminOnly(t *testing.T) {
	r, fp := newPlaygroundRig(t)
	code, body := r.json(t, "POST", "/admin/playground/chat", r.token(t, "viewer", "viewer-password"), `{"prompt":"hi","faults":[{"provider":"openai","kind":"slow"}]}`)
	if code != 403 || body["error"].(map[string]any)["code"] != "forbidden" {
		t.Errorf("%d %v", code, body)
	}
	if fp.last.Prompt != "" {
		t.Error("a viewer's request must never reach the service")
	}
}

func TestPlaygroundChatErrors(t *testing.T) {
	r, fp := newPlaygroundRig(t)
	admin := r.token(t, "admin", "admin-password")
	tests := []struct {
		name string
		err  error
		body string
		code int
		want string
	}{
		{"malformed", nil, `{`, 400, "invalid_request"},
		{"unknown field", nil, `{"prompt":"x","secret":1}`, 400, "invalid_request"},
		{"fault with an unknown field", nil, `{"prompt":"x","faults":[{"provider":"a","kind":"slow","extra":1}]}`, 400, "invalid_request"},
		{"refused by the service", errors.Join(playground.ErrInvalid), `{"prompt":"x"}`, 400, "invalid_request"},
		{"budget", &gateway.Error{Status: 402, Code: "budget_exceeded", Message: "used up"}, `{"prompt":"x"}`, 402, "budget_exceeded"},
		{"rate limit", &gateway.Error{Status: 429, Code: "rate_limit_exceeded", Message: "slow down", RetryAfter: 7 * time.Second}, `{"prompt":"x"}`, 429, "rate_limit_exceeded"},
		{"anything else", errors.New("boom"), `{"prompt":"x"}`, 500, "internal_error"},
	}
	for _, tc := range tests {
		fp.err = tc.err
		resp := r.do(t, "POST", "/admin/playground/chat", admin, tc.body)
		body := decode(t, resp)
		if resp.StatusCode != tc.code || body["error"].(map[string]any)["code"] != tc.want {
			t.Errorf("%s: %d %v", tc.name, resp.StatusCode, body)
		}
		if tc.code == 429 && resp.Header.Get("Retry-After") != "7" {
			t.Errorf("Retry-After = %q", resp.Header.Get("Retry-After"))
		}
		if tc.code == 500 && strings.Contains(decodeMsg(body), "boom") {
			t.Error("internal errors must not leak their text")
		}
	}
}

func decodeMsg(b map[string]any) string { return b["error"].(map[string]any)["message"].(string) }

func TestPlaygroundHistory(t *testing.T) {
	r, fp := newPlaygroundRig(t)
	viewer := r.token(t, "viewer", "viewer-password")
	code, body := r.json(t, "GET", "/admin/playground/history", viewer, "")
	if code != 200 || body["next_cursor"] != "NEXT" || len(body["requests"].([]any)) != 1 {
		t.Fatalf("%d %v", code, body)
	}
	item := body["requests"].([]any)[0].(map[string]any)
	if item["prompt"] != "p" || item["answer"] != "a" || item["cost_usd"] != "0.000100" {
		t.Errorf("item = %v", item)
	}
	if code, _ := r.json(t, "GET", "/admin/playground/history?limit=0", viewer, ""); code != 400 {
		t.Errorf("limit=0: %d", code)
	}
	fp.err = playground.ErrBadCursor
	if code, _ := r.json(t, "GET", "/admin/playground/history?cursor=x", viewer, ""); code != 400 {
		t.Errorf("bad cursor: %d", code)
	}
}
