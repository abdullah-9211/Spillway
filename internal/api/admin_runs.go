package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/auth"
	"github.com/abdullah-9211/spillway/internal/runs"
)

// RunsAdmin is what the dashboard reads about runs.
type RunsAdmin interface {
	Summary(ctx context.Context, since time.Time) (runs.Summary, error)
	Activity(ctx context.Context, hours int, now time.Time) (time.Duration, []runs.Bucket, error)
	List(ctx context.Context, f runs.ListFilter) ([]runs.Item, error)
	Get(ctx context.Context, id uuid.UUID) (runs.Item, error)
	Graph(ctx context.Context, id uuid.UUID, now time.Time) (runs.Graph, error)
	Steps(ctx context.Context, id uuid.UUID, after int64, limit int) ([]runs.Step, error)
}

// RunStarter starts and cancels runs for an admin of the dashboard.
type RunStarter interface {
	Create(ctx context.Context, req runs.Request, raw []byte) (runs.Run, error)
	CancelAny(ctx context.Context, id uuid.UUID, now time.Time) (runs.Run, error)
	// Decide approves or rejects a run waiting for a person, whichever key owns it.
	Decide(ctx context.Context, id uuid.UUID, d runs.Decision, now time.Time) (runs.Run, error)
}

func (a *Admin) registerRuns() {
	if a.d.RunStarter != nil {
		a.handle("POST", "/admin/runs", AccessAdmin, a.runsCreate)
		a.handle("POST", "/admin/runs/{id}/cancel", AccessAdmin, a.runsCancel)
		a.handle("POST", "/admin/runs/{id}/approve", AccessAdmin, a.runsDecide("approve"))
		a.handle("POST", "/admin/runs/{id}/reject", AccessAdmin, a.runsDecide("reject"))
	}
	a.handle("GET", "/admin/runs/summary", AccessViewer, a.runsSummary)
	a.handle("GET", "/admin/runs/activity", AccessViewer, a.runsActivity)
	a.handle("GET", "/admin/runs", AccessViewer, a.runsList)
	a.handle("GET", "/admin/runs/{id}", AccessViewer, a.runsGet)
	a.handle("GET", "/admin/runs/{id}/graph", AccessViewer, a.runsGraph)
	if a.d.Events != nil {
		a.handle("GET", "/admin/runs/{id}/events", AccessViewer, a.runsEvents)
	}
}

func (a *Admin) now() time.Time {
	if a.d.Now != nil {
		return a.d.Now()
	}
	return time.Now()
}

// rangeHours reads ?hours=, which is one of the three ranges the page offers.
func rangeHours(w http.ResponseWriter, r *http.Request) (int, bool) {
	switch s := r.URL.Query().Get("hours"); s {
	case "":
		return 24, true
	case "1", "24", "168":
		n, _ := strconv.Atoi(s)
		return n, true
	}
	writeAdminError(w, http.StatusBadRequest, "invalid_request", "hours must be 1, 24 or 168.")
	return 0, false
}

type waitingBody struct {
	ID           uuid.UUID `json:"id"`
	Goal         string    `json:"goal"`
	Tool         string    `json:"tool"`
	WaitingSince time.Time `json:"waiting_since"`
	Key          string    `json:"key"`
}

func (a *Admin) runsSummary(w http.ResponseWriter, r *http.Request, _ auth.Claims) {
	hours, ok := rangeHours(w, r)
	if !ok {
		return
	}
	s, err := a.d.Runs.Summary(r.Context(), a.now().Add(-time.Duration(hours)*time.Hour))
	if err != nil {
		a.d.Log.Error("runs summary", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "internal_error", "Could not load the run counts.")
		return
	}
	waiting := make([]waitingBody, 0, len(s.Waiting))
	for _, x := range s.Waiting {
		waiting = append(waiting, waitingBody(x))
	}
	writeJSON(w, http.StatusOK, map[string]any{"hours": hours, "counts": s.Counts, "waiting": waiting})
}

func (a *Admin) runsActivity(w http.ResponseWriter, r *http.Request, _ auth.Claims) {
	hours, ok := rangeHours(w, r)
	if !ok {
		return
	}
	step, buckets, err := a.d.Runs.Activity(r.Context(), hours, a.now())
	if err != nil {
		a.d.Log.Error("runs activity", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "internal_error", "Could not load the activity.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hours": hours, "bucket_seconds": int(step.Seconds()), "buckets": buckets})
}

type runBody struct {
	ID            uuid.UUID   `json:"id"`
	Status        runs.Status `json:"status"`
	Goal          string      `json:"goal"`
	Key           string      `json:"key"`
	Model         string      `json:"model"`
	StepCount     int         `json:"step_count"`
	CostUSD       string      `json:"cost_usd"`
	CreatedAt     time.Time   `json:"created_at"`
	FinishedAt    *time.Time  `json:"finished_at"`
	DurationMs    int64       `json:"duration_ms"`
	WakeAt        *time.Time  `json:"wake_at"`
	FailureReason *string     `json:"failure_reason"`
	Strip         runs.Strip  `json:"strip"`
}

func runView(it runs.Item, now time.Time) runBody {
	end := now
	if it.FinishedAt != nil {
		end = *it.FinishedAt
	}
	b := runBody{ID: it.ID, Status: it.Status, Goal: it.Goal, Key: it.Key, Model: it.Model, StepCount: it.StepCount, CostUSD: it.Cost.String(),
		CreatedAt: it.CreatedAt, FinishedAt: it.FinishedAt, DurationMs: max(end.Sub(it.CreatedAt).Milliseconds(), 0), WakeAt: it.WakeAt, Strip: it.Strip}
	if b.Strip.Steps == nil {
		b.Strip.Steps = []runs.StripNode{}
	}
	if it.FailureReason != "" {
		b.FailureReason = &it.FailureReason
	}
	return b
}

var runStatuses = map[string]bool{"queued": true, "running": true, "waiting_tool": true, "waiting_human": true, "sleeping": true, "succeeded": true, "failed": true, "cancelled": true}

func (a *Admin) runsList(w http.ResponseWriter, r *http.Request, _ auth.Claims) {
	q := r.URL.Query()
	hours, ok := rangeHours(w, r)
	if !ok {
		return
	}
	f := runs.ListFilter{State: q.Get("state"), Since: a.now().Add(-time.Duration(hours) * time.Hour), Limit: 20}
	if f.State != "" && f.State != "active" && f.State != "finished" {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "state must be active or finished.")
		return
	}
	if s := q.Get("status"); s != "" {
		if !runStatuses[s] {
			writeAdminError(w, http.StatusBadRequest, "invalid_request", "That is not a run status.")
			return
		}
		f.Status = runs.Status(s)
	}
	f.Q = strings.TrimSpace(q.Get("q"))
	if len([]rune(f.Q)) > 100 {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "Search text is limited to 100 characters.")
		return
	}
	if s := q.Get("key_id"); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			writeAdminError(w, http.StatusBadRequest, "invalid_request", "key_id must be a key id.")
			return
		}
		f.KeyID = &id
	}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 100 {
			writeAdminError(w, http.StatusBadRequest, "invalid_request", "limit must be between 1 and 100.")
			return
		}
		f.Limit = n
	}
	if s := q.Get("cursor"); s != "" {
		c, err := runs.DecodeCursor(s)
		if err != nil {
			writeAdminError(w, http.StatusBadRequest, "invalid_request", "That cursor is not valid.")
			return
		}
		f.Before = &c
	}
	want := f.Limit
	f.Limit = want + 1 // one extra row says whether there is another page
	items, err := a.d.Runs.List(r.Context(), f)
	if err != nil {
		a.d.Log.Error("runs list", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "internal_error", "Could not load the runs.")
		return
	}
	var next *string
	if len(items) > want {
		items = items[:want]
		c := runs.EncodeCursor(runs.Cursor{At: items[want-1].CreatedAt, ID: items[want-1].ID})
		next = &c
	}
	now := a.now()
	out := make([]runBody, 0, len(items))
	for _, it := range items {
		out = append(out, runView(it, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out, "next_cursor": next})
}

func (a *Admin) runsGet(w http.ResponseWriter, r *http.Request, _ auth.Claims) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "not_found", "No run with that id.")
		return
	}
	it, err := a.d.Runs.Get(r.Context(), id)
	switch {
	case errors.Is(err, runs.ErrNotFound):
		writeAdminError(w, http.StatusNotFound, "not_found", "No run with that id.")
	case err != nil:
		a.d.Log.Error("run get", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "internal_error", "Could not load the run.")
	default:
		writeJSON(w, http.StatusOK, runView(it, a.now()))
	}
}

func (a *Admin) runsCreate(w http.ResponseWriter, r *http.Request, _ auth.Claims) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "Could not read the request.")
		return
	}
	var req runs.Request
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "Send a JSON body with at least an input.")
		return
	}
	run, err := a.d.RunStarter.Create(r.Context(), req, raw)
	var v *runs.Validation
	switch {
	case errors.As(err, &v):
		writeAdminError(w, http.StatusBadRequest, "invalid_request", v.Message)
	case err != nil:
		a.d.Log.Error("create run", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "internal_error", "Could not start the run. Try again.")
	default:
		writeJSON(w, http.StatusAccepted, map[string]any{"id": run.ID, "status": run.Status})
	}
}

func (a *Admin) runsCancel(w http.ResponseWriter, r *http.Request, _ auth.Claims) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "not_found", "No run with that id.")
		return
	}
	run, err := a.d.RunStarter.CancelAny(r.Context(), id, a.now())
	switch {
	case errors.Is(err, runs.ErrNotFound):
		writeAdminError(w, http.StatusNotFound, "not_found", "No run with that id.")
	case errors.Is(err, runs.ErrFinished):
		writeAdminError(w, http.StatusConflict, "run_finished", "The run has already finished.")
	case err != nil:
		a.d.Log.Error("cancel run", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "internal_error", "Could not cancel the run. Try again.")
	default:
		writeJSON(w, http.StatusAccepted, map[string]any{"id": run.ID, "status": run.Status, "cancel_requested": run.CancelRequested})
	}
}

func (a *Admin) runsGraph(w http.ResponseWriter, r *http.Request, _ auth.Claims) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "not_found", "No run with that id.")
		return
	}
	g, err := a.d.Runs.Graph(r.Context(), id, a.now())
	switch {
	case errors.Is(err, runs.ErrNotFound):
		writeAdminError(w, http.StatusNotFound, "not_found", "No run with that id.")
	case err != nil:
		a.d.Log.Error("run graph", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "internal_error", "Could not load the run graph.")
	default:
		writeJSON(w, http.StatusOK, g)
	}
}

// runsEvents streams a run's rows to the dashboard, for either role: it is read-only.
func (a *Admin) runsEvents(w http.ResponseWriter, r *http.Request, _ auth.Claims) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "not_found", "No run with that id.")
		return
	}
	after, ok, msg := cursorOf(r)
	if !ok {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", msg)
		return
	}
	if _, err := a.d.Runs.Get(r.Context(), id); err != nil {
		if errors.Is(err, runs.ErrNotFound) {
			writeAdminError(w, http.StatusNotFound, "not_found", "No run with that id.")
			return
		}
		a.d.Log.Error("run events", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "internal_error", "Could not load the run.")
		return
	}
	streamEvents(w, r, id, after, stepSource{
		steps: func(ctx context.Context, after int64, limit int) ([]runs.Step, error) {
			return a.d.Runs.Steps(ctx, id, after, limit)
		},
		finished: func(ctx context.Context) (bool, error) {
			it, err := a.d.Runs.Get(ctx, id)
			return it.Status.Terminal(), err
		},
	}, a.d.Events, a.d.Log)
}
