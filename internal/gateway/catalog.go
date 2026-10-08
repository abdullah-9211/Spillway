// Package gateway turns a chat request into a provider call: it resolves the model or policy from the
// catalog, calls the provider, and records usage.
package gateway

import (
	"fmt"
	"os"
	"sort"
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
	Providers map[string]providerConfig `yaml:"providers"`
	Models    []modelConfig             `yaml:"models"`
	Policies  []policyConfig            `yaml:"policies"`
	Gateway   gatewayConfig             `yaml:"gateway"`
	Breaker   breakerConfig             `yaml:"breaker"`
	Cache     cacheConfig               `yaml:"cache"`
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
	Providers map[string]Provider
	Models    map[string]Model
	Policies  map[string]Policy
	Exec      ExecConfig
	Breaker   BreakerConfig
	Cache     CacheSettings
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
