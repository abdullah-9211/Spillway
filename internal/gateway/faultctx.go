package gateway

import (
	"context"

	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/provider/faults"
)

type faultsKey struct{}

type requestFaults struct {
	byProvider map[string]faults.Fault
	settings   faults.Settings
}

// WithFaults asks the gateway to make providers fail on purpose for the one request that carries this context.
// Only the playground handler calls it. The gateway still refuses unless fault injection is switched on and the
// request is made with the built-in playground key, so no other caller can use it by accident.
func WithFaults(ctx context.Context, fs []faults.Fault, s faults.Settings) context.Context {
	if len(fs) == 0 {
		return ctx
	}
	rf := requestFaults{byProvider: map[string]faults.Fault{}, settings: s}
	for _, f := range fs {
		rf.byProvider[f.Provider] = f // one fault per provider; a later one replaces an earlier one
	}
	return context.WithValue(ctx, faultsKey{}, rf)
}

func faultFor(ctx context.Context, provider string) (faults.Fault, faults.Settings, bool) {
	rf, ok := ctx.Value(faultsKey{}).(requestFaults)
	if !ok { // absent, or cleared by vetFaults
		return faults.Fault{}, faults.Settings{}, false
	}
	f, ok := rf.byProvider[provider]
	return f, rf.settings, ok
}

// hasFaults reports whether this request is running with injected faults.
func hasFaults(ctx context.Context) bool {
	rf, ok := ctx.Value(faultsKey{}).(requestFaults)
	return ok && len(rf.byProvider) > 0
}

// vetFaults drops any requested faults unless they are allowed here.
func (g *Gateway) vetFaults(ctx context.Context, key keys.Key) context.Context {
	if _, ok := ctx.Value(faultsKey{}).(requestFaults); !ok {
		return ctx
	}
	if g.faultInjection && key.Name == keys.BuiltinName {
		return ctx
	}
	g.log.Warn("ignored fault injection on a request that may not use it", "key_prefix", key.Prefix)
	return context.WithValue(ctx, faultsKey{}, nil)
}

// CandidateProviders names the providers a request could be sent to, in the order its policy would try them. The
// playground uses it to say which requested faults had nothing to act on.
func (g *Gateway) CandidateProviders(model string) ([]string, error) {
	plan, err := g.cat.Plan(model, &provider.ChatRequest{}, g.breakers, "", g.planner)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range plan.Candidates {
		if !seen[m.Provider] {
			seen[m.Provider] = true
			out = append(out, m.Provider)
		}
	}
	return out, nil
}
