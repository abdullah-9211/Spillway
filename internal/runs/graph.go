package runs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/abdullah-9211/spillway/internal/db"
	"github.com/abdullah-9211/spillway/internal/money"
	"github.com/abdullah-9211/spillway/internal/usage"
)

// NodeState: finished and failed are ended attempts; running is the one in flight; stopped is an attempt that never
// ended because its worker was lost (or the run ended around it).
const (
	NodeFinished = "finished"
	NodeFailed   = "failed"
	NodeRunning  = "running"
	NodeStopped  = "stopped"
	// NodeWaiting is a wait_human step nobody has decided yet; NodeSleeping is a sleep step before its wake time.
	NodeWaiting  = "waiting"
	NodeSleeping = "sleeping"
)

// GraphNode is one attempt at one step. A step that was re-issued after a worker was lost appears once per attempt.
type GraphNode struct {
	StepNo         int             `json:"step_no"`
	Type           StepType        `json:"type"`
	State          string          `json:"state"`
	Worker         string          `json:"worker"`
	Epoch          int64           `json:"epoch"`
	Reissued       bool            `json:"reissued"`
	PreviousWorker string          `json:"previous_worker,omitempty"`
	PreviousEpoch  int64           `json:"previous_epoch,omitempty"`
	StartedAt      time.Time       `json:"started_at"`
	DurationMs     *int64          `json:"duration_ms"`
	CostUSD        string          `json:"cost_usd"`
	Tool           string          `json:"tool,omitempty"`
	Model          string          `json:"model,omitempty"`
	Provider       string          `json:"provider,omitempty"`
	Cache          string          `json:"cache,omitempty"`
	Tokens         *GraphTokens    `json:"tokens,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	Attempts       []usage.Attempt `json:"attempts"`
	Arguments      string          `json:"arguments,omitempty"`
	Result         string          `json:"result,omitempty"`
	Message        string          `json:"message,omitempty"`
	Error          string          `json:"error,omitempty"`
	// wait_human: why, what is waiting (Gate), and the decision once there is one.
	Reason   string `json:"reason,omitempty"`
	Gate     bool   `json:"gate,omitempty"`
	Decision string `json:"decision,omitempty"`
	By       string `json:"by,omitempty"`
	Note     string `json:"note,omitempty"`
	// sleep: how long, and until when.
	Seconds int        `json:"seconds,omitempty"`
	WakeAt  *time.Time `json:"wake_at,omitempty"`
}

// GraphApproval is the pending approval of a run in waiting_human: what the card on the run page shows.
type GraphApproval struct {
	StepNo    int       `json:"step_no"`
	Reason    string    `json:"reason"`
	Tool      string    `json:"tool,omitempty"`
	Arguments string    `json:"arguments,omitempty"`
	Gate      bool      `json:"gate"`
	Since     time.Time `json:"since"`
}

type GraphTokens struct {
	In  int `json:"in"`
	Out int `json:"out"`
}

type GraphWorker struct {
	ID     string  `json:"id"`
	Epochs []int64 `json:"epochs"`
}

// Recovery is the point where the worker holding the run changed.
type Recovery struct {
	AfterStep  int       `json:"after_step"`
	FromWorker string    `json:"from_worker"`
	ToWorker   string    `json:"to_worker"`
	Epoch      int64     `json:"epoch"`
	At         time.Time `json:"at"`
}

// GraphRun is the run's own facts, for the header, the limits and the lease panel.
type GraphRun struct {
	ID              uuid.UUID  `json:"id"`
	Status          Status     `json:"status"`
	Goal            string     `json:"goal"`
	Key             string     `json:"key"`
	Model           string     `json:"model"`
	Tools           []string   `json:"tools"`
	StepCount       int        `json:"step_count"`
	CostUSD         string     `json:"cost_usd"`
	MaxSteps        int        `json:"max_steps"`
	MaxCostUSD      string     `json:"max_cost_usd"`
	DeadlineSeconds int        `json:"deadline_seconds"`
	CreatedAt       time.Time  `json:"created_at"`
	FinishedAt      *time.Time `json:"finished_at"`
	DeadlineAt      time.Time  `json:"deadline_at"`
	FailureReason   *string    `json:"failure_reason"`
	CancelRequested bool       `json:"cancel_requested"`
	LeaseOwner      *string    `json:"lease_owner"`
	LeaseEpoch      int64      `json:"lease_epoch"`
	LeaseExpiresAt  *time.Time `json:"lease_expires_at"`
	WakeAt          *time.Time `json:"wake_at"`
	// Approval is set while the run waits for a person.
	Approval *GraphApproval `json:"approval"`
}

type Graph struct {
	Run        GraphRun      `json:"run"`
	Workers    []GraphWorker `json:"workers"`
	Nodes      []GraphNode   `json:"nodes"`
	Recoveries []Recovery    `json:"recoveries"`
	LastEvent  int64         `json:"last_event_id"`
}

// UsageInfo is what a model call's usage row says about how it was served.
type UsageInfo struct {
	Provider string
	Model    string
	Cache    string
	Attempts []usage.Attempt
}

const maxDetail = 2000

func clip(s string) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= maxDetail {
		return s
	}
	return string([]rune(s)[:maxDetail]) + "…"
}

type nodeKey struct {
	step  int
	epoch int64
}

// BuildGraph derives the run graph from the step rows alone (and the usage rows they point at). Nodes are grouped by
// (step number, lease epoch), so a step that was resumed by another worker shows both attempts. now stamps the
// duration of an attempt still in flight.
func BuildGraph(run GraphRun, steps []Step, uses map[string]UsageInfo, now time.Time) Graph {
	g := Graph{Run: run, Workers: []GraphWorker{}, Nodes: []GraphNode{}, Recoveries: []Recovery{}}
	type acc struct {
		n        GraphNode
		firstID  int64
		terminal *Step
	}
	nodes := map[nodeKey]*acc{}
	var order []*acc

	var prev *Step
	for i := range steps {
		s := steps[i]
		if s.ID > g.LastEvent {
			g.LastEvent = s.ID
		}
		if s.StepNo == nil {
			continue
		}
		parkBoundary := prev != nil && (prev.Type == WaitHuman || prev.Type == Sleep) && s.Epoch != prev.Epoch &&
			(prev.Phase.Terminal() || (prev.Phase == PhaseStarted && s.Type == prev.Type))
		if prev != nil && s.Epoch != prev.Epoch && !parkBoundary {
			g.Recoveries = append(g.Recoveries, Recovery{AfterStep: *prev.StepNo, FromWorker: prev.WorkerID, ToWorker: s.WorkerID, Epoch: s.Epoch, At: s.At})
		}
		prev = &steps[i]

		k := nodeKey{*s.StepNo, s.Epoch}
		a := nodes[k]
		if a == nil && s.Phase.Terminal() && (s.Type == Sleep || s.Type == WaitHuman) {
			// A sleep is woken, and a wait decided, in a later lease epoch than the one that started it. That is the
			// same attempt finishing, not a new one.
			for _, o := range order {
				if o.n.StepNo == *s.StepNo && o.n.Type == s.Type {
					a = o
				}
			}
		}
		if a == nil {
			a = &acc{firstID: s.ID, n: GraphNode{StepNo: *s.StepNo, Type: s.Type, Worker: s.WorkerID, Epoch: s.Epoch, StartedAt: s.At, CostUSD: money.Micros(0).String(), Attempts: []usage.Attempt{}}}
			nodes[k] = a
			order = append(order, a)
		}
		if s.IdempotencyKey != "" {
			a.n.IdempotencyKey = s.IdempotencyKey
		}
		switch s.Phase {
		case PhaseReissued:
			a.n.Reissued = true
			var p Reissued
			if json.Unmarshal(s.Payload, &p) == nil {
				a.n.PreviousWorker, a.n.PreviousEpoch = p.PreviousWorker, p.PreviousEpoch
			}
		case PhaseStarted:
			switch s.Type {
			case ToolCall:
				var p ToolStarted
				if json.Unmarshal(s.Payload, &p) == nil {
					a.n.Tool, a.n.Arguments = p.Tool, clip(string(p.Arguments))
				}
			case WaitHuman:
				var p WaitStarted
				if json.Unmarshal(s.Payload, &p) == nil {
					a.n.Reason, a.n.Gate, a.n.Tool, a.n.Arguments = p.Reason, !p.Builtin, p.Tool, clip(string(p.Arguments))
				}
			case Sleep:
				var p SleepStarted
				if json.Unmarshal(s.Payload, &p) == nil && !p.WakeAt.IsZero() {
					w := p.WakeAt
					a.n.Seconds, a.n.WakeAt = p.Seconds, &w
				}
			}
		case PhaseFinished, PhaseFailed:
			t := s
			a.terminal = &t
			a.n.CostUSD = s.Cost.String()
		}
	}

	// A re-issued attempt writes no `started` row of its own, so it learns the tool and arguments from the attempt it follows.
	for _, a := range order {
		if a.n.Type != ToolCall || a.n.Tool != "" {
			continue
		}
		for _, o := range order {
			if o.n.StepNo == a.n.StepNo && o.n.Tool != "" {
				a.n.Tool, a.n.Arguments = o.n.Tool, o.n.Arguments
				break
			}
		}
	}

	ended := run.Status.Terminal()
	for _, a := range order {
		n := &a.n
		later := false
		for _, o := range order {
			if o.n.StepNo == n.StepNo && o.firstID > a.firstID {
				later = true
			}
		}
		switch {
		case a.terminal != nil && a.terminal.Phase == PhaseFailed:
			n.State = NodeFailed
		case a.terminal != nil:
			n.State = NodeFinished
		case later || ended:
			n.State = NodeStopped
		case n.Type == WaitHuman && run.Status == WaitingHuman:
			n.State = NodeWaiting
		case n.Type == Sleep && run.Status == Sleeping:
			n.State = NodeSleeping
		default:
			n.State = NodeRunning
		}
		switch n.State {
		case NodeFinished, NodeFailed:
			d := a.terminal.At.Sub(n.StartedAt).Milliseconds()
			n.DurationMs = &d
		case NodeRunning, NodeWaiting, NodeSleeping:
			d := max(now.Sub(n.StartedAt).Milliseconds(), 0)
			n.DurationMs = &d
		}
		if n.State == NodeWaiting {
			g.Run.Approval = &GraphApproval{StepNo: n.StepNo, Reason: n.Reason, Tool: n.Tool, Arguments: n.Arguments, Gate: n.Gate, Since: n.StartedAt}
		}
		if n.State == NodeSleeping {
			g.Run.WakeAt = n.WakeAt
		}
		if a.terminal != nil {
			fillResult(n, a.terminal, uses)
		}
		g.Nodes = append(g.Nodes, *n)
	}
	seen := map[string]int{}
	for _, n := range g.Nodes {
		i, ok := seen[n.Worker]
		if !ok {
			i = len(g.Workers)
			seen[n.Worker] = i
			g.Workers = append(g.Workers, GraphWorker{ID: n.Worker, Epochs: []int64{}})
		}
		w := &g.Workers[i]
		if len(w.Epochs) == 0 || w.Epochs[len(w.Epochs)-1] != n.Epoch {
			w.Epochs = append(w.Epochs, n.Epoch)
		}
	}
	return g
}

func fillResult(n *GraphNode, t *Step, uses map[string]UsageInfo) {
	switch {
	case t.Phase == PhaseFailed:
		var p ErrorPayload
		_ = json.Unmarshal(t.Payload, &p)
		n.Error = clip(p.Error)
	case t.Type == ToolCall:
		var p ToolFinished
		_ = json.Unmarshal(t.Payload, &p)
		n.Result = clip(p.Result)
	case t.Type == WaitHuman:
		var p Decision
		_ = json.Unmarshal(t.Payload, &p)
		n.Decision, n.By, n.Note = p.Decision, p.By, clip(p.Note)
	case t.Type == Compaction:
		var p CompactionFinished
		_ = json.Unmarshal(t.Payload, &p)
		n.Message = clip(p.Summary)
	case t.Type == ModelCall:
		var p ModelFinished
		if json.Unmarshal(t.Payload, &p) != nil {
			return
		}
		n.Provider, n.Model = p.Provider, p.Model
		n.Message = clip(p.Message.Content.PlainText())
		if len(p.Message.ToolCalls) > 0 {
			var names []string
			for _, c := range p.Message.ToolCalls {
				names = append(names, c.Function.Name)
			}
			if n.Message == "" {
				n.Message = "Asked for: " + strings.Join(names, ", ")
			}
		}
		if p.Usage != nil {
			n.Tokens = &GraphTokens{In: p.Usage.PromptTokens, Out: p.Usage.CompletionTokens}
		}
		if u, ok := uses[p.UsageID]; ok {
			n.Cache = u.Cache
			if u.Provider != "" {
				n.Provider = u.Provider
			}
			if u.Model != "" {
				n.Model = u.Model
			}
			if u.Attempts != nil {
				n.Attempts = u.Attempts
			}
		}
	}
}

// Graph loads a run's rows and derives its graph. It reports ErrNotFound for an unknown run.
func (rd *Reader) Graph(ctx context.Context, id uuid.UUID, now time.Time) (Graph, error) {
	var gr GraphRun
	var raw, limits []byte
	var status string
	var cost pgtype.Numeric
	var reason, owner string
	var expires *time.Time
	err := rd.pool.QueryRow(ctx, `SELECT r.id, r.status, r.request, r.limits, r.step_count, r.cost_usd, COALESCE(k.name,''), r.created_at, r.finished_at, r.deadline_at,
		COALESCE(r.failure_reason,''), r.cancel_requested_at IS NOT NULL, COALESCE(r.lease_owner,''), r.lease_epoch, r.lease_expires_at
		FROM runs r LEFT JOIN api_keys k ON k.id = r.api_key_id WHERE r.id = $1`, id).Scan(
		&gr.ID, &status, &raw, &limits, &gr.StepCount, &cost, &gr.Key, &gr.CreatedAt, &gr.FinishedAt, &gr.DeadlineAt, &reason, &gr.CancelRequested, &owner, &gr.LeaseEpoch, &expires)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Graph{}, ErrNotFound
		}
		return Graph{}, err
	}
	gr.Status = Status(status)
	micros, err := db.MicrosFromNumeric(cost)
	if err != nil {
		return Graph{}, err
	}
	gr.CostUSD = micros.String()
	var req Request
	_ = json.Unmarshal(raw, &req)
	var lim Limits
	_ = json.Unmarshal(limits, &lim)
	gr.Goal, gr.Model, gr.Tools = goalOf(raw), req.ModelName(), req.Tools
	if gr.Tools == nil {
		gr.Tools = []string{}
	}
	gr.MaxSteps, gr.MaxCostUSD, gr.DeadlineSeconds = lim.MaxSteps, lim.MaxCost.String(), int(lim.Deadline/time.Second)
	if reason != "" {
		gr.FailureReason = &reason
	}
	if owner != "" {
		gr.LeaseOwner, gr.LeaseExpiresAt = &owner, expires
	}

	st := &Store{pool: rd.pool}
	steps, err := st.Steps(ctx, id, 0, 0)
	if err != nil {
		return Graph{}, err
	}
	var ids []string
	for _, s := range steps {
		if s.Type == ModelCall && s.Phase == PhaseFinished {
			var p ModelFinished
			if json.Unmarshal(s.Payload, &p) == nil && p.UsageID != "" {
				ids = append(ids, p.UsageID)
			}
		}
	}
	uses, err := rd.usageFor(ctx, ids)
	if err != nil {
		return Graph{}, err
	}
	return BuildGraph(gr, steps, uses, now), nil
}

func (rd *Reader) usageFor(ctx context.Context, ids []string) (map[string]UsageInfo, error) {
	out := map[string]UsageInfo{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := rd.pool.Query(ctx, `SELECT id::text, COALESCE(provider,''), COALESCE(model,''), cache_status, attempts FROM usage WHERE id::text = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var u UsageInfo
		var att []byte
		if err := rows.Scan(&id, &u.Provider, &u.Model, &u.Cache, &att); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(att, &u.Attempts)
		out[id] = u
	}
	return out, rows.Err()
}
