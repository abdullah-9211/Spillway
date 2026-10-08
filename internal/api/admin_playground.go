package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/abdullah-9211/spillway/internal/auth"
	"github.com/abdullah-9211/spillway/internal/gateway"
	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/playground"
	"github.com/abdullah-9211/spillway/internal/provider/faults"
)

// PlaygroundAdmin is what the playground endpoints need.
type PlaygroundAdmin interface {
	Run(ctx context.Context, in playground.Input) (*playground.Output, error)
	History(ctx context.Context, limit int, cursor string) ([]playground.HistoryItem, string, error)
	FaultInjection() bool
}

// PlaygroundDeps are the playground's collaborators beyond the service itself.
type PlaygroundDeps struct {
	Service PlaygroundAdmin
	Catalog func() *gateway.Catalog
	// Key reports the built-in playground key with this month's spend.
	Key func(ctx context.Context, now time.Time) (keys.Stats, error)
}

func (a *Admin) registerPlayground() {
	a.handle("GET", "/admin/models", AccessViewer, a.models)
	a.handle("GET", "/admin/playground", AccessViewer, a.playgroundState)
	a.handle("POST", "/admin/playground/chat", AccessAdmin, a.playgroundChat)
	a.handle("GET", "/admin/playground/history", AccessViewer, a.playgroundHistory)
}

type modelBody struct {
	ID            string   `json:"id"`
	Provider      string   `json:"provider"`
	Upstream      string   `json:"upstream"`
	Tags          []string `json:"tags"`
	ContextWindow int      `json:"context_window"`
	InputUSDPerM  string   `json:"input_usd_per_mtok"`
	OutputUSDPerM string   `json:"output_usd_per_mtok"`
}

type modelRef struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
}

type policyBody struct {
	Name         string     `json:"name"`
	Type         string     `json:"type"`
	Description  string     `json:"description"`
	Models       []modelRef `json:"models"`
	Providers    []string   `json:"providers"` // distinct, in the order they would be tried
	Weights      []int      `json:"weights"`
	Tag          string     `json:"tag"`
	HedgeAfterMs int        `json:"hedge_after_ms"`
}

func catalogBodies(c *gateway.Catalog) ([]modelBody, []policyBody) {
	ms, ps := c.Describe()
	models := make([]modelBody, 0, len(ms))
	for _, m := range ms {
		tags := m.Tags
		if tags == nil {
			tags = []string{}
		}
		models = append(models, modelBody{ID: m.ID, Provider: m.Provider, Upstream: m.Upstream, Tags: tags, ContextWindow: m.ContextWindow,
			InputUSDPerM: usd(m.InputPerMtok), OutputUSDPerM: usd(m.OutputPerMtok)})
	}
	policies := make([]policyBody, 0, len(ps))
	for _, p := range ps {
		pb := policyBody{Name: p.Name, Type: p.Type, Description: p.Description, Tag: p.Tag, HedgeAfterMs: p.HedgeAfterMs,
			Models: []modelRef{}, Providers: []string{}, Weights: p.Weights}
		if pb.Weights == nil {
			pb.Weights = []int{}
		}
		seen := map[string]bool{}
		for _, m := range p.Models {
			pb.Models = append(pb.Models, modelRef{ID: m.ID, Provider: m.Provider})
			if !seen[m.Provider] {
				seen[m.Provider] = true
				pb.Providers = append(pb.Providers, m.Provider)
			}
		}
		policies = append(policies, pb)
	}
	return models, policies
}

func (a *Admin) models(w http.ResponseWriter, _ *http.Request, _ auth.Claims) {
	models, policies := catalogBodies(a.d.Playground.Catalog())
	writeJSON(w, http.StatusOK, map[string]any{"models": models, "policies": policies})
}

type playgroundKeyBody struct {
	Prefix           string  `json:"prefix"`
	MonthlyBudgetUSD *string `json:"monthly_budget_usd"`
	SpendUSD         string  `json:"spend_usd"`
	RateLimitRPM     *int    `json:"rate_limit_rpm"`
}

func (a *Admin) playgroundState(w http.ResponseWriter, r *http.Request, _ auth.Claims) {
	st, err := a.d.Playground.Key(r.Context(), time.Now())
	if err != nil {
		a.d.Log.Error("playground key", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "internal_error", "Could not load the playground.")
		return
	}
	kb := playgroundKeyBody{Prefix: st.Prefix, SpendUSD: usd(st.Spend), RateLimitRPM: st.RateLimitRPM}
	if st.MonthlyBudget != nil {
		s := usd(*st.MonthlyBudget)
		kb.MonthlyBudgetUSD = &s
	}
	_, policies := catalogBodies(a.d.Playground.Catalog())
	writeJSON(w, http.StatusOK, map[string]any{"key": kb, "fault_injection": a.d.Playground.Service.FaultInjection(), "policies": policies})
}

type chatRequest struct {
	Policy      string         `json:"policy"`
	Prompt      string         `json:"prompt"`
	System      string         `json:"system"`
	Temperature *float64       `json:"temperature"`
	MaxTokens   *int           `json:"max_tokens"`
	Stream      bool           `json:"stream"`
	Faults      []faults.Fault `json:"faults"`
}

func (a *Admin) playgroundChat(w http.ResponseWriter, r *http.Request, _ auth.Claims) {
	var req chatRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.Policy == "" {
		req.Policy = "default"
	}
	out, err := a.d.Playground.Service.Run(r.Context(), playground.Input{Policy: req.Policy, Prompt: req.Prompt, System: req.System,
		Temperature: req.Temperature, MaxTokens: req.MaxTokens, Stream: req.Stream, Faults: req.Faults})
	var ge *gateway.Error
	switch {
	case errors.Is(err, playground.ErrInvalid):
		writeAdminError(w, http.StatusBadRequest, "invalid_request", trimPrefix(err.Error(), playground.ErrInvalid.Error()+": "))
	case errors.As(err, &ge) && (ge.Status == 402 || ge.Status == 429 || ge.Status == 400):
		if ge.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(ge.RetryAfter.Round(time.Second).Seconds())))
		}
		code := ge.Code
		if ge.Status == 402 {
			code = "budget_exceeded"
		}
		writeAdminError(w, ge.Status, code, ge.Message)
	case err != nil:
		a.d.Log.Error("playground chat", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "internal_error", "The playground request failed. Try again.")
	default:
		writeJSON(w, http.StatusOK, out)
	}
}

func trimPrefix(s, p string) string {
	if len(s) >= len(p) && s[:len(p)] == p {
		return s[len(p):]
	}
	return s
}

func (a *Admin) playgroundHistory(w http.ResponseWriter, r *http.Request, _ auth.Claims) {
	limit := 0
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			writeAdminError(w, http.StatusBadRequest, "invalid_request", "limit must be a positive number.")
			return
		}
		limit = n
	}
	items, next, err := a.d.Playground.Service.History(r.Context(), limit, r.URL.Query().Get("cursor"))
	if errors.Is(err, playground.ErrBadCursor) {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "That cursor is not valid.")
		return
	}
	if err != nil {
		a.d.Log.Error("playground history", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "internal_error", "Could not load the history.")
		return
	}
	if items == nil {
		items = []playground.HistoryItem{}
	}
	var cursor *string
	if next != "" {
		cursor = &next
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": items, "next_cursor": cursor})
}
