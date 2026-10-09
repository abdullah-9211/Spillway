package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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

type Engine struct {
	Model        ModelCaller
	Tools        ToolRunner // optional
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
			}
			err = x.model(ctx, act)
		case RunTool, ResumeTool:
			if act.Kind == RunTool {
				if reason := x.limit(); reason != "" {
					return x.endRun(ctx, Failed, reason, "")
				}
			}
			err = x.tool(ctx, act)
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
	var tools []provider.Tool
	if x.e.Tools != nil && len(x.run.Request.Tools) > 0 {
		var err error
		if tools, err = x.e.Tools.Specs(x.run.Request.Tools); err != nil {
			return x.failStep(ctx, n, ModelCall, ReasonToolFailed, err.Error())
		}
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
	switch {
	case !slices.Contains(x.run.Request.Tools, call.Function.Name):
		callErr = fmt.Errorf("tool %q is not allowed in this run", call.Function.Name) // fed back to the model
	case x.e.Tools == nil:
		callErr = fmt.Errorf("tool %q is not available", call.Function.Name)
	default:
		result, callErr = x.e.Tools.Call(ctx, ToolInvocation{RunID: x.run.ID, StepNo: n, Name: call.Function.Name, Arguments: args, IdempotencyKey: key})
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
