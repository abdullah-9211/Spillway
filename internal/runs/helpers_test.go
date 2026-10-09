package runs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/money"
	"github.com/abdullah-9211/spillway/internal/provider"
)

// memLog is an in-memory Log that behaves like the database one: ids in order, the writer's epoch and worker id on every
// row, at most one terminal row per step, and a lease that can be taken away.
type memLog struct {
	mu     sync.Mutex
	run    uuid.UUID
	epoch  int64
	worker string
	steps  []Step
	lost   bool
	nextID int64
}

func newMemLog(run uuid.UUID, epoch int64, worker string, prior []Step) *memLog {
	m := &memLog{run: run, epoch: epoch, worker: worker, steps: append([]Step(nil), prior...)}
	for _, s := range prior {
		m.nextID = max(m.nextID, s.ID)
	}
	return m
}

func (m *memLog) Append(_ context.Context, ns NewStep) (Step, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lost {
		return Step{}, ErrLeaseLost
	}
	if ns.StepNo != nil && ns.Phase.Terminal() {
		for _, s := range m.steps {
			if s.StepNo != nil && *s.StepNo == *ns.StepNo && s.Phase.Terminal() {
				return Step{}, ErrDuplicateStep
			}
		}
	}
	b, err := json.Marshal(ns.Payload)
	if err != nil {
		return Step{}, err
	}
	m.nextID++
	s := Step{ID: m.nextID, RunID: m.run, StepNo: ns.StepNo, Type: ns.Type, Phase: ns.Phase, IdempotencyKey: ns.Key, Payload: b,
		Cost: ns.Cost, Epoch: m.epoch, WorkerID: m.worker, At: time.Unix(1_700_000_000+m.nextID, 0).UTC()}
	m.steps = append(m.steps, s)
	return s, nil
}

func (m *memLog) all() []Step {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Step(nil), m.steps...)
}

// scriptedModel answers deterministically from the conversation alone, so a run resumed from any prefix gets the same
// answers the original did. It asks for `echo` as many times as `Calls` says, then gives a final answer.
type scriptedModel struct {
	Calls int
	Cost  int64 // micro-dollars per call; default 1000

	mu    sync.Mutex
	calls int
	fail  []error // returned, in order, before any success
	hook  func(ctx context.Context) error
}

func (m *scriptedModel) Call(ctx context.Context, _ Run, msgs []provider.Message, _ []provider.Tool, _ uuid.UUID) (ModelResult, error) {
	m.mu.Lock()
	m.calls++
	var err error
	if len(m.fail) > 0 {
		err, m.fail = m.fail[0], m.fail[1:]
	}
	hook := m.hook
	m.mu.Unlock()
	if hook != nil {
		if e := hook(ctx); e != nil {
			return ModelResult{}, e
		}
	}
	if err != nil {
		return ModelResult{}, err
	}
	assistant, results := 0, []string{}
	for _, msg := range msgs {
		switch msg.Role {
		case "assistant":
			assistant++
		case "tool":
			results = append(results, msg.Content.PlainText())
		}
	}
	cost := m.Cost
	if cost == 0 {
		cost = 1000
	}
	out := ModelResult{Provider: "fake", Model: "m", FinishReason: "stop", Cost: money.Micros(cost), Usage: &provider.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}
	if assistant < m.Calls {
		out.FinishReason = "tool_calls"
		out.Message = provider.Message{Role: "assistant", Content: provider.Content{Null: true}, ToolCalls: []provider.ToolCall{{
			ID: fmt.Sprintf("call_%d", assistant), Type: "function", Function: provider.FunctionCall{Name: "echo", Arguments: fmt.Sprintf(`{"n":%d}`, assistant)}}}}
		return out, nil
	}
	out.Message = provider.Message{Role: "assistant", Content: provider.TextContent("done: " + strings.Join(results, ","))}
	return out, nil
}

func (m *scriptedModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// echoTool returns "echo:<arguments>" and remembers the idempotency keys it was called with.
type echoTool struct {
	mu   sync.Mutex
	keys []string
	fail []error
	hook func(ctx context.Context, inv ToolInvocation) error
}

func (e *echoTool) Specs(names []string) ([]provider.Tool, error) {
	var out []provider.Tool
	for _, n := range names {
		out = append(out, provider.Tool{Type: "function", Function: provider.ToolFunction{Name: n}})
	}
	return out, nil
}

func (e *echoTool) Call(ctx context.Context, inv ToolInvocation) (string, error) {
	e.mu.Lock()
	e.keys = append(e.keys, inv.IdempotencyKey)
	var err error
	if len(e.fail) > 0 {
		err, e.fail = e.fail[0], e.fail[1:]
	}
	hook := e.hook
	e.mu.Unlock()
	if hook != nil {
		if herr := hook(ctx, inv); herr != nil {
			return "", herr
		}
	}
	if err != nil {
		return "", err
	}
	return "echo:" + string(inv.Arguments), nil
}

func testRun(t *testing.T, req Request, l Limits) Run {
	t.Helper()
	id, _ := uuid.NewV7()
	if l.MaxSteps == 0 {
		l = Limits{MaxSteps: 50, MaxCost: 1_000_000, Deadline: 15 * time.Minute}
	}
	return Run{ID: id, KeyID: uuid.New(), Status: Running, Request: req, Limits: l, DeadlineAt: time.Unix(1_800_000_000, 0).UTC()}
}

func testEngine(m ModelCaller, tools ToolRunner) *Engine {
	return &Engine{Model: m, Tools: tools, Now: func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		Sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() }, ModelRetries: 3, ToolBudget: 3}
}

func req(tools ...string) Request {
	return Request{Input: Input{Text: "do the thing"}, Tools: tools}
}

// outline is a compact reading of a step list: "1 model_call started", "run_status failed:max_steps" and so on.
func outline(steps []Step) []string {
	var out []string
	for _, s := range steps {
		no := "-"
		if s.StepNo != nil {
			no = fmt.Sprint(*s.StepNo)
		}
		line := fmt.Sprintf("%s %s %s", no, s.Type, s.Phase)
		if s.Type == RunStatus {
			var p StatusPayload
			_ = json.Unmarshal(s.Payload, &p)
			line = "run_status " + string(p.Status)
			if p.Reason != "" {
				line += ":" + p.Reason
			}
		}
		out = append(out, line)
	}
	return out
}

func finalStatus(steps []Step) StatusPayload {
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].Type == RunStatus {
			var p StatusPayload
			_ = json.Unmarshal(steps[i].Payload, &p)
			return p
		}
	}
	return StatusPayload{}
}
