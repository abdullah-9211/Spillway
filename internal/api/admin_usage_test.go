package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/gateway"
	"github.com/abdullah-9211/spillway/internal/usage"
)

// fakeUsage returns fixed reports and remembers what it was asked for.
type fakeUsage struct {
	rng     usage.Range
	key     *uuid.UUID
	groupBy string
	stack   string
	filter  usage.RequestFilter
}

func (f *fakeUsage) Summary(_ context.Context, rng usage.Range, key *uuid.UUID, groupBy, stack string) (*usage.Summary, error) {
	f.rng, f.key, f.groupBy, f.stack = rng, key, groupBy, stack
	if stack != "" && stack != "model" && stack != "key" {
		return nil, usage.ErrBadStack
	}
	if stack == "" {
		stack = "model"
	}
	if groupBy == "" {
		groupBy = "day"
	}
	if groupBy != "day" && groupBy != "model" && groupBy != "key" {
		return nil, usage.ErrBadGroup
	}
	s := &usage.Summary{Range: rng, GroupBy: groupBy, Stack: stack, Fallbacks: 3,
		Totals: usage.Totals{Metrics: usage.Metrics{Requests: 10, InputTokens: 1000, OutputTokens: 200, Cost: 9_000_000, Saved: 1_000_000, CacheHits: 2, Errors: 1}, Rejected: 1,
			Cache: usage.CacheSplit{Miss: 6, HitExact: 1, HitSemantic: 1, Bypass: 2}},
		Providers: []usage.ProviderAttempts{{Provider: "anthropic", FailedAttempts: 4, FallbacksTo: 0}, {Provider: "openai", FailedAttempts: 0, FallbacksTo: 3}}}
	switch groupBy {
	case "day":
		s.Groups = []usage.Group{{Key: "2026-10-08", Label: "2026-10-08", Metrics: usage.Metrics{Requests: 10, Cost: 9_000_000}, Series: map[string]usage.SeriesStat{
			"sonnet": {Label: "sonnet", Requests: 6, InputTokens: 600, OutputTokens: 100, Cost: 7_000_000, Saved: 500_000},
			"mini":   {Label: "mini", Requests: 4, InputTokens: 400, OutputTokens: 100, Cost: 2_000_000}}}}
	case "model":
		s.Groups = []usage.Group{{Key: "sonnet", Label: "sonnet", Metrics: usage.Metrics{Requests: 6, Cost: 7_000_000}, TimedRequests: 5, P50Ms: 1900, P95Ms: 5200},
			{Key: "local", Label: "local", Metrics: usage.Metrics{Requests: 4}}}
	case "key":
		s.Groups = []usage.Group{{Key: uuid.NewString(), Label: "ci", Metrics: usage.Metrics{Requests: 10, Cost: 9_000_000}}}
	}
	return s, nil
}

func (f *fakeUsage) Requests(_ context.Context, fl usage.RequestFilter) ([]usage.RequestRow, string, error) {
	f.filter = fl
	if fl.Cursor == "bad" {
		return nil, "", usage.ErrBadCursor
	}
	kid := uuid.New()
	rows := []usage.RequestRow{{ID: uuid.New(), KeyID: &kid, KeyName: "ci", Policy: "default", Provider: "openai", Model: "mini", InputTokens: 10, OutputTokens: 5,
		Cost: 105, LatencyMs: 120, CacheStatus: "miss", Outcome: "ok", CreatedAt: time.Now(),
		Attempts: []usage.Attempt{{Provider: "anthropic", Model: "sonnet", Kind: "primary", ErrorKind: "server", Error: "boom"}, {Provider: "openai", Model: "mini", Kind: "fallback"}}}}
	return rows, "NEXT", nil
}

func (f *fakeUsage) WriteCSV(_ context.Context, w io.Writer, rng usage.Range, key *uuid.UUID) error {
	f.rng, f.key = rng, key
	_, err := io.WriteString(w, "day,key,model\n2026-10-08,ci,sonnet\n")
	return err
}

type fakeLatency struct{ ok bool }

func (l fakeLatency) OverheadQuantiles(qs ...float64) ([]float64, uint64, time.Time, bool) {
	return []float64{0.003, 0.014, 0.029}, 480, time.Unix(1_800_000_000, 0), l.ok
}

func testHealth() []gateway.ProviderHealth {
	return []gateway.ProviderHealth{
		{Name: "anthropic", State: "closed"},
		{Name: "google", State: "open", OpenRemaining: 20500 * time.Millisecond},
		{Name: "openai", State: "half_open"},
		{Name: "ollama", State: "not_configured", Reason: "no URL"},
	}
}

func TestUsageSummaryShape(t *testing.T) {
	r := newAdminRig(t)
	viewer := r.token(t, "viewer", "viewer-password")
	code, body := r.json(t, "GET", "/admin/usage/summary?from=2026-10-01&to=2026-10-08&group_by=model", viewer, "")
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	rng := body["range"].(map[string]any)
	if rng["from"] != "2026-10-01" || rng["to"] != "2026-10-08" || rng["days"].(float64) != 8 {
		t.Errorf("range = %v (to is inclusive)", rng)
	}
	tot := body["totals"].(map[string]any)
	if tot["cost_usd"] != "9.000000" || tot["saved_usd"] != "1.000000" || tot["saved_share"].(float64) != 0.1 || tot["requests"].(float64) != 10 {
		t.Errorf("totals = %v", tot)
	}
	if c := tot["cache"].(map[string]any); c["hit_exact"].(float64) != 1 || c["bypass"].(float64) != 2 {
		t.Errorf("cache = %v", c)
	}
	groups := body["groups"].([]any)
	sonnet, local := groups[0].(map[string]any), groups[1].(map[string]any)
	if sonnet["p50_ms"].(float64) != 1900 || sonnet["cost_usd"] != "7.000000" {
		t.Errorf("sonnet = %v", sonnet)
	}
	if local["p50_ms"] != nil {
		t.Errorf("a model with no timed requests has no latency figures, got %v", local["p50_ms"])
	}
	if body["fallbacks_fired"].(float64) != 3 {
		t.Errorf("fallbacks = %v", body["fallbacks_fired"])
	}
	lat := body["added_latency"].(map[string]any)
	if lat["p95_ms"].(float64) != 14 || lat["samples"].(float64) != 480 {
		t.Errorf("latency = %v", lat)
	}

	byName := map[string]map[string]any{}
	for _, p := range body["providers"].([]any) {
		pm := p.(map[string]any)
		byName[pm["name"].(string)] = pm
	}
	if byName["google"]["state"] != "open" || byName["google"]["open_remaining_seconds"].(float64) != 21 {
		t.Errorf("an open breaker reports its remaining wait, rounded up: %v", byName["google"])
	}
	if byName["anthropic"]["failed_attempts"].(float64) != 4 || byName["openai"]["fallbacks_to"].(float64) != 3 {
		t.Errorf("attempt counts are joined to providers: %v", byName)
	}
	if byName["ollama"]["state"] != "not_configured" || byName["ollama"]["reason"] != "no URL" {
		t.Errorf("ollama = %v", byName["ollama"])
	}
}

func TestUsageSummaryDayGroupsCarryPerModelCost(t *testing.T) {
	r := newAdminRig(t)
	_, body := r.json(t, "GET", "/admin/usage/summary", r.token(t, "viewer", "viewer-password"), "")
	day := body["groups"].([]any)[0].(map[string]any)
	series := day["series"].(map[string]any)
	sonnet := series["sonnet"].(map[string]any)
	if body["group_by"] != "day" || body["stack"] != "model" || sonnet["cost_usd"] != "7.000000" || sonnet["requests"].(float64) != 6 ||
		sonnet["input_tokens"].(float64) != 600 || sonnet["saved_usd"] != "0.500000" || sonnet["label"] != "sonnet" || series["mini"].(map[string]any)["cost_usd"] != "2.000000" {
		t.Errorf("group_by defaults to day, split by model, with each model's figures: %v", body)
	}
}

func TestUsageParametersAreValidated(t *testing.T) {
	r := newAdminRig(t)
	viewer := r.token(t, "viewer", "viewer-password")
	for name, path := range map[string]string{
		"bad from":        "/admin/usage/summary?from=yesterday",
		"bad to":          "/admin/usage/summary?to=2026-13-40",
		"from after to":   "/admin/usage/summary?from=2026-10-09&to=2026-10-01",
		"too long":        "/admin/usage/summary?from=2020-01-01&to=2026-10-01",
		"bad key":         "/admin/usage/summary?key_id=nope",
		"bad group":       "/admin/usage/summary?group_by=planet",
		"bad stack":       "/admin/usage/summary?stack=colour",
		"bad limit":       "/admin/usage/requests?limit=0",
		"text limit":      "/admin/usage/requests?limit=many",
		"bad cursor":      "/admin/usage/requests?cursor=bad",
		"export bad date": "/admin/usage/export.csv?from=x",
	} {
		code, body := r.json(t, "GET", path, viewer, "")
		if code != 400 || body["error"].(map[string]any)["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", name, code, body)
		}
	}
}

func TestUsageQueryReachesTheReader(t *testing.T) {
	r := newAdminRig(t)
	viewer := r.token(t, "viewer", "viewer-password")
	id := uuid.New()
	r.json(t, "GET", "/admin/usage/summary?from=2026-10-01&to=2026-10-03&key_id="+id.String()+"&group_by=key", viewer, "")
	r.json(t, "GET", "/admin/usage/summary?stack=key", viewer, "")
	if r.usage.stack != "key" {
		t.Errorf("stack = %q, want key", r.usage.stack)
	}
	r.json(t, "GET", "/admin/usage/summary?from=2026-10-01&to=2026-10-03&key_id="+id.String()+"&group_by=key", viewer, "")
	if got := r.usage; got.groupBy != "key" || got.key == nil || *got.key != id ||
		!got.rng.From.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) || !got.rng.To.Equal(time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("reader saw %+v", got)
	}
	r.json(t, "GET", "/admin/usage/requests?model=mini&outcome=ok&policy=default&limit=5&cursor=abc", viewer, "")
	f := r.usage.filter
	if f.Model != "mini" || f.Outcome != "ok" || f.Policy != "default" || f.Limit != 5 || f.Cursor != "abc" {
		t.Errorf("filter = %+v", f)
	}
}

func TestRequestLogPage(t *testing.T) {
	r := newAdminRig(t)
	code, body := r.json(t, "GET", "/admin/usage/requests", r.token(t, "viewer", "viewer-password"), "")
	if code != 200 || body["next_cursor"] != "NEXT" {
		t.Fatalf("%d %v", code, body)
	}
	row := body["requests"].([]any)[0].(map[string]any)
	attempts := row["attempts"].([]any)
	if row["cost_usd"] != "0.000105" || row["cache_status"] != "miss" || len(attempts) != 2 || attempts[1].(map[string]any)["kind"] != "fallback" {
		t.Errorf("row = %v", row)
	}
}

func TestExportIsACSVDownload(t *testing.T) {
	r := newAdminRig(t)
	resp := r.do(t, "GET", "/admin/usage/export.csv?from=2026-10-01&to=2026-10-08", r.token(t, "viewer", "viewer-password"), "")
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="spillway-usage-2026-10-01-to-2026-10-08.csv"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if !strings.HasPrefix(string(raw), "day,key,model\n") {
		t.Errorf("body = %q", raw)
	}
}

func TestUsageNeedsASession(t *testing.T) {
	r := newAdminRig(t)
	for _, p := range []string{"/admin/usage/summary", "/admin/usage/requests", "/admin/usage/export.csv"} {
		if code := r.do(t, "GET", p, "", "").StatusCode; code != http.StatusUnauthorized {
			t.Errorf("%s without a token: %d", p, code)
		}
	}
}
