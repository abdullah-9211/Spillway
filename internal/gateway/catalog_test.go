package gateway

import (
	"strings"
	"testing"
	"time"

	"github.com/abdullah-9211/spillway/internal/money"
)

const goodCatalog = `
providers:
  anthropic: { api_key_env: ANTHROPIC_API_KEY }
  local:     { type: openai, base_url: "http://localhost:9999/v1" }
models:
  - { id: sonnet, provider: anthropic, upstream: claude-x, input_usd_per_mtok: 3.00, output_usd_per_mtok: 15, context_window: 200000, tags: [reasoning] }
  - { id: fake,   provider: local,     upstream: fake-1,   input_usd_per_mtok: 0.075, output_usd_per_mtok: 0, tags: [fast] }
policies:
  - { name: default, type: fixed, model: sonnet }
  - { name: chain,   type: fallback, models: [sonnet, fake] }
  - { name: cheap,   type: cheapest, tag: fast }
  - { name: ab,      type: weighted, arms: [{model: sonnet, weight: 1}, {model: fake, weight: 1}] }
gateway: { request_timeout: 30s }
`

func TestParseCatalog(t *testing.T) {
	c, err := ParseCatalog([]byte(goodCatalog))
	if err != nil {
		t.Fatal(err)
	}
	if m := c.Models["sonnet"]; m.InputPerMtok != 3_000_000 || m.OutputPerMtok != 15_000_000 || m.ContextWindow != 200000 {
		t.Errorf("sonnet: %+v", m)
	}
	if m := c.Models["fake"]; m.InputPerMtok != money.Micros(75_000) {
		t.Errorf("fractional price: %+v", m)
	}
	if c.Providers["local"].Adapter != "openai" || c.Providers["anthropic"].Adapter != "anthropic" {
		t.Errorf("adapters: %+v", c.Providers)
	}
	if c.Exec.RequestTimeout != 30*time.Second || c.Exec.MaxRetries != 2 || c.Exec.MaxTotal != 120*time.Second {
		t.Errorf("exec config = %+v", c.Exec)
	}
	if len(c.Policies) != 4 {
		t.Errorf("policies = %d", len(c.Policies))
	}
}

func TestParseCatalogDefaultsTimeout(t *testing.T) {
	c, err := ParseCatalog([]byte("providers: {}\nmodels: []\n"))
	if err != nil || c.Exec.RequestTimeout != 60*time.Second {
		t.Fatalf("%v %v", c, err)
	}
}

func TestParseCatalogErrors(t *testing.T) {
	prov := "providers: { p: {} }\n"
	tests := []struct{ name, yaml, want string }{
		{"bad yaml", "models: [", "parse catalog"},
		{"no id", prov + "models: [{provider: p, upstream: u}]", "id is required"},
		{"no upstream", prov + "models: [{id: a, provider: p}]", "upstream is required"},
		{"unknown provider", prov + "models: [{id: a, provider: q, upstream: u}]", "not declared"},
		{"duplicate model", prov + "models: [{id: a, provider: p, upstream: u}, {id: a, provider: p, upstream: u}]", "listed twice"},
		{"bad price", prov + "models: [{id: a, provider: p, upstream: u, input_usd_per_mtok: abc}]", "invalid dollar amount"},
		{"too precise price", prov + "models: [{id: a, provider: p, upstream: u, input_usd_per_mtok: 0.0000001}]", "decimal places"},
		{"negative price", prov + "models: [{id: a, provider: p, upstream: u, input_usd_per_mtok: -1}]", "must not be negative"},
		{"unknown policy type", prov + "policies: [{name: x, type: magic}]", "unknown type"},
		{"fixed without model", prov + "policies: [{name: x, type: fixed}]", "need a model"},
		{"policy unknown model", prov + "policies: [{name: x, type: fixed, model: nope}]", "unknown model"},
		{"policy clashes with model", prov + "models: [{id: a, provider: p, upstream: u}]\npolicies: [{name: a, type: fixed, model: a}]", "same name as a model"},
		{"zero weight", prov + "models: [{id: a, provider: p, upstream: u}]\npolicies: [{name: x, type: weighted, arms: [{model: a, weight: 0}]}]", "positive weight"},
		{"cheapest without tag", prov + "policies: [{name: x, type: cheapest}]", "need a tag"},
	}
	for _, tc := range tests {
		_, err := ParseCatalog([]byte(tc.yaml))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want error containing %q", tc.name, err, tc.want)
		}
	}
}

func TestShippedCatalogParses(t *testing.T) {
	if _, err := LoadCatalog("../../config/models.yaml"); err != nil {
		t.Fatalf("config/models.yaml must always be valid: %v", err)
	}
}
