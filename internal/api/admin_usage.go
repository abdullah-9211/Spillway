package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/auth"
	"github.com/abdullah-9211/spillway/internal/money"
	"github.com/abdullah-9211/spillway/internal/usage"
)

// UsageAdmin is what the usage endpoints need from the reports.
type UsageAdmin interface {
	Summary(ctx context.Context, rng usage.Range, keyID *uuid.UUID, groupBy, stack string) (*usage.Summary, error)
	Requests(ctx context.Context, f usage.RequestFilter) ([]usage.RequestRow, string, error)
	WriteCSV(ctx context.Context, w io.Writer, rng usage.Range, keyID *uuid.UUID) error
}

// LatencySource reports Spillway's own added latency.
type LatencySource interface {
	OverheadQuantiles(qs ...float64) (values []float64, samples uint64, since time.Time, ok bool)
}

func (a *Admin) registerUsage() {
	a.handle("GET", "/admin/usage/summary", AccessViewer, a.usageSummary)
	a.handle("GET", "/admin/usage/requests", AccessViewer, a.usageRequests)
	a.handle("GET", "/admin/usage/export.csv", AccessViewer, a.usageExport)
}

func usd(m money.Micros) string { return m.String() }

type cacheBody struct {
	Miss        int64 `json:"miss"`
	HitExact    int64 `json:"hit_exact"`
	HitSemantic int64 `json:"hit_semantic"`
	Bypass      int64 `json:"bypass"`
}

type totalsBody struct {
	Requests     int64     `json:"requests"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	CostUSD      string    `json:"cost_usd"`
	SavedUSD     string    `json:"saved_usd"`
	SavedShare   float64   `json:"saved_share"` // saved / (spent + saved): the part of the bill the caches avoided
	CacheHits    int64     `json:"cache_hits"`
	Errors       int64     `json:"errors"`
	Rejected     int64     `json:"rejected"`
	Cache        cacheBody `json:"cache"`
}

type groupBody struct {
	Key           string                `json:"key"`
	Label         string                `json:"label"`
	Requests      int64                 `json:"requests"`
	InputTokens   int64                 `json:"input_tokens"`
	OutputTokens  int64                 `json:"output_tokens"`
	CostUSD       string                `json:"cost_usd"`
	SavedUSD      string                `json:"saved_usd"`
	CacheHits     int64                 `json:"cache_hits"`
	Errors        int64                 `json:"errors"`
	TimedRequests int64                 `json:"timed_requests"`
	P50Ms         *float64              `json:"p50_ms"`
	P95Ms         *float64              `json:"p95_ms"`
	Series        map[string]seriesBody `json:"series,omitempty"`
}

// seriesBody is one slice of a day: a model's or a key's share of it.
type seriesBody struct {
	Label        string `json:"label"`
	Requests     int64  `json:"requests"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	CostUSD      string `json:"cost_usd"`
	SavedUSD     string `json:"saved_usd"`
}

type providerBody struct {
	Name                string `json:"name"`
	State               string `json:"state"`
	OpenRemainingSecond *int   `json:"open_remaining_seconds"`
	Reason              string `json:"reason,omitempty"`
	FailedAttempts      int64  `json:"failed_attempts"`
	FallbacksTo         int64  `json:"fallbacks_to"`
}

type latencyBody struct {
	P50Ms   float64   `json:"p50_ms"`
	P95Ms   float64   `json:"p95_ms"`
	P99Ms   float64   `json:"p99_ms"`
	Samples uint64    `json:"samples"`
	Since   time.Time `json:"since"`
}

type summaryBody struct {
	Range struct {
		From string `json:"from"`
		To   string `json:"to"`
		Days int    `json:"days"`
	} `json:"range"`
	GroupBy        string         `json:"group_by"`
	Stack          string         `json:"stack"`
	Totals         totalsBody     `json:"totals"`
	AddedLatency   *latencyBody   `json:"added_latency"`
	Providers      []providerBody `json:"providers"`
	FallbacksFired int64          `json:"fallbacks_fired"`
	Groups         []groupBody    `json:"groups"`
}

func ratio(saved, spent money.Micros) float64 {
	if saved+spent <= 0 {
		return 0
	}
	return float64(saved) / float64(saved+spent)
}

// reportParams reads the range and the optional key from a query string.
func reportParams(w http.ResponseWriter, q url.Values) (usage.Range, *uuid.UUID, bool) {
	rng, err := usage.ParseRange(q.Get("from"), q.Get("to"), time.Now())
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return usage.Range{}, nil, false
	}
	var key *uuid.UUID
	if s := q.Get("key_id"); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			writeAdminError(w, http.StatusBadRequest, "invalid_request", "key_id must be a key's id.")
			return usage.Range{}, nil, false
		}
		key = &id
	}
	return rng, key, true
}

func (a *Admin) usageSummary(w http.ResponseWriter, r *http.Request, _ auth.Claims) {
	q := r.URL.Query()
	rng, key, ok := reportParams(w, q)
	if !ok {
		return
	}
	s, err := a.d.Usage.Summary(r.Context(), rng, key, q.Get("group_by"), q.Get("stack"))
	if errors.Is(err, usage.ErrBadGroup) || errors.Is(err, usage.ErrBadStack) {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err != nil {
		a.d.Log.Error("usage summary", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "internal_error", "Could not build the report.")
		return
	}

	var out summaryBody
	out.Range.From, out.Range.To, out.Range.Days = rng.From.Format("2006-01-02"), rng.To.AddDate(0, 0, -1).Format("2006-01-02"), rng.Days()
	out.GroupBy, out.Stack = s.GroupBy, s.Stack
	t := s.Totals
	out.Totals = totalsBody{Requests: t.Requests, InputTokens: t.InputTokens, OutputTokens: t.OutputTokens, CostUSD: usd(t.Cost), SavedUSD: usd(t.Saved),
		SavedShare: ratio(t.Saved, t.Cost), CacheHits: t.CacheHits, Errors: t.Errors, Rejected: t.Rejected,
		Cache: cacheBody{Miss: t.Cache.Miss, HitExact: t.Cache.HitExact, HitSemantic: t.Cache.HitSemantic, Bypass: t.Cache.Bypass}}
	out.FallbacksFired = s.Fallbacks

	out.Groups = make([]groupBody, 0, len(s.Groups))
	for _, g := range s.Groups {
		gb := groupBody{Key: g.Key, Label: g.Label, Requests: g.Requests, InputTokens: g.InputTokens, OutputTokens: g.OutputTokens,
			CostUSD: usd(g.Cost), SavedUSD: usd(g.Saved), CacheHits: g.CacheHits, Errors: g.Errors, TimedRequests: g.TimedRequests}
		if s.GroupBy == "model" && g.TimedRequests > 0 {
			p50, p95 := g.P50Ms, g.P95Ms
			gb.P50Ms, gb.P95Ms = &p50, &p95
		}
		if s.GroupBy == "day" {
			gb.Series = map[string]seriesBody{}
			for id, st := range g.Series {
				gb.Series[id] = seriesBody{Label: st.Label, Requests: st.Requests, InputTokens: st.InputTokens, OutputTokens: st.OutputTokens,
					CostUSD: usd(st.Cost), SavedUSD: usd(st.Saved)}
			}
		}
		out.Groups = append(out.Groups, gb)
	}

	attempts := map[string]usage.ProviderAttempts{}
	for _, p := range s.Providers {
		attempts[p.Provider] = p
	}
	out.Providers = []providerBody{}
	if a.d.Health != nil {
		for _, h := range a.d.Health() {
			pb := providerBody{Name: h.Name, State: h.State, Reason: h.Reason, FailedAttempts: attempts[h.Name].FailedAttempts, FallbacksTo: attempts[h.Name].FallbacksTo}
			if h.State == "open" {
				secs := int((h.OpenRemaining + time.Second - 1) / time.Second)
				pb.OpenRemainingSecond = &secs
			}
			out.Providers = append(out.Providers, pb)
		}
	}
	if a.d.Latency != nil {
		if v, n, since, ok := a.d.Latency.OverheadQuantiles(0.5, 0.95, 0.99); ok {
			out.AddedLatency = &latencyBody{P50Ms: v[0] * 1000, P95Ms: v[1] * 1000, P99Ms: v[2] * 1000, Samples: n, Since: since.UTC()}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

type requestBody struct {
	ID           string          `json:"id"`
	KeyID        *string         `json:"key_id"`
	KeyName      string          `json:"key_name"`
	Policy       string          `json:"policy"`
	Provider     string          `json:"provider"`
	Model        string          `json:"model"`
	InputTokens  int             `json:"input_tokens"`
	OutputTokens int             `json:"output_tokens"`
	CostUSD      string          `json:"cost_usd"`
	SavedUSD     string          `json:"saved_usd"`
	LatencyMs    int             `json:"latency_ms"`
	TTFBMs       *int            `json:"ttfb_ms"`
	CacheStatus  string          `json:"cache_status"`
	Outcome      string          `json:"outcome"`
	Attempts     []usage.Attempt `json:"attempts"`
	CreatedAt    time.Time       `json:"created_at"`
}

func (a *Admin) usageRequests(w http.ResponseWriter, r *http.Request, _ auth.Claims) {
	q := r.URL.Query()
	rng, key, ok := reportParams(w, q)
	if !ok {
		return
	}
	limit := 0
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			writeAdminError(w, http.StatusBadRequest, "invalid_request", "limit must be a positive number.")
			return
		}
		limit = n
	}
	rows, next, err := a.d.Usage.Requests(r.Context(), usage.RequestFilter{Range: rng, KeyID: key, Model: q.Get("model"),
		Outcome: q.Get("outcome"), Policy: q.Get("policy"), Limit: limit, Cursor: q.Get("cursor")})
	if errors.Is(err, usage.ErrBadCursor) {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "That cursor is not valid.")
		return
	}
	if err != nil {
		a.d.Log.Error("usage requests", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "internal_error", "Could not load the requests.")
		return
	}
	out := make([]requestBody, 0, len(rows))
	for _, x := range rows {
		b := requestBody{ID: x.ID.String(), KeyName: x.KeyName, Policy: x.Policy, Provider: x.Provider, Model: x.Model,
			InputTokens: x.InputTokens, OutputTokens: x.OutputTokens, CostUSD: usd(x.Cost), SavedUSD: usd(x.Saved), LatencyMs: x.LatencyMs,
			TTFBMs: x.TTFBMs, CacheStatus: x.CacheStatus, Outcome: x.Outcome, Attempts: x.Attempts, CreatedAt: x.CreatedAt}
		if b.Attempts == nil {
			b.Attempts = []usage.Attempt{}
		}
		if x.KeyID != nil {
			s := x.KeyID.String()
			b.KeyID = &s
		}
		out = append(out, b)
	}
	var nextCursor *string
	if next != "" {
		nextCursor = &next
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": out, "next_cursor": nextCursor})
}

func (a *Admin) usageExport(w http.ResponseWriter, r *http.Request, _ auth.Claims) {
	rng, key, ok := reportParams(w, r.URL.Query())
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="spillway-usage-`+rng.From.Format("2006-01-02")+`-to-`+rng.To.AddDate(0, 0, -1).Format("2006-01-02")+`.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	if err := a.d.Usage.WriteCSV(r.Context(), w, rng, key); err != nil {
		// Headers are out already for a partial file; log it, since the status can no longer change.
		a.d.Log.Error("usage export", "error", err)
	}
}
