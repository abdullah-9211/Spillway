package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/runs"
)

type fakeEvents struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

func (f *fakeEvents) Subscribe(uuid.UUID) (<-chan struct{}, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subs == nil {
		f.subs = map[chan struct{}]struct{}{}
	}
	ch := make(chan struct{}, 1)
	f.subs[ch] = struct{}{}
	return ch, func() { f.mu.Lock(); delete(f.subs, ch); f.mu.Unlock() }
}

func (f *fakeEvents) signal() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch := range f.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (f *fakeEvents) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.subs) }

func (r *runsRig) addStep(run uuid.UUID, s runs.Step) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	s.RunID = run
	r.store.steps[run] = append(r.store.steps[run], s)
	if s.Type == runs.RunStatus && s.Phase == runs.PhaseFinished {
		var p runs.StatusPayload
		_ = json.Unmarshal(s.Payload, &p)
		if p.Status.Terminal() {
			r.store.runs[run].Status = p.Status
		}
	}
}

func mkStep(id int64, no int, typ runs.StepType, ph runs.Phase) runs.Step {
	s := runs.Step{ID: id, Type: typ, Phase: ph, Payload: json.RawMessage(`{"n":1}`), WorkerID: "w-1", Epoch: 1, At: time.Unix(1_700_000_000, 0).UTC()}
	if typ != runs.RunStatus {
		s.StepNo = &no
	}
	return s
}

func statusStep(id int64, status runs.Status) runs.Step {
	s := mkStep(id, 0, runs.RunStatus, runs.PhaseFinished)
	s.Payload = json.RawMessage(`{"status":"` + string(status) + `"}`)
	return s
}

type sseEvent struct{ ID, Name, Data string }

type sseReader struct {
	resp *http.Response
	sc   *bufio.Scanner
	t    *testing.T
}

// connect opens the stream and returns a reader of its events. Comment lines (": ping") come back as Name "comment".
func (r *runsRig) connect(t *testing.T, path, token string, hdr ...string) *sseReader {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", r.srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return &sseReader{resp: resp, sc: bufio.NewScanner(resp.Body), t: t}
}

// next reads one event or comment; ok is false at the end of the stream.
func (s *sseReader) next() (sseEvent, bool) {
	var ev sseEvent
	got := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		for s.sc.Scan() {
			line := s.sc.Text()
			switch {
			case line == "":
				if got {
					return
				}
			case strings.HasPrefix(line, ": "):
				if !got {
					ev, got = sseEvent{Name: "comment", Data: line[2:]}, true
					return
				}
			case strings.HasPrefix(line, "id: "):
				ev.ID, got = line[4:], true
			case strings.HasPrefix(line, "event: "):
				ev.Name, got = line[7:], true
			case strings.HasPrefix(line, "data: "):
				ev.Data, got = line[6:], true
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		s.t.Fatal("timed out waiting for an event")
	}
	return ev, got
}

// nextEvent skips comments.
func (s *sseReader) nextEvent() (sseEvent, bool) {
	for {
		ev, ok := s.next()
		if !ok || ev.Name != "comment" {
			return ev, ok
		}
	}
}

func newEventsRig(t *testing.T) (*runsRig, *fakeEvents, uuid.UUID) {
	t.Helper()
	old1, old2 := sseHeartbeat, ssePoll
	sseHeartbeat, ssePoll = time.Hour, time.Hour
	t.Cleanup(func() { sseHeartbeat, ssePoll = old1, old2 })
	r := newRunsRig(t)
	ev := &fakeEvents{}
	r.srvOptions(ev)
	_, c, _ := r.do(t, "POST", "/v1/runs", "key-a", `{"input":"x"}`)
	return r, ev, uuid.MustParse(c["id"].(string))
}

func TestEventsReplayThenLiveThenEnd(t *testing.T) {
	r, ev, id := newEventsRig(t)
	r.addStep(id, statusStep(1, runs.Queued))
	r.addStep(id, mkStep(2, 1, runs.ModelCall, runs.PhaseStarted))

	s := r.connect(t, "/v1/runs/"+id.String()+"/events", "key-a")
	if got := s.resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("content type = %q", got)
	}
	for _, want := range []sseEvent{{ID: "1", Name: "run.status"}, {ID: "2", Name: "step.started"}} {
		e, ok := s.nextEvent()
		if !ok || e.ID != want.ID || e.Name != want.Name {
			t.Fatalf("got %+v, want %+v", e, want)
		}
	}
	// A row written after the client connected arrives live.
	r.addStep(id, mkStep(3, 1, runs.ModelCall, runs.PhaseFinished))
	ev.signal()
	e, _ := s.nextEvent()
	if e.ID != "3" || e.Name != "step.finished" {
		t.Fatalf("live event = %+v", e)
	}
	var d eventData
	if err := json.Unmarshal([]byte(e.Data), &d); err != nil || *d.StepNo != 1 || d.Type != runs.ModelCall || d.Phase != runs.PhaseFinished || d.WorkerID != "w-1" || d.LeaseEpoch != 1 || d.CostUSD != "0.000000" || string(d.Payload) != `{"n":1}` {
		t.Errorf("data = %s (%v)", e.Data, err)
	}
	// The final run.status row is the last event, and the server then ends the stream.
	r.addStep(id, statusStep(4, runs.Succeeded))
	ev.signal()
	if e, _ = s.nextEvent(); e.ID != "4" || e.Name != "run.status" {
		t.Fatalf("final event = %+v", e)
	}
	if _, ok := s.nextEvent(); ok {
		t.Error("the stream must end after the final run.status")
	}
	waitFor(t, func() bool { return ev.count() == 0 }, "the subscription to be released")
}

func waitFor(t *testing.T, f func() bool, what string) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEventNamesFollowThePhase(t *testing.T) {
	for want, s := range map[string]runs.Step{
		"step.started":  mkStep(1, 1, runs.ToolCall, runs.PhaseStarted),
		"step.reissued": mkStep(2, 1, runs.ToolCall, runs.PhaseReissued),
		"step.finished": mkStep(3, 1, runs.ToolCall, runs.PhaseFinished),
		"step.failed":   mkStep(4, 1, runs.ModelCall, runs.PhaseFailed),
		"run.status":    statusStep(5, runs.Running),
	} {
		if got := eventName(s); got != want {
			t.Errorf("%v/%v -> %q, want %q", s.Type, s.Phase, got, want)
		}
	}
}

func TestReconnectWithLastEventIDResumesWithoutGapsOrRepeats(t *testing.T) {
	r, _, id := newEventsRig(t)
	for i := int64(1); i <= 5; i++ {
		r.addStep(id, mkStep(i, int(i), runs.ModelCall, runs.PhaseStarted))
	}
	path := "/v1/runs/" + id.String() + "/events"
	// The first connection reads three events and drops.
	s := r.connect(t, path, "key-a")
	var last string
	for i := 0; i < 3; i++ {
		e, _ := s.nextEvent()
		last = e.ID
	}
	s.resp.Body.Close()
	if last != "3" {
		t.Fatalf("last id = %s", last)
	}
	// Rows keep arriving while the client is away.
	r.addStep(id, mkStep(6, 6, runs.ModelCall, runs.PhaseStarted))
	r.addStep(id, statusStep(7, runs.Succeeded))

	for name, s2 := range map[string]*sseReader{
		"Last-Event-ID": r.connect(t, path, "key-a", "Last-Event-ID", last),
		"?after=":       r.connect(t, path+"?after="+last, "key-a"),
	} {
		var ids []string
		for {
			e, ok := s2.nextEvent()
			if !ok {
				break
			}
			ids = append(ids, e.ID)
		}
		if strings.Join(ids, ",") != "4,5,6,7" {
			t.Errorf("%s: ids = %v, want 4,5,6,7", name, ids)
		}
	}
}

func TestEventsForAFinishedRunEndAtOnceWhenTheClientIsCaughtUp(t *testing.T) {
	r, _, id := newEventsRig(t)
	r.addStep(id, mkStep(1, 1, runs.ModelCall, runs.PhaseStarted))
	r.addStep(id, statusStep(2, runs.Failed))
	s := r.connect(t, "/v1/runs/"+id.String()+"/events", "key-a", "Last-Event-ID", "2")
	if e, ok := s.nextEvent(); ok {
		t.Errorf("a client that already has the final event gets none: %+v", e)
	}
}

func TestEventsCatchUpWithoutANotificationAndSendPings(t *testing.T) {
	r, _, id := newEventsRig(t)
	sseHeartbeat, ssePoll = 30*time.Millisecond, 20*time.Millisecond
	s := r.connect(t, "/v1/runs/"+id.String()+"/events", "key-a")
	if c, _ := s.next(); c.Name != "comment" || c.Data != "connected" {
		t.Fatalf("first line = %+v", c)
	}
	var pinged bool
	for i := 0; i < 5 && !pinged; i++ {
		c, _ := s.next()
		pinged = c.Name == "comment" && c.Data == "ping"
	}
	if !pinged {
		t.Error("an idle stream sends ': ping' comments")
	}
	// No notification is sent, yet the poll finds the new row.
	r.addStep(id, mkStep(9, 1, runs.ModelCall, runs.PhaseStarted))
	if e, _ := s.nextEvent(); e.ID != "9" {
		t.Errorf("poll missed the row: %+v", e)
	}
}

func TestEventsAccessAndErrors(t *testing.T) {
	r, _, id := newEventsRig(t)
	path := "/v1/runs/" + id.String() + "/events"
	if code, _, _ := r.do(t, "GET", path, "", ""); code != 401 {
		t.Errorf("no key: %d", code)
	}
	if code, body, _ := r.do(t, "GET", path, "key-b", ""); code != 404 || errCode(body) != "run_not_found" {
		t.Errorf("another key: %d %v", code, body)
	}
	for _, bad := range []string{"?after=x", "?after=-5"} {
		if code, _, _ := r.do(t, "GET", path+bad, "key-a", ""); code != 400 {
			t.Errorf("%s: %d", bad, code)
		}
	}
	if code, _, _ := r.do(t, "GET", path, "key-a", "", "Last-Event-ID", "nope"); code != 400 {
		t.Errorf("bad Last-Event-ID: %d", code)
	}
	if code, _, _ := r.do(t, "GET", "/v1/runs/not-a-uuid/events", "key-a", ""); code != 404 {
		t.Errorf("bad id: %d", code)
	}
	// Without a broker the endpoint says so instead of hanging.
	r2 := newRunsRig(t)
	_, c, _ := r2.do(t, "POST", "/v1/runs", "key-a", `{"input":"x"}`)
	if code, body, _ := r2.do(t, "GET", "/v1/runs/"+c["id"].(string)+"/events", "key-a", ""); code != 501 || errCode(body) != "events_unavailable" {
		t.Errorf("no broker: %d %v", code, body)
	}
}
