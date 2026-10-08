package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/money"
	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/telemetry"
	"github.com/abdullah-9211/spillway/internal/usage"
)

// Observer receives what the gateway measures. telemetry.Metrics implements it.
type Observer interface {
	ObserveRequest(telemetry.RequestEvent)
	ObserveAttempt(provider, kind, errKind string)
	DependencyError(dep string)
}

type noopObserver struct{}

func (noopObserver) ObserveRequest(telemetry.RequestEvent) {}
func (noopObserver) ObserveAttempt(_, _, _ string)         {}
func (noopObserver) DependencyError(string)                {}

// Options turns on the features that need Redis, Postgres or Ollama. Every field is optional: a gateway with
// none of them still routes, retries and records usage. Redis-backed features (rate limit, exact cache) are
// simply absent without Redis.
type Options struct {
	Limiter    Limiter
	Spend      SpendReader // enables monthly budgets
	Exact      ExactCache
	Semantic   SemanticCache
	Embedder   provider.Embedder
	EmbedModel string
	Observer   Observer
	Tracer     trace.Tracer
}

// Gateway resolves a request into a plan, runs it through the executor and records usage.
type Gateway struct {
	cat      *Catalog
	exec     *Executor
	breakers *Breakers
	rec      usage.Recorder
	log      *slog.Logger
	now      func() time.Time
	planner  Planner

	limiter    Limiter
	budgets    *Budgets
	exact      ExactCache
	semantic   SemanticCache
	embedder   provider.Embedder
	embedModel string
	obs        Observer
	tracer     trace.Tracer

	fills   sync.WaitGroup
	fillSem chan struct{}
}

func New(cat *Catalog, providers map[string]provider.Provider, missing map[string]string, rec usage.Recorder, log *slog.Logger) *Gateway {
	br := NewBreakers(cat.Breaker, time.Now)
	g := &Gateway{
		cat: cat, breakers: br, rec: rec, log: log, now: time.Now,
		obs: noopObserver{}, tracer: otel.Tracer("spillway/gateway"), fillSem: make(chan struct{}, 16),
	}
	g.exec = NewExecutor(cat.Exec, providers, missing, br, log)
	g.exec.obs, g.exec.tracer = g.obs, g.tracer
	return g
}

// Use applies options. Call it before serving.
func (g *Gateway) Use(o Options) {
	g.limiter, g.exact, g.semantic, g.embedder, g.embedModel = o.Limiter, o.Exact, o.Semantic, o.Embedder, o.EmbedModel
	if o.Spend != nil {
		g.budgets = NewBudgets(o.Spend, 5*time.Second)
	}
	if o.Observer != nil {
		g.obs = o.Observer
	}
	if o.Tracer != nil {
		g.tracer = o.Tracer
	}
	g.exec.obs, g.exec.tracer = g.obs, g.tracer
}

func (g *Gateway) Catalog() *Catalog { return g.cat }

// BreakerStates reports each provider's breaker, for metrics and the dashboard.
func (g *Gateway) BreakerStates() map[string]BreakerState { return g.breakers.States() }

// WaitForFills blocks until background cache writes finish. Tests use it; shutdown uses it too.
func (g *Gateway) WaitForFills() { g.fills.Wait() }

// background runs f off the response path, dropping it when too many are already in flight.
func (g *Gateway) background(f func(ctx context.Context)) {
	select {
	case g.fillSem <- struct{}{}:
	default:
		return
	}
	g.fills.Add(1)
	go func() {
		defer g.fills.Done()
		defer func() { <-g.fillSem }()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		f(ctx)
	}()
}

// BuildProviders creates an adapter for every provider that has credentials. A provider whose API key
// variable is unset is skipped with a warning, so the service still starts and serves the others.
func BuildProviders(cat *Catalog, getenv func(string) string, log *slog.Logger) (map[string]provider.Provider, map[string]string, error) {
	out := map[string]provider.Provider{}
	missing := map[string]string{}
	used := map[string]bool{}
	for _, m := range cat.Models {
		used[m.Provider] = true
	}
	for name, pc := range cat.Providers {
		if !used[name] {
			continue
		}
		key := ""
		if pc.APIKeyEnv != "" {
			if key = getenv(pc.APIKeyEnv); key == "" && pc.BaseURL == "" {
				missing[name] = fmt.Sprintf("%s is not set", pc.APIKeyEnv)
				log.Warn("provider disabled: API key variable is not set", "provider", name, "env", pc.APIKeyEnv)
				continue
			}
		}
		p, err := provider.New(pc.Adapter, provider.Config{Name: name, APIKey: key, BaseURL: pc.BaseURL})
		if err != nil {
			return nil, nil, fmt.Errorf("provider %q: %w", name, err)
		}
		out[name] = p
	}
	return out, missing, nil
}

// ValidateRequest checks what the gateway can check without calling anyone.
func ValidateRequest(req *provider.ChatRequest) error {
	if req.Model == "" {
		return invalid("model", "model is required")
	}
	if len(req.Messages) == 0 {
		return invalid("messages", "messages must contain at least one message")
	}
	for i, m := range req.Messages {
		switch m.Role {
		case "system", "developer", "user", "assistant", "tool":
		default:
			return invalid(fmt.Sprintf("messages[%d].role", i), "unsupported role %q", m.Role)
		}
		if m.Role == "tool" && m.ToolCallID == "" {
			return invalid(fmt.Sprintf("messages[%d].tool_call_id", i), "tool messages need a tool_call_id")
		}
	}
	if req.N != nil && *req.N != 1 {
		return invalid("n", "only n=1 is supported")
	}
	if req.Temperature != nil && (*req.Temperature < 0 || *req.Temperature > 2) {
		return invalid("temperature", "temperature must be between 0 and 2")
	}
	for i, t := range req.Tools {
		if t.Type != "function" || t.Function.Name == "" {
			return invalid(fmt.Sprintf("tools[%d]", i), "only function tools with a name are supported")
		}
	}
	return nil
}

// Result is a finished non-streaming call.
type Result struct {
	Response *provider.ChatResponse
	Provider string
	Model    string // catalog model id that answered
	Cost     money.Micros
	Saved    money.Micros // what a cache hit avoided
	Attempts int          // provider calls made, not counting skipped candidates
	Cache    usage.CacheStatus
}

// call is the state of one request as it moves through the pipeline.
type call struct {
	g       *Gateway
	key     keys.Key
	id      uuid.UUID
	plan    Plan
	req     *provider.ChatRequest
	model   Model // the model that answered; before that, the first candidate
	started time.Time
	span    trace.Span

	cacheStatus usage.CacheStatus // miss or bypass until a hit
	cacheKey    string            // exact-cache key, when the request was looked up
	embedding   []float32         // from the semantic lookup, reused to fill
	group       string
}

func (g *Gateway) begin(ctx context.Context, key keys.Key, id uuid.UUID, bucket string, req *provider.ChatRequest) (context.Context, *call, error) {
	if err := ValidateRequest(req); err != nil {
		return ctx, nil, err
	}
	plan, err := g.cat.Plan(req.Model, req, g.breakers, bucket, g.planner)
	if err != nil {
		return ctx, nil, err
	}
	ctx, span := g.tracer.Start(ctx, "gateway.request", trace.WithAttributes(
		attribute.String("spillway.request_id", id.String()),
		attribute.String("spillway.policy", plan.Name),
		attribute.String("spillway.key_prefix", key.Prefix),
		attribute.Bool("spillway.stream", req.Stream),
	))
	return ctx, &call{g: g, key: key, id: id, plan: plan, req: req, model: plan.Candidates[0], started: g.now(),
		span: span, cacheStatus: usage.CacheBypass}, nil
}

func countAttempts(as []usage.Attempt) int {
	n := 0
	for _, a := range as {
		if a.Kind != "skipped" {
			n++
		}
	}
	return n
}

// finished is everything record needs to know about how a request ended.
type finished struct {
	outcome  usage.Outcome
	in, out  int
	cost     *money.Micros // nil: priced from the tokens and the model's prices
	saved    money.Micros
	cache    usage.CacheStatus
	ttfb     *int
	attempts []usage.Attempt
	overhead *time.Duration // nil: derived from the attempts when there was a single one
}

func (c *call) record(f finished) money.Micros {
	g := c.g
	cost := money.TokenCost(f.in, c.model.InputPerMtok, f.out, c.model.OutputPerMtok)
	if f.cost != nil {
		cost = *f.cost
	}
	if f.cache == "" {
		f.cache = c.cacheStatus
	}
	latency := g.now().Sub(c.started)
	g.rec.Record(usage.Row{
		ID: c.id, KeyID: c.key.ID, Policy: c.plan.Name, Provider: c.model.Provider, Model: c.model.ID,
		InputTokens: f.in, OutputTokens: f.out, Cost: cost, Saved: f.saved, LatencyMs: int(latency.Milliseconds()),
		TTFBMs: f.ttfb, CacheStatus: f.cache, Outcome: f.outcome, Attempts: f.attempts, CreatedAt: c.started.UTC(),
	})
	if g.budgets != nil {
		g.budgets.Charge(c.key.ID, cost)
	}

	ev := telemetry.RequestEvent{
		Policy: c.plan.Name, Provider: c.model.Provider, Model: c.model.ID, Outcome: string(f.outcome), Cache: string(f.cache),
		Key: c.key.Prefix, Latency: latency, CostUSD: float64(cost) / 1e6, SavedUSD: float64(f.saved) / 1e6,
	}
	switch {
	case f.overhead != nil:
		ev.Overhead, ev.HasOverhead = *f.overhead, true
	case countAttempts(f.attempts) <= 1: // with retries or fallbacks, backoff waits would be counted as overhead
		var upstream time.Duration
		for _, a := range f.attempts {
			upstream += time.Duration(a.LatencyMs) * time.Millisecond
		}
		ev.Overhead, ev.HasOverhead = max(latency-upstream, 0), true
	}
	g.obs.ObserveRequest(ev)

	c.span.SetAttributes(attribute.String("spillway.outcome", string(f.outcome)), attribute.String("spillway.cache", string(f.cache)),
		attribute.String("spillway.provider", c.model.Provider), attribute.String("spillway.model", c.model.ID),
		attribute.Int("spillway.attempts", countAttempts(f.attempts)))
	if f.outcome != usage.OutcomeOK {
		c.span.SetStatus(codes.Error, string(f.outcome))
	}
	c.span.End()
	return cost
}

// admit applies the key's rate limit and budget. Both fail open if their backing store is unreachable: a
// monitoring or Redis outage must not take the gateway down.
func (c *call) admit(ctx context.Context) error {
	g := c.g
	if rpm := c.key.RateLimitRPM; rpm != nil && g.limiter != nil {
		ok, retry, err := g.limiter.Allow(ctx, c.key.ID, *rpm)
		switch {
		case err != nil:
			g.obs.DependencyError("redis")
			g.log.Warn("rate limiter unavailable, allowing the request", "error", err)
		case !ok:
			c.record(finished{outcome: usage.OutcomeRateLimited})
			secs := max(int(math.Ceil(retry.Seconds())), 1)
			return &Error{Status: 429, Type: "rate_limit_error", Code: "rate_limit_exceeded", RetryAfter: time.Duration(secs) * time.Second,
				Message: fmt.Sprintf("Rate limit of %d requests per minute reached. Retry in %ds.", *rpm, secs)}
		}
	}
	if b := c.key.MonthlyBudget; b != nil && g.budgets != nil {
		exceeded, spend, err := g.budgets.Exceeded(ctx, c.key.ID, *b)
		switch {
		case err != nil:
			g.obs.DependencyError("postgres")
			g.log.Warn("budget check unavailable, allowing the request", "error", err)
		case exceeded:
			c.record(finished{outcome: usage.OutcomeOverBudget})
			return &Error{Status: 402, Type: "insufficient_quota", Code: "budget_exceeded",
				Message: fmt.Sprintf("Monthly budget of $%s is used up (spent $%s). It resets on the first of the month, UTC.", *b, spend)}
		}
	}
	return nil
}

// lookup tries the exact cache, then the semantic cache. It returns a hit and how it matched, or nil. It
// also leaves c.cacheStatus at miss (looked up, not found) or bypass (not eligible or no cache), and keeps
// the key and embedding so a miss can fill the caches afterwards.
func (c *call) lookup(ctx context.Context) (*Entry, usage.CacheStatus) {
	g := c.g
	if !Cacheable(c.req, c.key) {
		return nil, ""
	}
	if g.exact != nil {
		k, err := CacheKey(scopeFor(g.cat.Cache.Global, c.key.ID), c.req)
		if err == nil {
			c.cacheKey = k
			e, err := g.exact.Get(ctx, k)
			switch {
			case err != nil:
				g.obs.DependencyError("redis")
				g.log.Warn("exact cache unavailable", "error", err)
				c.cacheKey = ""
			case e != nil:
				return e, usage.CacheHitExact
			default:
				c.cacheStatus = usage.CacheMiss
			}
		}
	}
	if c.key.SemanticCache && g.semantic != nil && g.embedder != nil {
		if text, ok := SemanticText(c.req); ok {
			ectx, cancel := context.WithTimeout(ctx, 2*time.Second)
			vecs, err := g.embedder.Embed(ectx, g.embedModel, []string{text})
			cancel()
			if err != nil || len(vecs) != 1 {
				g.obs.DependencyError("embedder")
				g.log.Warn("embedding unavailable, skipping the semantic cache", "error", err)
				return nil, ""
			}
			c.embedding, c.group = vecs[0], SemanticGroup(c.req)
			e, _, err := g.semantic.Lookup(ctx, g.semanticScope(c.key.ID), c.group, c.embedding, g.cat.Cache.Threshold)
			if err != nil {
				g.obs.DependencyError("postgres")
				g.log.Warn("semantic cache unavailable", "error", err)
				c.embedding = nil
				return nil, ""
			}
			c.cacheStatus = usage.CacheMiss
			if e != nil {
				return e, usage.CacheHitSemantic
			}
		}
	}
	return nil, ""
}

func (g *Gateway) semanticScope(id uuid.UUID) uuid.UUID {
	if g.cat.Cache.Global {
		return uuid.Nil
	}
	return id
}

// fill stores a finished answer in whichever caches the request was looked up in. It runs in the background.
func (c *call) fill(resp provider.ChatResponse, cost money.Micros) {
	g := c.g
	if !StoreWorthy(&resp) || (c.cacheKey == "" && c.embedding == nil) {
		return
	}
	e := &Entry{Response: resp, Provider: c.model.Provider, Model: c.model.ID, Cost: cost}
	ttl := g.cat.Cache.TTL
	key, emb, group, scope := c.cacheKey, c.embedding, c.group, g.semanticScope(c.key.ID)
	if key != "" && g.exact != nil {
		g.background(func(ctx context.Context) {
			if err := g.exact.Set(ctx, key, e, ttl); err != nil {
				g.obs.DependencyError("redis")
				g.log.Warn("cache fill failed", "error", err)
			}
		})
	}
	if emb != nil && g.semantic != nil {
		g.background(func(ctx context.Context) {
			if err := g.semantic.Store(ctx, scope, group, emb, e, ttl); err != nil {
				g.obs.DependencyError("postgres")
				g.log.Warn("semantic cache fill failed", "error", err)
			}
		})
	}
}

// useHit points the call at the model that produced a cached answer, so usage and headers name it.
func (c *call) useHit(e *Entry) {
	if m, ok := c.g.cat.Models[e.Model]; ok {
		c.model = m
		return
	}
	c.model = Model{ID: e.Model, Provider: e.Provider}
}

// fail records a request no candidate could answer and returns the API error for it.
func (c *call) fail(err error) error {
	var ee *ExecError
	if !errors.As(err, &ee) {
		c.span.RecordError(err)
		c.span.End()
		return upstream("%v", err)
	}
	// Attribute the failure to the last model actually tried, else the first candidate.
	for i := len(ee.Attempts) - 1; i >= 0; i-- {
		if m, ok := c.g.cat.Models[ee.Attempts[i].Model]; ok && ee.Attempts[i].Kind != "skipped" {
			c.model = m
			break
		}
	}
	outcome := usage.OutcomeUpstreamError
	switch {
	case ee.Canceled:
		outcome = usage.OutcomeClientCancelled
	case !ee.BadRequest && (ee.Candidates > 1 || ee.Deadline):
		outcome = usage.OutcomeAllFailed
	}
	c.record(finished{outcome: outcome, attempts: ee.Attempts})

	switch {
	case ee.Canceled:
		return &Error{Status: 499, Type: "api_error", Code: "client_closed_request", Message: "request cancelled"}
	case ee.BadRequest:
		var pe *provider.ProviderError
		msg := ee.Error()
		if errors.As(ee.Err, &pe) {
			msg = pe.Err.Error()
		}
		return &Error{Status: 400, Type: "invalid_request_error", Code: "invalid_request",
			Message: fmt.Sprintf("%s rejected the request: %s", c.model.Provider, msg)}
	case ee.Candidates > 1 || ee.Deadline:
		return &Error{Status: 503, Type: "api_error", Code: "all_providers_failed", Attempts: ee.Attempts,
			Message: "Every provider failed or was unavailable: " + lastReason(ee)}
	}
	return &Error{Status: 502, Type: "api_error", Code: "upstream_error", Attempts: ee.Attempts,
		Message: fmt.Sprintf("%s call failed: %s", c.model.Provider, lastReason(ee))}
}

func lastReason(ee *ExecError) string {
	var pe *provider.ProviderError
	if errors.As(ee.Err, &pe) {
		return fmt.Sprintf("%s (%s)", pe.Err, pe.Kind)
	}
	return ee.Error()
}

// Chat makes a non-streaming call.
func (g *Gateway) Chat(ctx context.Context, key keys.Key, id uuid.UUID, bucket string, req *provider.ChatRequest) (*Result, error) {
	ctx, c, err := g.begin(ctx, key, id, bucket, req)
	if err != nil {
		return nil, err
	}
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	if e, status := c.lookup(ctx); e != nil {
		c.useHit(e)
		resp := e.Response
		resp.ID, resp.Object, resp.Created, resp.Model = "chatcmpl-"+id.String(), "chat.completion", g.now().Unix(), e.Model
		zero := money.Micros(0)
		c.record(finished{outcome: usage.OutcomeOK, cost: &zero, saved: e.Cost, cache: status})
		return &Result{Response: &resp, Provider: e.Provider, Model: e.Model, Saved: e.Cost, Cache: status}, nil
	}

	t0 := g.now()
	res, err := g.exec.run(ctx, c.plan, g.exec.tryChat(req))
	if err != nil {
		return nil, c.fail(err)
	}
	took := g.now().Sub(t0)
	resp := res.Value.(*provider.ChatResponse)
	c.model = res.Winner

	in, out, estimated := tokens(resp.Usage, req, responseText(resp))
	if estimated {
		res.Attempts[winnerIndex(res.Attempts)].Estimated = true
	}
	ttfb := int(took.Milliseconds())
	cost := c.record(finished{outcome: usage.OutcomeOK, in: in, out: out, ttfb: &ttfb, attempts: res.Attempts})

	resp.Model = c.model.ID // clients see the id they asked about; the upstream id is in usage
	resp.Object = "chat.completion"
	if resp.Created == 0 {
		resp.Created = g.now().Unix()
	}
	if resp.ID == "" {
		resp.ID = "chatcmpl-" + id.String()
	}
	if resp.Usage == nil || estimated {
		resp.Usage = &provider.Usage{PromptTokens: in, CompletionTokens: out, TotalTokens: in + out}
	}
	c.fill(*resp, cost)
	return &Result{Response: resp, Provider: c.model.Provider, Model: c.model.ID, Cost: cost,
		Attempts: countAttempts(res.Attempts), Cache: c.cacheStatus}, nil
}

// winnerIndex is the last attempt that did not fail: the one that produced the answer.
func winnerIndex(as []usage.Attempt) int {
	for i := len(as) - 1; i >= 0; i-- {
		if as[i].Error == "" && as[i].Kind != "skipped" {
			return i
		}
	}
	return len(as) - 1
}

// --- token estimation, used only when a provider reports no usage ---

func estimate(chars int) int { return (chars + 3) / 4 }

func requestChars(req *provider.ChatRequest) int {
	n := 0
	for _, m := range req.Messages {
		n += len(m.Content.PlainText())
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Arguments) + len(tc.Function.Name)
		}
	}
	return n
}

func responseText(r *provider.ChatResponse) string {
	var b strings.Builder
	for _, c := range r.Choices {
		b.WriteString(c.Message.Content.PlainText())
		for _, tc := range c.Message.ToolCalls {
			b.WriteString(tc.Function.Arguments)
		}
	}
	return b.String()
}

// tokens returns reported usage, or an estimate (flagged) when the provider reported none.
func tokens(u *provider.Usage, req *provider.ChatRequest, out string) (in, outTok int, estimated bool) {
	if u != nil {
		return u.PromptTokens, u.CompletionTokens, false
	}
	return estimate(requestChars(req)), estimate(len(out)), true
}
