package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/runs"
)

// RunStore is the part of the run store the public API uses. Everything is scoped to the calling key.
type RunStore interface {
	Create(ctx context.Context, p runs.CreateParams) (runs.Run, bool, error)
	GetForKey(ctx context.Context, id, key uuid.UUID) (runs.Run, error)
	Steps(ctx context.Context, run uuid.UUID, after int64, limit int) ([]runs.Step, error)
	FinalAnswer(ctx context.Context, run uuid.UUID) (string, error)
	RequestCancel(ctx context.Context, id, key uuid.UUID, now time.Time) (runs.Run, error)
	Decide(ctx context.Context, id uuid.UUID, key *uuid.UUID, d runs.Decision, now time.Time) (runs.Run, error)
}

// RunsOptions switch on the run endpoints under /v1/runs.
type RunsOptions struct {
	Store      RunStore
	Caps       runs.Caps
	Known      func(model string) bool // a model id or policy name in the catalog
	ToolsReady bool
	Now        func() time.Time
	// Events delivers live step rows to GET /v1/runs/{id}/events.
	Events Events
	// MissingTools returns which of the names are not in the tool registry. nil means no check.
	MissingTools func(ctx context.Context, names []string) ([]string, error)
}

type runsAPI struct {
	RunsOptions
	v1  *v1
	log *slog.Logger
}

const (
	maxRunBody     = 1 << 20
	maxIdemKeyLen  = 255
	defaultStepsPg = 100
	maxStepsPg     = 500
)

func (a *runsAPI) register(mux *http.ServeMux) {
	mux.Handle("POST /v1/runs", a.v1.withRequest(a.create))
	mux.Handle("GET /v1/runs/{id}", a.v1.withRequest(a.get))
	mux.Handle("GET /v1/runs/{id}/steps", a.v1.withRequest(a.steps))
	mux.Handle("GET /v1/runs/{id}/events", a.v1.withRequest(a.events))
	mux.Handle("POST /v1/runs/{id}/cancel", a.v1.withRequest(a.cancel))
	mux.Handle("POST /v1/runs/{id}/approve", a.v1.withRequest(a.decide("approve")))
	mux.Handle("POST /v1/runs/{id}/reject", a.v1.withRequest(a.decide("reject")))
}

func (a *runsAPI) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

type runJSON struct {
	ID              uuid.UUID       `json:"id"`
	Status          runs.Status     `json:"status"`
	Model           string          `json:"model"`
	StepCount       int             `json:"step_count"`
	CostUSD         string          `json:"cost_usd"`
	FailureReason   *string         `json:"failure_reason"`
	Output          *string         `json:"output"`
	Limits          runs.Limits     `json:"limits"`
	DeadlineAt      time.Time       `json:"deadline_at"`
	CreatedAt       time.Time       `json:"created_at"`
	FinishedAt      *time.Time      `json:"finished_at"`
	CancelRequested bool            `json:"cancel_requested"`
	Metadata        json.RawMessage `json:"metadata,omitempty"`
}

func (a *runsAPI) view(ctx context.Context, r runs.Run) (runJSON, error) {
	out := runJSON{ID: r.ID, Status: r.Status, Model: r.Request.ModelName(), StepCount: r.StepCount, CostUSD: r.Cost.String(), Limits: r.Limits,
		DeadlineAt: r.DeadlineAt, CreatedAt: r.CreatedAt, FinishedAt: r.FinishedAt, CancelRequested: r.CancelRequested, Metadata: r.Request.Metadata}
	if r.FailureReason != "" {
		out.FailureReason = &r.FailureReason
	}
	if r.Status == runs.Succeeded {
		text, err := a.Store.FinalAnswer(ctx, r.ID)
		if err != nil {
			return out, err
		}
		out.Output = &text
	}
	return out, nil
}

func (a *runsAPI) internal(w http.ResponseWriter, id uuid.UUID, what string, err error) {
	a.log.Error(what, "request_id", id, "error", err)
	writeError(w, 500, "api_error", "internal_error", "", "Internal error.")
}

func (a *runsAPI) notFound(w http.ResponseWriter) {
	writeError(w, 404, "invalid_request_error", "run_not_found", "", "No run with that id.")
}

func (a *runsAPI) runID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		a.notFound(w)
		return uuid.Nil, false
	}
	return id, true
}

func (a *runsAPI) create(w http.ResponseWriter, r *http.Request, key keys.Key, reqID uuid.UUID) {
	idem := r.Header.Get("Idempotency-Key")
	if len(idem) > maxIdemKeyLen {
		writeError(w, 400, "invalid_request_error", "invalid_request", "Idempotency-Key", "The Idempotency-Key is too long (255 characters at most).")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRunBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, 413, "invalid_request_error", "request_too_large", "", "Request body is too large.")
			return
		}
		writeError(w, 400, "invalid_request_error", "invalid_request", "", "Could not read the request body.")
		return
	}
	var req runs.Request
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, 400, "invalid_request_error", "invalid_request", "", "Could not parse the request body: "+err.Error())
		return
	}
	if err := req.Validate(runs.ValidateOptions{Known: a.Known, ToolsReady: a.ToolsReady}); err != nil {
		var v *runs.Validation
		if errors.As(err, &v) {
			writeError(w, 400, "invalid_request_error", "invalid_request", v.Param, v.Message)
			return
		}
		a.internal(w, reqID, "validate run request", err)
		return
	}
	if a.MissingTools != nil && len(req.Tools) > 0 {
		missing, err := a.MissingTools(r.Context(), req.Tools)
		if err != nil {
			a.internal(w, reqID, "check tools", err)
			return
		}
		if len(missing) > 0 {
			writeError(w, 400, "invalid_request_error", "invalid_request", "tools", fmt.Sprintf("Unknown tool %q. Register it before a run can use it.", missing[0]))
			return
		}
	}
	run, created, err := a.Store.Create(r.Context(), runs.CreateParams{KeyID: key.ID, IdempotencyKey: idem, Request: req, Raw: raw, Limits: req.ResolveLimits(a.Caps), Now: a.now()})
	switch {
	case errors.Is(err, runs.ErrIdempotencyConflict):
		writeError(w, 409, "invalid_request_error", "idempotency_key_reused", "Idempotency-Key", "This Idempotency-Key was already used with a different request body.")
		return
	case err != nil:
		a.internal(w, reqID, "create run", err)
		return
	}
	if !created {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	writeJSON(w, 202, map[string]any{"id": run.ID, "status": run.Status})
}

func (a *runsAPI) get(w http.ResponseWriter, r *http.Request, key keys.Key, reqID uuid.UUID) {
	id, ok := a.runID(w, r)
	if !ok {
		return
	}
	run, err := a.Store.GetForKey(r.Context(), id, key.ID)
	switch {
	case errors.Is(err, runs.ErrNotFound):
		a.notFound(w)
		return
	case err != nil:
		a.internal(w, reqID, "get run", err)
		return
	}
	out, err := a.view(r.Context(), run)
	if err != nil {
		a.internal(w, reqID, "read run output", err)
		return
	}
	writeJSON(w, 200, out)
}

type stepJSON struct {
	ID             int64           `json:"id"`
	StepNo         *int            `json:"step_no"`
	Type           runs.StepType   `json:"type"`
	Phase          runs.Phase      `json:"phase"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	Payload        json.RawMessage `json:"payload"`
	CostUSD        string          `json:"cost_usd"`
	WorkerID       string          `json:"worker_id"`
	LeaseEpoch     int64           `json:"lease_epoch"`
	At             time.Time       `json:"at"`
}

func (a *runsAPI) steps(w http.ResponseWriter, r *http.Request, key keys.Key, reqID uuid.UUID) {
	id, ok := a.runID(w, r)
	if !ok {
		return
	}
	var after int64
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, 400, "invalid_request_error", "invalid_request", "after", "after must be a step id, zero or more.")
			return
		}
		after = n
	}
	limit := defaultStepsPg
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxStepsPg {
			writeError(w, 400, "invalid_request_error", "invalid_request", "limit", "limit must be between 1 and 500.")
			return
		}
		limit = n
	}
	if _, err := a.Store.GetForKey(r.Context(), id, key.ID); err != nil {
		if errors.Is(err, runs.ErrNotFound) {
			a.notFound(w)
			return
		}
		a.internal(w, reqID, "get run", err)
		return
	}
	rows, err := a.Store.Steps(r.Context(), id, after, limit)
	if err != nil {
		a.internal(w, reqID, "read steps", err)
		return
	}
	out := make([]stepJSON, 0, len(rows))
	for _, s := range rows {
		out = append(out, stepJSON{ID: s.ID, StepNo: s.StepNo, Type: s.Type, Phase: s.Phase, IdempotencyKey: s.IdempotencyKey, Payload: s.Payload,
			CostUSD: s.Cost.String(), WorkerID: s.WorkerID, LeaseEpoch: s.Epoch, At: s.At})
	}
	next := after
	if n := len(rows); n > 0 {
		next = rows[n-1].ID
	}
	writeJSON(w, 200, map[string]any{"steps": out, "next_after": next})
}

func (a *runsAPI) cancel(w http.ResponseWriter, r *http.Request, key keys.Key, reqID uuid.UUID) {
	id, ok := a.runID(w, r)
	if !ok {
		return
	}
	run, err := a.Store.RequestCancel(r.Context(), id, key.ID, a.now())
	switch {
	case errors.Is(err, runs.ErrNotFound):
		a.notFound(w)
		return
	case errors.Is(err, runs.ErrFinished):
		writeError(w, 409, "invalid_request_error", "run_finished", "", "The run has already finished.")
		return
	case err != nil:
		a.internal(w, reqID, "cancel run", err)
		return
	}
	out, err := a.view(r.Context(), run)
	if err != nil {
		a.internal(w, reqID, "read run output", err)
		return
	}
	writeJSON(w, 202, out)
}

// decide answers a run that waits for a person. The caller's key must be the one the run belongs to.
func (a *runsAPI) decide(verb string) func(http.ResponseWriter, *http.Request, keys.Key, uuid.UUID) {
	return func(w http.ResponseWriter, r *http.Request, key keys.Key, reqID uuid.UUID) {
		id, ok := a.runID(w, r)
		if !ok {
			return
		}
		var body struct {
			Note string `json:"note"`
		}
		if r.ContentLength != 0 {
			raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
			if err == nil && len(bytes.TrimSpace(raw)) > 0 {
				err = json.Unmarshal(raw, &body)
			}
			if err != nil {
				writeError(w, 400, "invalid_request_error", "invalid_request", "", "The body must be JSON like {\"note\": \"...\"}, 16 KB at most.")
				return
			}
		}
		if len(body.Note) > runs.MaxNoteBytes {
			writeError(w, 400, "invalid_request_error", "invalid_request", "note", fmt.Sprintf("The note is limited to %d characters.", runs.MaxNoteBytes))
			return
		}
		run, err := a.Store.Decide(r.Context(), id, &key.ID, runs.Decision{Decision: verb, By: "api key " + key.Name, Note: body.Note}, a.now())
		switch {
		case errors.Is(err, runs.ErrNotFound):
			a.notFound(w)
			return
		case errors.Is(err, runs.ErrFinished):
			writeError(w, 409, "invalid_request_error", "run_finished", "", "The run has already finished.")
			return
		case errors.Is(err, runs.ErrNotWaiting):
			writeError(w, 409, "invalid_request_error", "run_not_waiting", "", "The run is not waiting for a decision.")
			return
		case err != nil:
			a.internal(w, reqID, verb+" run", err)
			return
		}
		out, err := a.view(r.Context(), run)
		if err != nil {
			a.internal(w, reqID, "read run output", err)
			return
		}
		writeJSON(w, 202, out)
	}
}
