package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/runs"
)

// Events wakes a watcher when rows are written for a run. runs.Broker is the production one.
type Events interface {
	Subscribe(run uuid.UUID) (<-chan struct{}, func())
}

// Variables so tests can shorten them.
var (
	sseHeartbeat = 15 * time.Second // a comment line, so proxies do not close an idle stream
	ssePoll      = time.Second      // a catch-up read in case a notification was missed
)

const sseBatch = 200

// eventName is the SSE event for a row.
func eventName(s runs.Step) string {
	if s.Type == runs.RunStatus {
		return "run.status"
	}
	return "step." + string(s.Phase)
}

type eventData struct {
	StepNo     *int            `json:"step_no"`
	Type       runs.StepType   `json:"type"`
	Phase      runs.Phase      `json:"phase"`
	Payload    json.RawMessage `json:"payload"`
	CostUSD    string          `json:"cost_usd"`
	WorkerID   string          `json:"worker_id"`
	LeaseEpoch int64           `json:"lease_epoch"`
	At         time.Time       `json:"at"`
}

// endsStream: the final run.status row is the last event of a run.
func endsStream(s runs.Step) bool {
	if s.Type != runs.RunStatus || s.Phase != runs.PhaseFinished {
		return false
	}
	var p runs.StatusPayload
	return json.Unmarshal(s.Payload, &p) == nil && p.Status.Terminal()
}

// stepSource reads a run's rows and says whether it has ended. The public API and the admin API each supply one.
type stepSource struct {
	steps    func(ctx context.Context, after int64, limit int) ([]runs.Step, error)
	finished func(ctx context.Context) (bool, error)
}

// cursorOf reads the resume point: Last-Event-ID, or ?after=. ok is false (and a 400 sent) for a bad one.
func cursorOf(r *http.Request) (int64, bool, string) {
	cursor := r.Header.Get("Last-Event-ID")
	if q := r.URL.Query().Get("after"); q != "" {
		cursor = q
	}
	if cursor == "" {
		return 0, true, ""
	}
	n, err := strconv.ParseInt(cursor, 10, 64)
	if err != nil || n < 0 {
		return 0, false, "Last-Event-ID and after must be a step id, zero or more."
	}
	return n, true, ""
}

// streamEvents writes a run's rows as server-sent events: every row after the cursor first, then live ones. The stream
// ends after the final run.status event, or when the client goes. The caller has already checked access.
func streamEvents(w http.ResponseWriter, r *http.Request, id uuid.UUID, after int64, src stepSource, ev Events, log *slog.Logger) {
	rc := http.NewResponseController(w)
	// Subscribe before the first read, so a row written in between is not missed.
	wake, stop := ev.Subscribe(id)
	defer stop()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	write := func(s string) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout)) // a client stalled this long is treated as gone
		if _, err := fmt.Fprint(w, s); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !write(": connected\n\n") {
		return
	}

	ping := time.NewTicker(sseHeartbeat)
	defer ping.Stop()
	poll := time.NewTicker(ssePoll)
	defer poll.Stop()
	drained := false // the run is over and one more read found nothing new
	for {
		rows, err := src.steps(r.Context(), after, sseBatch)
		if err != nil {
			if r.Context().Err() == nil {
				log.Error("read run events", "run_id", id, "error", err)
			}
			return
		}
		for _, s := range rows {
			data, _ := json.Marshal(eventData{StepNo: s.StepNo, Type: s.Type, Phase: s.Phase, Payload: s.Payload, CostUSD: s.Cost.String(), WorkerID: s.WorkerID, LeaseEpoch: s.Epoch, At: s.At})
			if !write(fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", s.ID, eventName(s), data)) {
				return
			}
			after = s.ID
			if endsStream(s) {
				return
			}
		}
		if len(rows) == sseBatch {
			continue // more are waiting
		}
		if len(rows) == 0 {
			// A client that reconnects after the last event has nothing to wait for. The read after seeing the run
			// finished is the check that no final row slipped in between.
			if done, err := src.finished(r.Context()); err == nil && done {
				if drained {
					return
				}
				drained = true
				continue
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-wake:
		case <-poll.C:
		case <-ping.C:
			if !write(": ping\n\n") {
				return
			}
		}
	}
}

// events is GET /v1/runs/{id}/events: the calling key's own run.
func (a *runsAPI) events(w http.ResponseWriter, r *http.Request, key keys.Key, reqID uuid.UUID) {
	id, ok := a.runID(w, r)
	if !ok {
		return
	}
	after, ok, msg := cursorOf(r)
	if !ok {
		writeError(w, 400, "invalid_request_error", "invalid_request", "after", msg)
		return
	}
	if _, err := a.Store.GetForKey(r.Context(), id, key.ID); err != nil {
		if errors.Is(err, runs.ErrNotFound) {
			a.notFound(w)
			return
		}
		a.internal(w, reqID, "get run", err)
		return
	}
	if a.Events == nil {
		writeError(w, 501, "api_error", "events_unavailable", "", "Live events are not available on this server.")
		return
	}
	streamEvents(w, r, id, after, stepSource{
		steps: func(ctx context.Context, after int64, limit int) ([]runs.Step, error) {
			return a.Store.Steps(ctx, id, after, limit)
		},
		finished: func(ctx context.Context) (bool, error) {
			run, err := a.Store.GetForKey(ctx, id, key.ID)
			return run.Status.Terminal(), err
		},
	}, a.Events, a.log)
}
