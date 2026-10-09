// Package runs is the durable run engine. A run is a loop of model calls and tool calls whose every step is
// appended to run_steps. Any worker can pick a run up from those rows alone, so a worker that dies mid-run loses
// nothing: the next one replays the rows, finds the step that was in flight and does it again.
package runs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/money"
)

type Status string

const (
	Queued       Status = "queued"
	Running      Status = "running"
	WaitingTool  Status = "waiting_tool"
	WaitingHuman Status = "waiting_human"
	Sleeping     Status = "sleeping"
	Succeeded    Status = "succeeded"
	Failed       Status = "failed"
	Cancelled    Status = "cancelled"
)

func (s Status) Terminal() bool { return s == Succeeded || s == Failed || s == Cancelled }

// Failure reasons, as stored in runs.failure_reason.
const (
	ReasonMaxSteps       = "max_steps"
	ReasonMaxCost        = "max_cost"
	ReasonDeadline       = "deadline"
	ReasonProviderFailed = "provider_failed"
	ReasonToolFailed     = "tool_failed"
	ReasonCancelled      = "cancelled"
	ReasonRejected       = "rejected"
	ReasonKeyRevoked     = "key_revoked"
)

type StepType string

const (
	ModelCall  StepType = "model_call"
	ToolCall   StepType = "tool_call"
	WaitHuman  StepType = "wait_human"
	Sleep      StepType = "sleep"
	Compaction StepType = "compaction"
	RunStatus  StepType = "run_status"
)

type Phase string

const (
	PhaseStarted  Phase = "started"
	PhaseReissued Phase = "reissued"
	PhaseFinished Phase = "finished"
	PhaseFailed   Phase = "failed"
)

func (p Phase) Terminal() bool { return p == PhaseFinished || p == PhaseFailed }

var (
	// ErrLeaseLost: another worker holds the run now, so this worker must stop and write nothing more.
	ErrLeaseLost = errors.New("runs: the lease on this run was lost")
	// ErrDuplicateStep: the step already has a terminal row; the second line of defence behind the lease check.
	ErrDuplicateStep = errors.New("runs: the step already has a terminal row")
	// ErrNotFound covers a missing run and a run that belongs to another key.
	ErrNotFound = errors.New("runs: no such run")
	// ErrFinished: the run already ended.
	ErrFinished = errors.New("runs: the run already finished")
	// ErrIdempotencyConflict: the Idempotency-Key was used before with a different request body.
	ErrIdempotencyConflict = errors.New("runs: the idempotency key was used with a different request")
)

// Limits are the limits in force for one run: what the request asked for, capped by the server.
type Limits struct {
	MaxSteps int
	MaxCost  money.Micros
	Deadline time.Duration
}

type limitsJSON struct {
	MaxSteps        int    `json:"max_steps"`
	MaxCostUSD      string `json:"max_cost_usd"`
	DeadlineSeconds int    `json:"deadline_seconds"`
}

func (l Limits) MarshalJSON() ([]byte, error) {
	return json.Marshal(limitsJSON{MaxSteps: l.MaxSteps, MaxCostUSD: l.MaxCost.String(), DeadlineSeconds: int(l.Deadline / time.Second)})
}

func (l *Limits) UnmarshalJSON(b []byte) error {
	var j limitsJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	c, err := money.ParseUSD(j.MaxCostUSD)
	if err != nil {
		return err
	}
	*l = Limits{MaxSteps: j.MaxSteps, MaxCost: c, Deadline: time.Duration(j.DeadlineSeconds) * time.Second}
	return nil
}

// Run is a row of runs: the projection and the lease.
type Run struct {
	ID              uuid.UUID
	KeyID           uuid.UUID
	IdempotencyKey  string
	Status          Status
	Request         Request
	RawRequest      json.RawMessage
	Limits          Limits
	FailureReason   string
	StepCount       int
	Cost            money.Micros
	DeadlineAt      time.Time
	LeaseOwner      string
	LeaseExpiresAt  *time.Time
	LeaseEpoch      int64
	CancelRequested bool
	CreatedAt       time.Time
	FinishedAt      *time.Time
}

// Step is a row of run_steps.
type Step struct {
	ID             int64
	RunID          uuid.UUID
	StepNo         *int
	Type           StepType
	Phase          Phase
	IdempotencyKey string
	Payload        json.RawMessage
	Cost           money.Micros
	Epoch          int64
	WorkerID       string
	At             time.Time
}

// NewStep is what the engine asks a Log to append. The Log adds the id, the writer's epoch and worker id, and the time.
type NewStep struct {
	StepNo  *int
	Type    StepType
	Phase   Phase
	Key     string
	Payload any
	Cost    money.Micros
}

func ptr(n int) *int { return &n }

// IdempotencyKey is hex(sha256(run_id ":" step_no)). It depends only on the step, so a step re-issued after a crash
// sends exactly the key its first attempt did. The receiver must honour it for the effect to happen once.
func IdempotencyKey(run uuid.UUID, stepNo int) string {
	sum := sha256.Sum256([]byte(run.String() + ":" + strconv.Itoa(stepNo)))
	return hex.EncodeToString(sum[:])
}

// Payloads, as written to run_steps.payload.
type ModelStarted struct {
	Policy       string `json:"policy"`
	MessageCount int    `json:"message_count"`
}

type ToolStarted struct {
	Tool       string          `json:"tool"`
	Arguments  json.RawMessage `json:"arguments"`
	ToolCallID string          `json:"tool_call_id"`
}

type ToolFinished struct {
	Result string `json:"result"`
}

type ErrorPayload struct {
	Error string `json:"error"`
}

type Reissued struct {
	PreviousWorker string `json:"previous_worker"`
	PreviousEpoch  int64  `json:"previous_epoch"`
}

type StatusPayload struct {
	Status Status `json:"status"`
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type CompactionStarted struct {
	ReplacesThroughStep int `json:"replaces_through_step"`
}

type CompactionFinished struct {
	Summary string `json:"summary"`
}
