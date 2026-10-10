package runs

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/abdullah-9211/spillway/internal/keys"
)

// Creator starts runs on behalf of the dashboard. A person at the dashboard has no API key secret, so these runs are
// made under a key the service holds itself (the built-in playground key) and are charged to it.
type Creator struct {
	Store      *Store
	Key        func(ctx context.Context) (keys.Key, error)
	Caps       Caps
	Known      func(model string) bool
	ToolsReady bool
	// MissingTools returns which tool names are not registered. nil means no check.
	MissingTools func(ctx context.Context, names []string) ([]string, error)
	Now          func() time.Time
}

// Create validates the request, applies the server's caps and queues the run. raw is the body as received.
func (c *Creator) Create(ctx context.Context, req Request, raw []byte) (Run, error) {
	if err := req.Validate(ValidateOptions{Known: c.Known, ToolsReady: c.ToolsReady}); err != nil {
		return Run{}, err
	}
	if c.MissingTools != nil && len(req.Tools) > 0 {
		missing, err := c.MissingTools(ctx, req.Tools)
		if err != nil {
			return Run{}, err
		}
		if len(missing) > 0 {
			return Run{}, &Validation{Param: "tools", Message: "Unknown tool " + strconv.Quote(missing[0]) + ". Register it before a run can use it."}
		}
	}
	key, err := c.Key(ctx)
	if err != nil {
		return Run{}, err
	}
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	run, _, err := c.Store.Create(ctx, CreateParams{KeyID: key.ID, Request: req, Raw: json.RawMessage(raw), Limits: req.ResolveLimits(c.Caps), Now: now})
	return run, err
}
