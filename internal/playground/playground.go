// Package playground runs one prompt through the real gateway path under the dashboard's built-in key, optionally
// with providers made to fail on purpose, and remembers what happened so it can be looked at again.
package playground

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/abdullah-9211/spillway/internal/db/sqlcgen"
	"github.com/abdullah-9211/spillway/internal/gateway"
	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/money"
	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/provider/faults"
	"github.com/abdullah-9211/spillway/internal/usage"
)

const (
	MaxPrompt    = 8000
	MaxSystem    = 4000
	MaxTokensCap = 4096 // the playground spends real money, so answers are bounded
	DefaultMax   = 512
	MaxFaults    = 8
	maxStored    = 16000
)

// ErrInvalid is a request the playground refuses; its message says why.
var ErrInvalid = errors.New("invalid playground request")

func invalidf(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

type Input struct {
	Policy      string
	Prompt      string
	System      string
	Temperature *float64
	MaxTokens   *int
	Stream      bool
	Faults      []faults.Fault
}

// ErrorInfo is why a request produced no answer, or only part of one.
type ErrorInfo struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Output struct {
	ID           uuid.UUID       `json:"id"`
	CreatedAt    time.Time       `json:"created_at"`
	Policy       string          `json:"policy"`
	Stream       bool            `json:"stream"`
	Answer       string          `json:"answer"`
	FinishReason string          `json:"finish_reason"`
	Provider     string          `json:"provider"`
	Model        string          `json:"model"`
	Cache        string          `json:"cache"`
	InputTokens  int             `json:"input_tokens"`
	OutputTokens int             `json:"output_tokens"`
	CostUSD      string          `json:"cost_usd"`
	SavedUSD     string          `json:"saved_usd"`
	LatencyMs    int             `json:"latency_ms"`
	OverheadMs   int             `json:"overhead_ms"` // time spent in Spillway rather than at a provider
	Outcome      string          `json:"outcome"`
	Error        *ErrorInfo      `json:"error"`
	Attempts     []usage.Attempt `json:"attempts"`
	Faults       []faults.Fault  `json:"faults"`
	UnusedFaults []faults.Fault  `json:"unused_faults"` // faults that named a provider the policy would not try
}

// Gateway is the part of the gateway the playground uses.
type Gateway interface {
	Chat(ctx context.Context, key keys.Key, id uuid.UUID, bucket string, req *provider.ChatRequest) (*gateway.Result, error)
	ChatStream(ctx context.Context, key keys.Key, id uuid.UUID, bucket string, req *provider.ChatRequest) (*gateway.Stream, error)
	CandidateProviders(model string) ([]string, error)
	Catalog() *gateway.Catalog
}

type Service struct {
	gw             Gateway
	key            func(context.Context) (keys.Key, error)
	store          *Store
	log            *slog.Logger
	faultInjection bool
	settings       faults.Settings
	now            func() time.Time
}

func NewService(gw Gateway, key func(context.Context) (keys.Key, error), store *Store, faultInjection bool, log *slog.Logger) *Service {
	return &Service{gw: gw, key: key, store: store, log: log, faultInjection: faultInjection, now: time.Now}
}

// FaultInjection reports whether the config allows faults at all.
func (s *Service) FaultInjection() bool { return s.faultInjection }

func (s *Service) validate(in *Input) error {
	in.Prompt = strings.TrimSpace(in.Prompt)
	switch {
	case in.Prompt == "":
		return invalidf("write a prompt first")
	case utf8.RuneCountInString(in.Prompt) > MaxPrompt:
		return invalidf("the prompt is longer than %d characters", MaxPrompt)
	case utf8.RuneCountInString(in.System) > MaxSystem:
		return invalidf("the system prompt is longer than %d characters", MaxSystem)
	}
	cat := s.gw.Catalog()
	if _, isModel := cat.Models[in.Policy]; !isModel {
		if _, isPolicy := cat.Policies[in.Policy]; !isPolicy {
			return invalidf("there is no model or policy named %q", in.Policy)
		}
	}
	if in.Temperature != nil && (*in.Temperature < 0 || *in.Temperature > 2) {
		return invalidf("temperature must be between 0 and 2")
	}
	if in.MaxTokens != nil && (*in.MaxTokens < 1 || *in.MaxTokens > MaxTokensCap) {
		return invalidf("max tokens must be between 1 and %d", MaxTokensCap)
	}
	if len(in.Faults) > 0 && !s.faultInjection {
		return invalidf("fault injection is switched off (playground.fault_injection in the config)")
	}
	if len(in.Faults) > MaxFaults {
		return invalidf("at most %d faults", MaxFaults)
	}
	for _, f := range in.Faults {
		if !f.Kind.Valid() {
			return invalidf("unknown fault %q (use rate_limit, server_error, slow or cut_stream)", f.Kind)
		}
		if _, ok := cat.Providers[f.Provider]; !ok {
			return invalidf("fault names %q, which is not a provider in the catalog", f.Provider)
		}
		if f.Kind == faults.CutStream && !in.Stream {
			return invalidf("cutting a stream needs streaming switched on")
		}
	}
	return nil
}

// Run sends the prompt. A prompt that no provider could answer is a result, not an error: the route that failed is
// the interesting part. Errors are for requests refused before routing (bad input, budget, rate limit).
func (s *Service) Run(ctx context.Context, in Input) (*Output, error) {
	if err := s.validate(&in); err != nil {
		return nil, err
	}
	key, err := s.key(ctx)
	if err != nil {
		return nil, fmt.Errorf("playground key: %w", err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}

	req := &provider.ChatRequest{Model: in.Policy, Temperature: in.Temperature, Stream: in.Stream}
	max := DefaultMax
	if in.MaxTokens != nil {
		max = *in.MaxTokens
	}
	req.MaxTokens = &max
	if in.System = strings.TrimSpace(in.System); in.System != "" {
		req.Messages = append(req.Messages, provider.Message{Role: "system", Content: provider.TextContent(in.System)})
	}
	req.Messages = append(req.Messages, provider.Message{Role: "user", Content: provider.TextContent(in.Prompt)})
	if in.Stream {
		req.StreamOptions = &provider.StreamOptions{IncludeUsage: true}
	}

	out := &Output{ID: id, CreatedAt: s.now().UTC(), Policy: in.Policy, Stream: in.Stream, CostUSD: money.Micros(0).String(), SavedUSD: money.Micros(0).String(),
		Attempts: []usage.Attempt{}, Faults: orEmpty(in.Faults), UnusedFaults: []faults.Fault{}, Outcome: string(usage.OutcomeOK), Cache: string(usage.CacheBypass)}
	out.UnusedFaults = s.unusedFaults(in)

	cctx := gateway.WithFaults(ctx, in.Faults, s.settings)
	started := s.now()
	if in.Stream {
		err = s.runStream(cctx, key, id, req, out)
	} else {
		err = s.runChat(cctx, key, id, req, out)
	}
	out.LatencyMs = int(s.now().Sub(started).Milliseconds())
	if err != nil {
		var ge *gateway.Error
		if !errors.As(err, &ge) || ge.Status == 402 || ge.Status == 429 || ge.Status == 400 && len(ge.Attempts) == 0 {
			return nil, err // refused before any provider was asked
		}
		// Providers were asked and none answered: report the route that failed.
		out.Outcome = outcomeFor(ge)
		out.Error = &ErrorInfo{Code: ge.Code, Message: ge.Message}
		out.Attempts = orEmptyAttempts(ge.Attempts)
	}
	var spent time.Duration
	for _, a := range out.Attempts {
		if a.Kind != "skipped" {
			spent += time.Duration(a.LatencyMs) * time.Millisecond
		}
	}
	out.OverheadMs = max0(out.LatencyMs - int(spent.Milliseconds()))

	s.remember(in, out)
	return out, nil
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

func orEmpty(f []faults.Fault) []faults.Fault {
	if f == nil {
		return []faults.Fault{}
	}
	return f
}

func orEmptyAttempts(a []usage.Attempt) []usage.Attempt {
	if a == nil {
		return []usage.Attempt{}
	}
	return a
}

func outcomeFor(ge *gateway.Error) string {
	if ge.Code == "all_providers_failed" {
		return string(usage.OutcomeAllFailed)
	}
	return string(usage.OutcomeUpstreamError)
}

// unusedFaults are the faults that named a provider the policy would never call, so they had nothing to act on.
func (s *Service) unusedFaults(in Input) []faults.Fault {
	unused := []faults.Fault{}
	if len(in.Faults) == 0 {
		return unused
	}
	cands, err := s.gw.CandidateProviders(in.Policy)
	if err != nil {
		return unused
	}
	in_ := map[string]bool{}
	for _, c := range cands {
		in_[c] = true
	}
	for _, f := range in.Faults {
		if !in_[f.Provider] {
			unused = append(unused, f)
		}
	}
	return unused
}

func (s *Service) runChat(ctx context.Context, key keys.Key, id uuid.UUID, req *provider.ChatRequest, out *Output) error {
	res, err := s.gw.Chat(ctx, key, id, "", req)
	if err != nil {
		return err
	}
	ch := res.Response.Choices
	if len(ch) > 0 {
		out.Answer, out.FinishReason = ch[0].Message.Content.PlainText(), ch[0].FinishReason
	}
	out.Provider, out.Model, out.Cache = res.Provider, res.Model, string(res.Cache)
	out.CostUSD, out.SavedUSD = res.Cost.String(), res.Saved.String()
	if u := res.Response.Usage; u != nil {
		out.InputTokens, out.OutputTokens = u.PromptTokens, u.CompletionTokens
	}
	out.Attempts = orEmptyAttempts(res.Trace)
	return nil
}

func (s *Service) runStream(ctx context.Context, key keys.Key, id uuid.UUID, req *provider.ChatRequest, out *Output) error {
	st, err := s.gw.ChatStream(ctx, key, id, "", req)
	if err != nil {
		return err
	}
	out.Provider, out.Model, out.Cache = st.Provider(), st.Model(), string(st.Cache())
	var text strings.Builder
	var streamErr error
	for {
		c, err := st.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			streamErr = err
			break
		}
		for _, ch := range c.Choices {
			if ch.Delta.Content != nil {
				text.WriteString(*ch.Delta.Content)
			}
		}
	}
	attempts := st.Finish(ctx, streamErr)
	in, outTok, cost, finish := st.Totals()
	out.Answer, out.FinishReason = text.String(), finish
	out.InputTokens, out.OutputTokens, out.CostUSD = in, outTok, cost.String()
	out.Attempts = orEmptyAttempts(attempts)
	if streamErr != nil {
		// Bytes had been sent, so there is no failover: the answer stays partial and the cut is the story.
		out.Outcome = string(usage.OutcomeUpstreamError)
		out.FinishReason = "interrupted"
		out.Error = &ErrorInfo{Code: "stream_interrupted", Message: "The stream broke after part of the answer arrived: " + streamErr.Error()}
	}
	return nil
}

// remember saves the request so it shows in the history. A failure to save is logged, never shown to the person.
func (s *Service) remember(in Input, out *Output) {
	if s.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.Insert(ctx, in, out); err != nil {
		s.log.Error("could not save the playground request", "request_id", out.ID, "error", err)
	}
}

// --- history ---

type Store struct{ q *sqlcgen.Queries }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{q: sqlcgen.New(pool)} }

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

func (st *Store) Insert(ctx context.Context, in Input, out *Output) error {
	stored := *out
	stored.Answer = "" // kept in its own column
	result, err := json.Marshal(&stored)
	if err != nil {
		return err
	}
	fs, _ := json.Marshal(orEmpty(in.Faults))
	return st.q.InsertPlaygroundHistory(ctx, sqlcgen.InsertPlaygroundHistoryParams{
		ID: out.ID, Policy: in.Policy, Prompt: in.Prompt, System: in.System, Answer: clip(out.Answer, maxStored), Stream: in.Stream,
		Faults: fs, Result: result, CreatedAt: out.CreatedAt,
	})
}

// HistoryItem is a past request: what was asked, and what came back.
type HistoryItem struct {
	Prompt string `json:"prompt"`
	System string `json:"system"`
	Output
}

var ErrNotFound = errors.New("no such playground request")

func fromRow(id uuid.UUID, policy, prompt, system, answer string, stream bool, result []byte, at time.Time) (HistoryItem, error) {
	var out Output
	if err := json.Unmarshal(result, &out); err != nil {
		return HistoryItem{}, fmt.Errorf("history %s: %w", id, err)
	}
	out.ID, out.Policy, out.Answer, out.Stream, out.CreatedAt = id, policy, answer, stream, at.UTC()
	return HistoryItem{Prompt: prompt, System: system, Output: out}, nil
}

// List returns the newest requests first, a page at a time. next is empty on the last page.
func (st *Store) List(ctx context.Context, limit int, cursor string) (items []HistoryItem, next string, err error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	p := sqlcgen.ListPlaygroundHistoryParams{RowLimit: int32(limit + 1)}
	if cursor != "" {
		t, id, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		p.BeforeTs, p.BeforeID = &t, uuid.NullUUID{UUID: id, Valid: true}
	}
	rows, err := st.q.ListPlaygroundHistory(ctx, p)
	if err != nil {
		return nil, "", fmt.Errorf("list history: %w", err)
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	for _, r := range rows {
		it, err := fromRow(r.ID, r.Policy, r.Prompt, r.System, r.Answer, r.Stream, r.Result, r.CreatedAt)
		if err != nil {
			return nil, "", err
		}
		items = append(items, it)
	}
	if more && len(items) > 0 {
		last := items[len(items)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return items, next, nil
}

func (st *Store) Get(ctx context.Context, id uuid.UUID) (HistoryItem, error) {
	r, err := st.q.GetPlaygroundHistory(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return HistoryItem{}, ErrNotFound
	}
	if err != nil {
		return HistoryItem{}, fmt.Errorf("get history: %w", err)
	}
	return fromRow(r.ID, r.Policy, r.Prompt, r.System, r.Answer, r.Stream, r.Result, r.CreatedAt)
}

// SetFaultSettings changes how slow "slow" is and where a stream is cut; tests use it to stay fast.
func (s *Service) SetFaultSettings(f faults.Settings) { s.settings = f }

// History lists past requests newest first.
func (s *Service) History(ctx context.Context, limit int, cursor string) ([]HistoryItem, string, error) {
	return s.store.List(ctx, limit, cursor)
}
