package gateway

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/abdullah-9211/spillway/internal/provider"
)

const policyCatalog = `
providers:
  a: { type: openai, base_url: "http://a" }
  b: { type: openai, base_url: "http://b" }
  c: { type: openai, base_url: "http://c" }
models:
  - { id: big,   provider: a, upstream: x, input_usd_per_mtok: 3,    output_usd_per_mtok: 15, context_window: 200000, tags: [reasoning] }
  - { id: mid,   provider: b, upstream: x, input_usd_per_mtok: 0.5,  output_usd_per_mtok: 1.5, context_window: 128000, tags: [fast] }
  - { id: small, provider: c, upstream: x, input_usd_per_mtok: 0.1,  output_usd_per_mtok: 0.4, context_window: 8000,   tags: [fast] }
  - { id: free,  provider: c, upstream: x, input_usd_per_mtok: 0,    output_usd_per_mtok: 0,   context_window: 4000,   tags: [fast, free] }
policies:
  - { name: one,      type: fixed, model: big }
  - { name: onefb,    type: fixed, model: big, fallback: [mid] }
  - { name: chain,    type: fallback, models: [big, mid, small], hedge_after_ms: 250 }
  - { name: cheap,    type: cheapest, tag: fast }
  - { name: ab,       type: weighted, arms: [{model: big, weight: 1}, {model: mid, weight: 3}, {model: small, weight: 1}] }
`

type health map[string]bool // provider -> unavailable

func (h health) Available(p string) bool { return !h[p] }

func ids(p Plan) []string {
	var out []string
	for _, m := range p.Candidates {
		out = append(out, m.ID)
	}
	return out
}

func req(chars int, maxTokens int) *provider.ChatRequest {
	r := &provider.ChatRequest{Model: "x", Messages: []provider.Message{{Role: "user", Content: provider.TextContent(string(make([]byte, chars)))}}}
	if maxTokens > 0 {
		r.MaxTokens = &maxTokens
	}
	return r
}

func TestCandidateOrder(t *testing.T) {
	c, err := ParseCatalog([]byte(policyCatalog))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		policy string
		req    *provider.ChatRequest
		h      Health
		want   []string
	}{
		{"concrete model id is a fixed policy", "mid", req(40, 0), nil, []string{"mid"}},
		{"fixed has no fallback", "one", req(40, 0), nil, []string{"big"}},
		{"fixed with fallback list", "onefb", req(40, 0), nil, []string{"big", "mid"}},
		{"fallback keeps listed order", "chain", req(40, 0), nil, []string{"big", "mid", "small"}},
		{"cheapest sorts by estimated cost, free first", "cheap", req(40, 0), nil, []string{"free", "small", "mid"}},
		{"cheapest drops models whose window cannot fit the request", "cheap", req(20000, 0), nil, []string{"small", "mid"}},
		{"output limit counts toward the window", "cheap", req(40, 7995), nil, []string{"mid"}},
		{"cheapest puts unavailable providers last", "cheap", req(40, 0), health{"c": true}, []string{"mid", "free", "small"}},
		{"nothing fits is an error", "cheap", req(400000*4, 0), nil, nil},
		{"unknown name", "nope", req(40, 0), nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := c.Plan(tc.policy, tc.req, tc.h, "", Planner{})
			if tc.want == nil {
				if err == nil {
					t.Fatalf("want an error, got %v", ids(p))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := ids(p); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("order = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHedgeDelayComesFromPolicy(t *testing.T) {
	c, _ := ParseCatalog([]byte(policyCatalog))
	p, _ := c.Plan("chain", req(10, 0), nil, "", Planner{})
	if p.HedgeAfter != 250*time.Millisecond {
		t.Errorf("hedge = %v", p.HedgeAfter)
	}
	if p, _ := c.Plan("one", req(10, 0), nil, "", Planner{}); p.HedgeAfter != 0 {
		t.Errorf("no hedge expected, got %v", p.HedgeAfter)
	}
}

func TestWeightedDraw(t *testing.T) {
	c, _ := ParseCatalog([]byte(policyCatalog))
	// Weights 1,3,1 over a total of 5: draws 0 -> big, 1..3 -> mid, 4 -> small.
	tests := []struct {
		draw int
		want []string
	}{
		{0, []string{"big", "mid", "small"}},
		{1, []string{"mid", "big", "small"}},
		{3, []string{"mid", "big", "small"}},
		{4, []string{"small", "big", "mid"}},
	}
	for _, tc := range tests {
		draw := tc.draw
		p, err := c.Plan("ab", req(10, 0), nil, "", Planner{Intn: func(n int) int {
			if n != 5 {
				t.Errorf("Intn(%d), want the total weight 5", n)
			}
			return draw
		}})
		if err != nil || !reflect.DeepEqual(ids(p), tc.want) {
			t.Errorf("draw %d: %v, %v; want %v", tc.draw, ids(p), err, tc.want)
		}
	}
}

func TestWeightedDistribution(t *testing.T) {
	c, _ := ParseCatalog([]byte(policyCatalog))
	counts := map[string]int{}
	// A fixed, evenly spread sequence of draws, so the test is exact and repeatable.
	i := 0
	pl := Planner{Intn: func(n int) int { v := i % n; i++; return v }}
	for k := 0; k < 5000; k++ {
		p, _ := c.Plan("ab", req(10, 0), nil, "", pl)
		counts[p.Candidates[0].ID]++
	}
	if counts["big"] != 1000 || counts["mid"] != 3000 || counts["small"] != 1000 {
		t.Errorf("distribution = %v, want 1000/3000/1000", counts)
	}
}

func TestWeightedBucketIsSticky(t *testing.T) {
	c, _ := ParseCatalog([]byte(policyCatalog))
	first := map[string]string{}
	for k := 0; k < 200; k++ {
		bucket := fmt.Sprintf("user-%d", k)
		a, _ := c.Plan("ab", req(10, 0), nil, bucket, Planner{})
		first[bucket] = a.Candidates[0].ID
	}
	spread := map[string]bool{}
	for bucket, want := range first {
		for rep := 0; rep < 5; rep++ {
			p, _ := c.Plan("ab", req(10, 0), nil, bucket, Planner{})
			if p.Candidates[0].ID != want {
				t.Fatalf("bucket %s moved from %s to %s", bucket, want, p.Candidates[0].ID)
			}
		}
		spread[want] = true
	}
	if len(spread) < 2 {
		t.Errorf("buckets should spread over arms, all went to %v", spread)
	}
}
