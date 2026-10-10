package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/money"
	"github.com/abdullah-9211/spillway/internal/provider"
)

// Why a worker stopped. They are set as the cause of the run's context.
var (
	// ErrShutdown: the process is stopping. Nothing more is written; the lease is released so another worker resumes.
	ErrShutdown = errors.New("runs: the worker is shutting down")
	// ErrCancelRequested: a cancel arrived. The run ends as cancelled.
	ErrCancelRequested = errors.New("runs: a cancel was requested")
	// ErrAbandoned is what Execute returns when this worker stopped without finishing the run, because it lost the
	// lease or is shutting down. The run is not failed; another worker carries on.
	ErrAbandoned = errors.New("runs: this worker stopped working on the run")
)

// ModelResult is one answer from a model call.
type ModelResult struct {
	Message      provider.Message
	FinishReason string
	Usage        *provider.Usage
	Provider     string
	Model        string
	Cost         money.Micros
}

type CallErrorKind int

const (
	CallTransient CallErrorKind = iota // worth retrying: providers down, rate limited
	CallBudget                         // the key's budget is used up
	CallKeyRevoked
	CallPermanent // the request is wrong; retrying will not help
)

// CallError is a failed model call, already sorted into what the engine should do about it.
type CallError struct {
	Kind       CallErrorKind
	RetryAfter time.Duration
	Err        error
}

func (e *CallError) Error() string { return e.Err.Error() }
func (e *CallError) Unwrap() error { return e.Err }

// ModelCaller makes one model call for a run. The production one goes through the gateway in-process.
type ModelCaller interface {
	Call(ctx context.Context, run Run, messages []provider.Message, tools []provider.Tool, usageID uuid.UUID) (ModelResult, error)
}

type ToolInvocation struct {
	RunID          uuid.UUID
	StepNo         int
	Name           string
	Arguments      json.RawMessage
	IdempotencyKey string
}

// ToolRunner executes tools. The HTTP executor arrives in a later phase; until then the engine runs without one.
type ToolRunner interface {
	Specs(names []string) ([]provider.Tool, error)
	Call(ctx context.Context, inv ToolInvocation) (string, error)
}

// Log appends step rows for one run on behalf of one lease holder. The implementation fences the write: it fails with
// ErrLeaseLost once another worker has claimed the run.
type Log interface {
	Append(ctx context.Context, s NewStep) (Step, error)
}

// ApprovalPolicy is optionally implemented by a ToolRunner: it says whether a registered tool needs a person's approval
// before each call.
type ApprovalPolicy interface {
	RequiresApproval(ctx context.Context, name string) (bool, error)
}

// Compactor summarises the older part of a run's conversation so the run can go on past the model's context. The default
// asks a model; the hook is an interface so it can be replaced.
type Compactor interface {
	Summarise(ctx context.Context, run Run, msgs []provider.Message) (summary string, cost money.Micros, err error)
}

type Engine struct {
	Model        ModelCaller
	Tools        ToolRunner // optional
	Compactor    Compactor  // optional: without one a long run is never compacted
	Now          func() time.Time
	Sleep        func(ctx context.Context, d time.Duration) error
	Backoff      func(attempt int) time.Duration
	ModelRetries int // retries after the first attempt when providers fail; default 3
	ToolBudget   int // consecutive failed tool calls that fail the run; default 3
	Log          *slog.Logger
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) sleep(ctx context.Context, d time.Duration) error {
	if e.Sleep != nil {
		return e.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (e *Engine) backoff(attempt int) time.Duration {
	if e.Backoff != nil {
		return e.Backoff(attempt)
	}
	return min(500*time.Millisecond<<attempt, 8*time.Second)
}

func (e *Engine) retries() int {
	if e.ModelRetries > 0 {
		return e.ModelRetries
	}
	return 3
}

func (e *Engine) toolBudget() int {
	if e.ToolBudget > 0 {
		return e.ToolBudget
	}
	return 3
}

func (e *Engine) logger() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.New(slog.DiscardHandler)
}

type exec struct {
	e   *Engine
	run Run
	log Log
	st  *State
	// compactBroken is set when a compaction failed, so this worker does not keep trying and spending steps.
	compactBroken bool
}

// Execute carries a run forward from whatever its steps say, until it ends or this worker has to stop. It returns nil
// when the run reached a final status, ErrAbandoned when it stopped without one, and any other error for a failure of
// the database or the like; the lease is then left to expire so another worker retries.
func (e *Engine) Execute(ctx context.Context, run Run, log Log, steps []Step) error {
	x := &exec{e: e, run: run, log: log, st: Replay(run.Request, steps)}
	if run.CancelRequested && x.st.Terminal() == nil {
		return x.endRun(ctx, Cancelled, ReasonCancelled, "cancelled before the worker picked it up")
	}
	for {
		if x.st.Terminal() != nil {
			return nil
		}
		if ctx.Err() != nil {
			return x.stopped(ctx, nil)
		}
		act := x.st.Next()
		var err error
		switch act.Kind {
		case Done:
			return nil
		case Succeed:
			return x.endRun(ctx, Succeeded, "", "")
		case StartModel, ResumeModel:
			if act.Kind == StartModel {
				if reason := x.limit(); reason != "" {
					return x.endRun(ctx, Failed, reason, "")
				}
				if through, ok := x.wantsCompaction(); ok {
					err = x.compact(ctx, Action{Kind: StartModel, StepNo: act.StepNo, Replaces: through})
					break
				}
			}
			err = x.model(ctx, act)
		case RunTool, ResumeTool:
			if act.Kind == RunTool {
				if reason := x.limit(); reason != "" {
					return x.endRun(ctx, Failed, reason, "")
				}
			}
			err = x.dispatchTool(ctx, act)
		case ResumeSleep:
			err = x.wake(ctx, act)
		case ParkHuman:
			// Not reachable through a claim (a run waiting for a person is never claimable), but if it happens, say so and wait.
			if err := x.write(ctx, NewStep{Type: RunStatus, Phase: PhaseFinished, Payload: StatusPayload{Status: WaitingHuman}}); err != nil {
				return err
			}
			return ErrParked
		case ResumeCompaction:
			err = x.compact(ctx, act)
		default:
			return x.endRun(ctx, Failed, ReasonProviderFailed, fmt.Sprintf("step %d is of a type this worker cannot resume", act.StepNo))
		}
		if err != nil {
			return err
		}
	}
}

// limit is the check before every new step. It returns the failure reason, or "" if the run may go on.
func (x *exec) limit() string {
	l := x.run.Limits
	switch {
	case x.st.StepCount() >= l.MaxSteps:
		return ReasonMaxSteps
	case l.MaxCost > 0 && x.st.Cost() >= l.MaxCost:
		return ReasonMaxCost
	case !x.e.now().Before(x.run.DeadlineAt):
		return ReasonDeadline
	}
	return ""
}

// write appends a row and folds it into the state. A lost lease surfaces as ErrAbandoned.
func (x *exec) write(ctx context.Context, ns NewStep) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	s, err := x.log.Append(wctx, ns)
	switch {
	case errors.Is(err, ErrLeaseLost), errors.Is(err, ErrDuplicateStep):
		return ErrAbandoned
	case err != nil:
		return err
	}
	x.st.Apply(s)
	return nil
}

// endRun writes the final run_status row. The state then reports Done, so the loop in Execute ends by itself.
func (x *exec) endRun(ctx context.Context, status Status, reason, detail string) error {
	return x.write(ctx, NewStep{Type: RunStatus, Phase: PhaseFinished, Payload: StatusPayload{Status: status, Reason: reason, Detail: detail}})
}

// stopped decides what a cancelled context means. A cancel request ends the run (closing the open step first); a
// lost lease or a shutdown only abandons it.
func (x *exec) stopped(ctx context.Context, open *int) error {
	if errors.Is(context.Cause(ctx), ErrCancelRequested) {
		if open != nil {
			if err := x.write(ctx, NewStep{StepNo: open, Type: x.openType(*open), Phase: PhaseFailed, Payload: ErrorPayload{Error: "cancelled"}}); err != nil {
				return err
			}
		}
		return x.endRun(ctx, Cancelled, ReasonCancelled, "")
	}
	return ErrAbandoned
}

func (x *exec) openType(no int) StepType {
	if in := x.st.steps[no]; in != nil {
		return in.typ
	}
	return ModelCall
}

// reissueOrStart writes the row that opens an attempt at a step: started the first time, reissued when a previous
// worker's attempt never finished.
func (x *exec) open(ctx context.Context, act Action, typ StepType, key string, started any) error {
	if act.Kind == ResumeModel || act.Kind == ResumeTool {
		return x.write(ctx, NewStep{StepNo: ptr(act.StepNo), Type: typ, Phase: PhaseReissued, Key: key,
			Payload: Reissued{PreviousWorker: act.PreviousWorker, PreviousEpoch: act.PreviousEpoch}})
	}
	return x.write(ctx, NewStep{StepNo: ptr(act.StepNo), Type: typ, Phase: PhaseStarted, Key: key, Payload: started})
}

func (x *exec) model(ctx context.Context, act Action) error {
	n := act.StepNo
	msgs := x.st.Messages()
	if err := x.open(ctx, act, ModelCall, "", ModelStarted{Policy: x.run.Request.ModelName(), MessageCount: len(msgs)}); err != nil {
		return err
	}
	tools := builtinSpecs()
	if x.e.Tools != nil && len(x.run.Request.Tools) > 0 {
		specs, err := x.e.Tools.Specs(x.run.Request.Tools)
		if err != nil {
			return x.failStep(ctx, n, ModelCall, ReasonToolFailed, err.Error())
		}
		tools = append(specs, tools...)
	}
	callCtx, cancel := context.WithDeadline(ctx, x.run.DeadlineAt)
	defer cancel()

	var res ModelResult
	var usageID uuid.UUID
	for attempt := 0; ; attempt++ {
		usageID, _ = uuid.NewV7()
		var err error
		res, err = x.e.Model.Call(callCtx, x.run, msgs, tools, usageID)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return x.stopped(ctx, &n)
		}
		if callCtx.Err() != nil { // the run's deadline ended the call
			return x.failStep(ctx, n, ModelCall, ReasonDeadline, "the run's deadline passed during the model call")
		}
		var ce *CallError
		kind := CallTransient
		var wait time.Duration
		if errors.As(err, &ce) {
			kind, wait = ce.Kind, ce.RetryAfter
		}
		switch kind {
		case CallBudget:
			return x.failStep(ctx, n, ModelCall, ReasonMaxCost, "key_budget: "+err.Error())
		case CallKeyRevoked:
			return x.failStep(ctx, n, ModelCall, ReasonKeyRevoked, err.Error())
		case CallPermanent:
			return x.failStep(ctx, n, ModelCall, ReasonProviderFailed, err.Error())
		}
		if attempt >= x.e.retries() {
			return x.failStep(ctx, n, ModelCall, ReasonProviderFailed, fmt.Sprintf("after %d attempts: %v", attempt+1, err))
		}
		x.e.logger().Warn("model call failed, retrying the step", "run_id", x.run.ID, "step", n, "attempt", attempt+1, "error", err)
		if wait <= 0 || wait > 10*time.Second {
			wait = x.e.backoff(attempt)
		}
		if err := x.e.sleep(callCtx, wait); err != nil {
			if ctx.Err() != nil {
				return x.stopped(ctx, &n)
			}
			return x.failStep(ctx, n, ModelCall, ReasonDeadline, "the run's deadline passed while waiting to retry")
		}
	}
	return x.write(ctx, NewStep{StepNo: ptr(n), Type: ModelCall, Phase: PhaseFinished, Cost: res.Cost, Payload: ModelFinished{
		Message: res.Message, FinishReason: res.FinishReason, Usage: res.Usage, Provider: res.Provider, Model: res.Model, UsageID: usageID.String(),
	}})
}

// failStep closes the open step as failed, then ends the run with the reason.
func (x *exec) failStep(ctx context.Context, n int, typ StepType, reason, detail string) error {
	if err := x.write(ctx, NewStep{StepNo: ptr(n), Type: typ, Phase: PhaseFailed, Payload: ErrorPayload{Error: detail}}); err != nil {
		return err
	}
	return x.endRun(ctx, Failed, reason, detail)
}

func (x *exec) tool(ctx context.Context, act Action) error {
	n, call := act.StepNo, act.Call
	key := IdempotencyKey(x.run.ID, n)
	args := json.RawMessage(call.Function.Arguments)
	if !json.Valid(args) {
		args, _ = json.Marshal(call.Function.Arguments)
	}
	if err := x.open(ctx, act, ToolCall, key, ToolStarted{Tool: call.Function.Name, Arguments: args, ToolCallID: call.ID}); err != nil {
		return err
	}
	var result string
	var callErr error
	orig, allowed := x.resolve(call.Function.Name)
	switch {
	case !allowed:
		callErr = fmt.Errorf("tool %q is not allowed in this run", call.Function.Name) // fed back to the model
	case x.e.Tools == nil:
		callErr = fmt.Errorf("tool %q is not available", call.Function.Name)
	default:
		result, callErr = x.e.Tools.Call(ctx, ToolInvocation{RunID: x.run.ID, StepNo: n, Name: orig, Arguments: args, IdempotencyKey: key})
	}
	if callErr != nil && ctx.Err() != nil {
		return x.stopped(ctx, &n)
	}
	if callErr == nil {
		return x.write(ctx, NewStep{StepNo: ptr(n), Type: ToolCall, Phase: PhaseFinished, Key: key, Payload: ToolFinished{Result: result}})
	}
	if err := x.write(ctx, NewStep{StepNo: ptr(n), Type: ToolCall, Phase: PhaseFailed, Key: key, Payload: ErrorPayload{Error: callErr.Error()}}); err != nil {
		return err
	}
	if x.st.ToolErrors() >= x.e.toolBudget() {
		return x.endRun(ctx, Failed, ReasonToolFailed, fmt.Sprintf("%d tool calls in a row failed; the last: %v", x.st.ToolErrors(), callErr))
	}
	return nil
}

// --- built-in tools, approvals, sleeping and compaction ---

const (
	BuiltinSleep    = "sleep"
	BuiltinApproval = "request_human_approval"
	maxSleepSeconds = 86400
)

func builtinSpecs() []provider.Tool {
	return []provider.Tool{
		{Type: "function", Function: provider.ToolFunction{Name: BuiltinSleep, Description: "Wait for a number of seconds, then continue. Use it to poll or to wait for something that takes time.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"seconds":{"type":"integer","minimum":1,"maximum":86400}},"required":["seconds"]}`)}},
		{Type: "function", Function: provider.ToolFunction{Name: BuiltinApproval, Description: "Ask a person to approve before you go on. Say what you want to do and why.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string"}},"required":["reason"]}`)}},
	}
}

// ModelName is how a registered tool is named to the model: some providers do not allow a dot, which MCP tool names
// ("server.tool") contain.
func ModelName(registered string) string { return strings.ReplaceAll(registered, ".", "__") }

// resolve maps the name the model used to the registered tool it is allowed to call.
func (x *exec) resolve(callName string) (string, bool) {
	for _, t := range x.run.Request.Tools {
		if t == callName || ModelName(t) == callName {
			return t, true
		}
	}
	return "", false
}

func (x *exec) needsApproval(ctx context.Context, orig string) bool {
	for _, t := range x.run.Request.ApprovalRequired {
		if t == orig || ModelName(t) == orig {
			return true
		}
	}
	if ap, ok := x.e.Tools.(ApprovalPolicy); ok && x.e.Tools != nil {
		need, err := ap.RequiresApproval(ctx, orig)
		return err == nil && need
	}
	return false
}

// dispatchTool runs a pending tool call: a built-in, a gated tool (which first waits for approval), or a plain tool.
func (x *exec) dispatchTool(ctx context.Context, act Action) error {
	name := act.Call.Function.Name
	switch name {
	case BuiltinSleep:
		return x.sleep(ctx, act)
	case BuiltinApproval:
		return x.waitForPerson(ctx, act, WaitStarted{Reason: reasonOf(act.Call.Function.Arguments), ToolCallID: act.Call.ID, Builtin: true})
	}
	if act.Kind == RunTool {
		if orig, ok := x.resolve(name); ok && !x.st.Approved(act.Call.ID) && x.needsApproval(ctx, orig) {
			args := json.RawMessage(act.Call.Function.Arguments)
			if !json.Valid(args) {
				args, _ = json.Marshal(act.Call.Function.Arguments)
			}
			return x.waitForPerson(ctx, act, WaitStarted{Reason: "Approval needed to call " + orig, Tool: orig, Arguments: args, ToolCallID: act.Call.ID})
		}
	}
	return x.tool(ctx, act)
}

func reasonOf(args string) string {
	var a struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal([]byte(args), &a) == nil && strings.TrimSpace(a.Reason) != "" {
		return strings.TrimSpace(a.Reason)
	}
	return "The model asked for approval to continue."
}

// waitForPerson opens a wait_human step and parks the run. Nothing runs until a person decides.
func (x *exec) waitForPerson(ctx context.Context, act Action, p WaitStarted) error {
	if err := x.write(ctx, NewStep{StepNo: ptr(act.StepNo), Type: WaitHuman, Phase: PhaseStarted, Payload: p}); err != nil {
		return err
	}
	return ErrParked
}

// sleep starts a sleep step and parks the run until its wake time.
func (x *exec) sleep(ctx context.Context, act Action) error {
	var a struct {
		Seconds int `json:"seconds"`
	}
	_ = json.Unmarshal([]byte(act.Call.Function.Arguments), &a)
	n := act.StepNo
	started := SleepStarted{Seconds: a.Seconds, WakeAt: x.e.now().Add(time.Duration(a.Seconds) * time.Second), ToolCallID: act.Call.ID}
	bad := ""
	switch {
	case a.Seconds < 1 || a.Seconds > maxSleepSeconds:
		bad = fmt.Sprintf("seconds must be a whole number from 1 to %d", maxSleepSeconds)
	case !started.WakeAt.Before(x.run.DeadlineAt):
		bad = "sleeping that long would pass the run's deadline"
	}
	if bad != "" { // fed back to the model as the call's result
		if err := x.write(ctx, NewStep{StepNo: ptr(n), Type: Sleep, Phase: PhaseStarted, Payload: SleepStarted{ToolCallID: act.Call.ID}}); err != nil {
			return err
		}
		return x.write(ctx, NewStep{StepNo: ptr(n), Type: Sleep, Phase: PhaseFailed, Payload: ErrorPayload{Error: bad}})
	}
	if err := x.write(ctx, NewStep{StepNo: ptr(n), Type: Sleep, Phase: PhaseStarted, Payload: started}); err != nil {
		return err
	}
	return ErrParked
}

// wake finishes a sleep whose time has come. A worker is only handed a sleeping run at its wake time, so a sleep that
// still has time left (an early claim) parks again rather than cutting the wait short.
func (x *exec) wake(ctx context.Context, act Action) error {
	if x.e.now().Before(act.Sleep.WakeAt) {
		if err := x.write(ctx, NewStep{Type: RunStatus, Phase: PhaseFinished, Payload: StatusPayload{Status: Sleeping}}); err != nil {
			return err
		}
		return ErrParked
	}
	return x.write(ctx, NewStep{StepNo: ptr(act.StepNo), Type: Sleep, Phase: PhaseFinished, Payload: struct{}{}})
}

const (
	defaultCompactTokens = 24000
	defaultKeepTurns     = 6
)

// wantsCompaction: the history has outgrown the threshold and there is something older than the last few turns to fold.
func (x *exec) wantsCompaction() (int, bool) {
	if x.e.Compactor == nil || x.compactBroken {
		return 0, false
	}
	threshold, keep := defaultCompactTokens, defaultKeepTurns
	if c := x.run.Request.Compaction; c != nil {
		if c.ThresholdTokens > 0 {
			threshold = c.ThresholdTokens
		}
		if c.KeepLastTurns > 0 {
			keep = c.KeepLastTurns
		}
	}
	if EstimateTokens(x.st.Messages()) < threshold {
		return 0, false
	}
	through, _, ok := x.st.CompactionPlan(keep)
	return through, ok
}

// compact summarises the older turns as a compaction step. A failure is recorded and the run goes on without it.
func (x *exec) compact(ctx context.Context, act Action) error {
	n := act.StepNo
	reissue := act.Kind == ResumeCompaction
	through := act.Replaces
	if err := x.open(ctx, Action{Kind: map[bool]Frontier{true: ResumeModel, false: StartModel}[reissue], StepNo: n, PreviousWorker: act.PreviousWorker, PreviousEpoch: act.PreviousEpoch},
		Compaction, "", CompactionStarted{ReplacesThroughStep: through}); err != nil {
		return err
	}
	var msgs []provider.Message
	for _, e := range x.st.entries {
		if e.step != -1 && e.step <= through {
			msgs = append(msgs, e.msg)
		}
	}
	cctx, cancel := context.WithDeadline(ctx, x.run.DeadlineAt)
	defer cancel()
	summary, cost, err := x.e.Compactor.Summarise(cctx, x.run, msgs)
	if err != nil {
		if ctx.Err() != nil {
			return x.stopped(ctx, &n)
		}
		x.compactBroken = true
		x.e.logger().Warn("compaction failed, going on without it", "run_id", x.run.ID, "error", err)
		return x.write(ctx, NewStep{StepNo: ptr(n), Type: Compaction, Phase: PhaseFailed, Payload: ErrorPayload{Error: err.Error()}})
	}
	return x.write(ctx, NewStep{StepNo: ptr(n), Type: Compaction, Phase: PhaseFinished, Cost: cost, Payload: CompactionFinished{Summary: summary}})
}

// ModelCompactor is the default Compactor: it asks a model, through the same gateway path as the run's own calls.
type ModelCompactor struct {
	Model  ModelCaller
	Policy string // the policy or model to use; empty means the run's own
}

func (c *ModelCompactor) Summarise(ctx context.Context, run Run, msgs []provider.Message) (string, money.Micros, error) {
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "tool":
			fmt.Fprintf(&b, "[tool result] %s\n", m.Content.PlainText())
		default:
			text := m.Content.PlainText()
			for _, tc := range m.ToolCalls {
				text += fmt.Sprintf(" [asked for %s(%s)]", tc.Function.Name, tc.Function.Arguments)
			}
			fmt.Fprintf(&b, "[%s] %s\n", m.Role, strings.TrimSpace(text))
		}
	}
	r := run
	if c.Policy != "" {
		req := r.Request
		req.Model = c.Policy
		r.Request = req
	}
	id, _ := uuid.NewV7()
	res, err := c.Model.Call(ctx, r, []provider.Message{
		{Role: "system", Content: provider.TextContent("Summarise this conversation between an agent and its tools so the agent can continue the task from the summary alone. Keep facts, decisions, results and what is still to do. Be concise.")},
		{Role: "user", Content: provider.TextContent(b.String())},
	}, nil, id)
	if err != nil {
		return "", 0, err
	}
	text := strings.TrimSpace(res.Message.Content.PlainText())
	if text == "" {
		return "", 0, errors.New("the summary was empty")
	}
	return text, res.Cost, nil
}
