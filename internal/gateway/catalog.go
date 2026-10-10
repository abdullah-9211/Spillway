// Package gateway turns a chat request into a provider call: it resolves the model or policy from the
// catalog, calls the provider, and records usage.
package gateway

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/abdullah-9211/spillway/internal/money"
)

// usdPerMtok reads a price such as 3.00 from YAML straight into micro-dollars, never as a float.
type usdPerMtok money.Micros

func (u *usdPerMtok) UnmarshalYAML(n *yaml.Node) error {
	m, err := money.ParseUSD(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*u = usdPerMtok(m)
	return nil
}

type fileConfig struct {
	Providers  map[string]providerConfig `yaml:"providers"`
	Models     []modelConfig             `yaml:"models"`
	Policies   []policyConfig            `yaml:"policies"`
	Gateway    gatewayConfig             `yaml:"gateway"`
	Breaker    breakerConfig             `yaml:"breaker"`
	Cache      cacheConfig               `yaml:"cache"`
	Playground playgroundConfig          `yaml:"playground"`
	Runs       runsConfig                `yaml:"runs"`
}

type providerConfig struct {
	Type       string `yaml:"type"` // adapter name; defaults to the provider's own name
	APIKeyEnv  string `yaml:"api_key_env"`
	BaseURL    string `yaml:"base_url"`
	EmbedModel string `yaml:"embed_model"` // ollama: the model that embeds for the semantic cache
}

type modelConfig struct {
	ID            string     `yaml:"id"`
	Provider      string     `yaml:"provider"`
	Upstream      string     `yaml:"upstream"`
	InputPerMtok  usdPerMtok `yaml:"input_usd_per_mtok"`
	OutputPerMtok usdPerMtok `yaml:"output_usd_per_mtok"`
	ContextWindow int        `yaml:"context_window"`
	Tags          []string   `yaml:"tags"`
}

type policyConfig struct {
	Name     string   `yaml:"name"`
	Type     string   `yaml:"type"`
	Model    string   `yaml:"model"`    // fixed
	Fallback []string `yaml:"fallback"` // fixed: optional models to try if the first fails
	Models   []string `yaml:"models"`   // fallback
	Tag      string   `yaml:"tag"`      // cheapest
	Arms     []struct {
		Model  string `yaml:"model"`
		Weight int    `yaml:"weight"`
	} `yaml:"arms"` // weighted
	HedgeAfterMs int `yaml:"hedge_after_ms"`
}

type gatewayConfig struct {
	RequestTimeout   time.Duration `yaml:"request_timeout"`
	FirstByteTimeout time.Duration `yaml:"first_byte_timeout"`
	IdleTimeout      time.Duration `yaml:"idle_timeout"`
	MaxTotalMs       int           `yaml:"max_total_ms"`
	MaxRetries       *int          `yaml:"max_retries"`
}

type playgroundConfig struct {
	Key              string     `yaml:"key"`
	MonthlyBudgetUSD usdPerMtok `yaml:"monthly_budget_usd"` // read as a dollar amount; the type is shared with prices
	RateLimitRPM     int        `yaml:"rate_limit_rpm"`
	FaultInjection   *bool      `yaml:"fault_injection"`
}

type runsConfig struct {
	LeaseTTL        time.Duration `yaml:"lease_ttl"`
	Heartbeat       time.Duration `yaml:"heartbeat"`
	Workers         int           `yaml:"workers"`
	MaxSteps        int           `yaml:"max_steps"`
	MaxCostUSD      usdPerMtok    `yaml:"max_cost_usd"` // read as a dollar amount; the type is shared with prices
	Deadline        time.Duration `yaml:"deadline"`
	ToolErrorBudget int           `yaml:"tool_error_budget"`
	// CompactionPolicy is the policy or model that writes compaction summaries; empty means the run's own.
	CompactionPolicy string `yaml:"compaction_policy"`
}

// RunSettings are the run engine's timing and the caps no request can exceed.
type RunSettings struct {
	LeaseTTL         time.Duration
	Heartbeat        time.Duration
	Workers          int
	MaxSteps         int
	MaxCost          money.Micros
	Deadline         time.Duration
	ToolErrorBudget  int
	CompactionPolicy string
}

func (r runsConfig) settings() (RunSettings, error) {
	s := RunSettings{LeaseTTL: r.LeaseTTL, Heartbeat: r.Heartbeat, Workers: r.Workers, MaxSteps: r.MaxSteps,
		MaxCost: money.Micros(r.MaxCostUSD), Deadline: r.Deadline, ToolErrorBudget: r.ToolErrorBudget, CompactionPolicy: r.CompactionPolicy}
	if s.LeaseTTL <= 0 {
		s.LeaseTTL = 30 * time.Second
	}
	if s.Heartbeat <= 0 {
		s.Heartbeat = 10 * time.Second
	}
	if s.Workers <= 0 {
		s.Workers = 8
	}
	if s.MaxSteps <= 0 {
		s.MaxSteps = 50
	}
	if s.MaxCost <= 0 {
		s.MaxCost = 1_000_000
	}
	if s.Deadline <= 0 {
		s.Deadline = 15 * time.Minute
	}
	if s.ToolErrorBudget <= 0 {
		s.ToolErrorBudget = 3
	}
	if s.Heartbeat*2 > s.LeaseTTL {
		return s, fmt.Errorf("runs.heartbeat (%s) must be at most half of runs.lease_ttl (%s), or a healthy worker could lose its lease", s.Heartbeat, s.LeaseTTL)
	}
	return s, nil
}

type cacheConfig struct {
	ExactTTL          time.Duration `yaml:"exact_ttl"`
	Scope             string        `yaml:"scope"` // key (default) or global
	SemanticThreshold float64       `yaml:"semantic_threshold"`
}

type breakerConfig struct {
	ConsecutiveFailures int           `yaml:"consecutive_failures"`
	ErrorRate           float64       `yaml:"error_rate"`
	Window              int           `yaml:"window"`
	OpenFor             time.Duration `yaml:"open_for"`
}

type Provider struct {
	Name       string
	Adapter    string
	APIKeyEnv  string
	BaseURL    string
	EmbedModel string
}

// PlaygroundSettings configure the dashboard's playground and its built-in key.
type PlaygroundSettings struct {
	Budget         money.Micros
	RateLimitRPM   int
	FaultInjection bool
}

// CacheSettings configure both caches.
type CacheSettings struct {
	TTL       time.Duration
	Global    bool    // share entries between keys; the default is one cache per key
	Threshold float64 // semantic similarity needed for a hit
}

type Model struct {
	ID            string
	Provider      string
	Upstream      string
	InputPerMtok  money.Micros
	OutputPerMtok money.Micros
	ContextWindow int
	Tags          []string
}

type Arm struct {
	Model  string
	Weight int
}

type Policy struct {
	Name       string
	Type       string
	Models     []string // fixed: the model then its fallbacks; fallback: the ordered chain
	Tag        string   // cheapest
	Arms       []Arm    // weighted
	HedgeAfter time.Duration
}

// Catalog is the validated, immutable view of the model catalog file.
type Catalog struct {
	Providers  map[string]Provider
	Models     map[string]Model
	Policies   map[string]Policy
	Exec       ExecConfig
	Breaker    BreakerConfig
	Cache      CacheSettings
	Playground PlaygroundSettings
	Runs       RunSettings
}

var policyTypes = map[string]bool{"fixed": true, "fallback": true, "cheapest": true, "weighted": true}

func LoadCatalog(path string) (*Catalog, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read catalog: %w", err)
	}
	return ParseCatalog(raw)
}

func ParseCatalog(raw []byte) (*Catalog, error) {
	var f fileConfig
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse catalog: %w", err)
	}
	c := &Catalog{
		Providers: map[string]Provider{}, Models: map[string]Model{}, Policies: map[string]Policy{},
		Exec:    execConfigFrom(f.Gateway),
		Breaker: BreakerConfig(f.Breaker).withDefaults(),
		Cache:   CacheSettings{TTL: f.Cache.ExactTTL, Threshold: f.Cache.SemanticThreshold, Global: f.Cache.Scope == "global"},
	}
	c.Playground = PlaygroundSettings{Budget: money.Micros(f.Playground.MonthlyBudgetUSD), RateLimitRPM: f.Playground.RateLimitRPM, FaultInjection: true}
	if f.Playground.MonthlyBudgetUSD == 0 {
		c.Playground.Budget = 5_000_000 // $5 a month unless the file says otherwise
	}
	if c.Playground.RateLimitRPM <= 0 {
		c.Playground.RateLimitRPM = 20
	}
	if f.Playground.FaultInjection != nil {
		c.Playground.FaultInjection = *f.Playground.FaultInjection
	}
	rs, err := f.Runs.settings()
	if err != nil {
		return nil, err
	}
	c.Runs = rs
	if c.Cache.TTL <= 0 {
		c.Cache.TTL = 24 * time.Hour
	}
	if c.Cache.Threshold <= 0 {
		c.Cache.Threshold = 0.95
	}
	if f.Cache.Scope != "" && f.Cache.Scope != "key" && f.Cache.Scope != "global" {
		return nil, fmt.Errorf("cache.scope must be key or global, not %q", f.Cache.Scope)
	}
	if c.Cache.Threshold > 1 {
		return nil, fmt.Errorf("cache.semantic_threshold must be at most 1")
	}

	for name, p := range f.Providers {
		adapter := p.Type
		if adapter == "" {
			adapter = name
		}
		c.Providers[name] = Provider{Name: name, Adapter: adapter, APIKeyEnv: p.APIKeyEnv, BaseURL: p.BaseURL, EmbedModel: p.EmbedModel}
	}

	for i, m := range f.Models {
		switch {
		case m.ID == "":
			return nil, fmt.Errorf("models[%d]: id is required", i)
		case m.Upstream == "":
			return nil, fmt.Errorf("model %q: upstream is required", m.ID)
		case m.ContextWindow < 0:
			return nil, fmt.Errorf("model %q: context_window must not be negative", m.ID)
		case m.InputPerMtok < 0 || m.OutputPerMtok < 0:
			return nil, fmt.Errorf("model %q: prices must not be negative", m.ID)
		}
		if _, ok := c.Providers[m.Provider]; !ok {
			return nil, fmt.Errorf("model %q: provider %q is not declared under providers", m.ID, m.Provider)
		}
		if _, dup := c.Models[m.ID]; dup {
			return nil, fmt.Errorf("model %q is listed twice", m.ID)
		}
		c.Models[m.ID] = Model{
			ID: m.ID, Provider: m.Provider, Upstream: m.Upstream,
			InputPerMtok: money.Micros(m.InputPerMtok), OutputPerMtok: money.Micros(m.OutputPerMtok),
			ContextWindow: m.ContextWindow, Tags: m.Tags,
		}
	}

	for i, p := range f.Policies {
		switch {
		case p.Name == "":
			return nil, fmt.Errorf("policies[%d]: name is required", i)
		case !policyTypes[p.Type]:
			return nil, fmt.Errorf("policy %q: unknown type %q", p.Name, p.Type)
		}
		if _, dup := c.Policies[p.Name]; dup {
			return nil, fmt.Errorf("policy %q is listed twice", p.Name)
		}
		if _, clash := c.Models[p.Name]; clash {
			return nil, fmt.Errorf("policy %q has the same name as a model", p.Name)
		}
		pol := Policy{Name: p.Name, Type: p.Type, HedgeAfter: time.Duration(p.HedgeAfterMs) * time.Millisecond}
		var refs []string
		switch p.Type {
		case "fixed":
			if p.Model == "" {
				return nil, fmt.Errorf("policy %q: fixed policies need a model", p.Name)
			}
			pol.Models = append([]string{p.Model}, p.Fallback...)
			refs = pol.Models
		case "fallback":
			if len(p.Models) == 0 {
				return nil, fmt.Errorf("policy %q: fallback policies need models", p.Name)
			}
			pol.Models = p.Models
			refs = p.Models
		case "weighted":
			if len(p.Arms) == 0 {
				return nil, fmt.Errorf("policy %q: weighted policies need arms", p.Name)
			}
			for _, a := range p.Arms {
				if a.Weight <= 0 {
					return nil, fmt.Errorf("policy %q: arm %q needs a positive weight", p.Name, a.Model)
				}
				pol.Arms = append(pol.Arms, Arm{Model: a.Model, Weight: a.Weight})
				refs = append(refs, a.Model)
			}
		case "cheapest":
			if p.Tag == "" {
				return nil, fmt.Errorf("policy %q: cheapest policies need a tag", p.Name)
			}
			pol.Tag = p.Tag
		}
		if p.HedgeAfterMs < 0 {
			return nil, fmt.Errorf("policy %q: hedge_after_ms must not be negative", p.Name)
		}
		for _, r := range refs {
			if _, ok := c.Models[r]; !ok {
				return nil, fmt.Errorf("policy %q refers to unknown model %q", p.Name, r)
			}
		}
		if p.Type == "cheapest" && len(c.modelsWithTag(p.Tag)) == 0 {
			return nil, fmt.Errorf("policy %q: no model has the tag %q", p.Name, p.Tag)
		}
		c.Policies[p.Name] = pol
	}
	return c, nil
}

// Has reports whether name is a model id or a policy name, the two things a request's model field may be.
func (c *Catalog) Has(name string) bool {
	_, m := c.Models[name]
	_, p := c.Policies[name]
	return m || p
}

// Names lists model ids and policy names, sorted.
func (c *Catalog) Names() (models, policies []string) {
	for id := range c.Models {
		models = append(models, id)
	}
	for n := range c.Policies {
		policies = append(policies, n)
	}
	sort.Strings(models)
	sort.Strings(policies)
	return
}

// --- what the dashboard shows about the catalog ---

type ModelInfo struct {
	ID            string
	Provider      string
	Upstream      string
	Tags          []string
	ContextWindow int
	InputPerMtok  money.Micros
	OutputPerMtok money.Micros
}

type PolicyInfo struct {
	Name         string
	Type         string
	Models       []ModelInfo // in the order they are tried; for weighted, the arms
	Weights      []int       // weighted only, parallel to Models
	Tag          string      // cheapest only
	HedgeAfterMs int
	Description  string
}

// Describe lists the models and policies in a stable order, with a plain sentence for each policy.
func (c *Catalog) Describe() (models []ModelInfo, policies []PolicyInfo) {
	ids, names := c.Names()
	info := func(m Model) ModelInfo {
		return ModelInfo{ID: m.ID, Provider: m.Provider, Upstream: m.Upstream, Tags: m.Tags, ContextWindow: m.ContextWindow, InputPerMtok: m.InputPerMtok, OutputPerMtok: m.OutputPerMtok}
	}
	for _, id := range ids {
		models = append(models, info(c.Models[id]))
	}
	join := func(ms []ModelInfo) string {
		out := make([]string, len(ms))
		for i, m := range ms {
			out[i] = m.ID
		}
		return strings.Join(out, ", ")
	}
	for _, n := range names {
		p := c.Policies[n]
		pi := PolicyInfo{Name: n, Type: p.Type, Tag: p.Tag, HedgeAfterMs: int(p.HedgeAfter.Milliseconds())}
		switch p.Type {
		case "fixed", "fallback":
			for _, id := range p.Models {
				pi.Models = append(pi.Models, info(c.Models[id]))
			}
		case "weighted":
			for _, a := range p.Arms {
				pi.Models = append(pi.Models, info(c.Models[a.Model]))
				pi.Weights = append(pi.Weights, a.Weight)
			}
		case "cheapest":
			tagged := c.modelsWithTag(p.Tag)
			sort.SliceStable(tagged, func(i, j int) bool {
				ci, cj := tagged[i].InputPerMtok+tagged[i].OutputPerMtok, tagged[j].InputPerMtok+tagged[j].OutputPerMtok
				return ci < cj
			})
			for _, m := range tagged {
				pi.Models = append(pi.Models, info(m))
			}
		}
		switch {
		case p.Type == "fixed" && len(pi.Models) == 1:
			pi.Description = "Always " + pi.Models[0].ID
		case p.Type == "fixed":
			pi.Description = "Starts with " + pi.Models[0].ID + ", then " + join(pi.Models[1:])
		case p.Type == "fallback" && p.HedgeAfter > 0 && len(pi.Models) > 1:
			pi.Description = fmt.Sprintf("%s first; %s joins after %s", pi.Models[0].ID, pi.Models[1].ID, p.HedgeAfter.Round(time.Millisecond*100))
		case p.Type == "fallback":
			pi.Description = "Fallback order: " + join(pi.Models)
		case p.Type == "cheapest":
			pi.Description = "Cheapest model tagged " + p.Tag
		case p.Type == "weighted":
			total := 0
			for _, w := range pi.Weights {
				total += w
			}
			parts := make([]string, len(pi.Models))
			for i, m := range pi.Models {
				parts[i] = fmt.Sprintf("%s %d%%", m.ID, pi.Weights[i]*100/total)
			}
			pi.Description = "Split: " + strings.Join(parts, ", ")
		}
		policies = append(policies, pi)
	}
	return models, policies
}
