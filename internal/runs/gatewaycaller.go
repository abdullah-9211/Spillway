package runs

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/gateway"
	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/provider"
)

// Chatter is the part of the gateway a run needs: the in-process chat path, with routing, retries, fallback, caching,
// rate limits and budgets exactly as for an HTTP request.
type Chatter interface {
	Chat(ctx context.Context, key keys.Key, id uuid.UUID, bucket string, req *provider.ChatRequest) (*gateway.Result, error)
}

type KeyLookup interface {
	Get(ctx context.Context, id uuid.UUID) (keys.Key, error)
}

// GatewayCaller makes a run's model calls through the gateway in this process, charged to the key that created the
// run and tagged with the run's id so the usage rows link to it.
type GatewayCaller struct {
	GW   Chatter
	Keys KeyLookup
}

func (c *GatewayCaller) Call(ctx context.Context, run Run, msgs []provider.Message, tools []provider.Tool, usageID uuid.UUID) (ModelResult, error) {
	key, err := c.Keys.Get(ctx, run.KeyID)
	if err != nil {
		return ModelResult{}, &CallError{Kind: CallTransient, Err: fmt.Errorf("look up the run's key: %w", err)}
	}
	if key.RevokedAt != nil {
		return ModelResult{}, &CallError{Kind: CallKeyRevoked, Err: errors.New("the API key that created this run has been revoked")}
	}
	req := &provider.ChatRequest{Model: run.Request.ModelName(), Messages: msgs, Tools: tools}
	res, err := c.GW.Chat(gateway.WithRun(ctx, run.ID), key, usageID, run.ID.String(), req)
	if err != nil {
		return ModelResult{}, classifyGateway(err)
	}
	if res.Response == nil || len(res.Response.Choices) == 0 {
		return ModelResult{}, &CallError{Kind: CallTransient, Err: errors.New("the provider returned no choices")}
	}
	ch := res.Response.Choices[0]
	return ModelResult{Message: ch.Message, FinishReason: ch.FinishReason, Usage: res.Response.Usage, Provider: res.Provider, Model: res.Model, Cost: res.Cost}, nil
}

// classifyGateway sorts a gateway failure into what the engine does about it.
func classifyGateway(err error) error {
	var ge *gateway.Error
	if !errors.As(err, &ge) {
		return err // not a gateway verdict (a cancelled context, say): the engine decides from the context
	}
	ce := &CallError{Err: err, RetryAfter: ge.RetryAfter}
	switch {
	case ge.Status == 402:
		ce.Kind = CallBudget
	case ge.Status == 429 || ge.Status >= 500:
		ce.Kind = CallTransient
	default:
		ce.Kind = CallPermanent
	}
	return ce
}
