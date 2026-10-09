package gateway

import (
	"context"

	"github.com/google/uuid"
)

type runKey struct{}

// WithRun tags the requests made with this context as belonging to a run, so their usage rows link to it.
func WithRun(ctx context.Context, runID uuid.UUID) context.Context {
	return context.WithValue(ctx, runKey{}, runID)
}

func runFrom(ctx context.Context) *uuid.UUID {
	if id, ok := ctx.Value(runKey{}).(uuid.UUID); ok {
		return &id
	}
	return nil
}
