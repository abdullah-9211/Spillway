package gateway

import (
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/abdullah-9211/spillway/internal/money"
	"github.com/abdullah-9211/spillway/internal/provider"
)

// Health tells policies which providers are worth trying right now.
type Health interface {
	Available(provider string) bool
}

type allHealthy struct{}

func (allHealthy) Available(string) bool { return true }

// Plan is the ordered list of models the executor may try for one request.
type Plan struct {
	Name       string  // what the client asked for: a model id or a policy name
	Candidates []Model // in the order to try them
	HedgeAfter time.Duration
}

const defaultOutputTokens = 1024 // assumed output size for `cheapest` when the request sets no limit

func (c *Catalog) modelsWithTag(tag string) []Model {
	var out []Model
	for _, m := range c.Models {
		for _, t := range m.Tags {
			if t == tag {
				out = append(out, m)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Planner is the randomness a policy may use; tests replace it for repeatable draws.
type Planner struct {
	Intn func(n int) int
}

var defaultPlanner = Planner{Intn: rand.IntN}

// Plan turns the request's `model` field into candidates. A concrete model id is shorthand for a fixed
// policy with no fallback. bucket, when set, makes weighted draws sticky for A/B tests. Policies only
// propose candidates; retries, breakers and hedging are the executor's job, so this stays pure.
func (c *Catalog) Plan(name string, req *provider.ChatRequest, h Health, bucket string, pl Planner) (Plan, error) {
	if m, ok := c.Models[name]; ok {
		return Plan{Name: name, Candidates: []Model{m}}, nil
	}
	pol, ok := c.Policies[name]
	if !ok {
		return Plan{}, invalid("model", "unknown model or policy %q", name)
	}
	if h == nil {
		h = allHealthy{}
	}
	if pl.Intn == nil {
		pl = defaultPlanner
	}
	plan := Plan{Name: name, HedgeAfter: pol.HedgeAfter}
	switch pol.Type {
	case "fixed", "fallback":
		for _, id := range pol.Models {
			plan.Candidates = append(plan.Candidates, c.Models[id])
		}
	case "weighted":
		plan.Candidates = c.weighted(pol, bucket, pl)
	case "cheapest":
		plan.Candidates = c.cheapest(pol, req, h)
		if len(plan.Candidates) == 0 {
			return Plan{}, invalid("model", "no model tagged %q can fit this request", pol.Tag)
		}
	default:
		return Plan{}, fmt.Errorf("policy %q has unknown type %q", name, pol.Type)
	}
	return plan, nil
}

func (c *Catalog) weighted(pol Policy, bucket string, pl Planner) []Model {
	total := 0
	for _, a := range pol.Arms {
		total += a.Weight
	}
	var draw int
	if bucket != "" {
		h := fnv.New32a()
		h.Write([]byte(bucket))
		draw = int(h.Sum32() % uint32(total))
	} else {
		draw = pl.Intn(total)
	}
	first := 0
	for i, a := range pol.Arms {
		if draw < a.Weight {
			first = i
			break
		}
		draw -= a.Weight
	}
	out := []Model{c.Models[pol.Arms[first].Model]}
	for i, a := range pol.Arms {
		if i != first {
			out = append(out, c.Models[a.Model])
		}
	}
	return out
}

// cheapest keeps models with the tag whose context window fits the request, orders them by estimated
// cost, and puts models whose provider is currently unavailable last rather than dropping them.
func (c *Catalog) cheapest(pol Policy, req *provider.ChatRequest, h Health) []Model {
	in := estimate(requestChars(req))
	out := req.OutputLimit()
	if out == 0 {
		out = defaultOutputTokens
	}
	type scored struct {
		m    Model
		cost money.Micros
		up   bool
	}
	var list []scored
	for _, m := range c.modelsWithTag(pol.Tag) {
		if m.ContextWindow > 0 && in+out > m.ContextWindow {
			continue
		}
		list = append(list, scored{m, money.TokenCost(in, m.InputPerMtok, out, m.OutputPerMtok), h.Available(m.Provider)})
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].up != list[j].up {
			return list[i].up
		}
		return list[i].cost < list[j].cost
	})
	models := make([]Model, len(list))
	for i, s := range list {
		models[i] = s.m
	}
	return models
}
