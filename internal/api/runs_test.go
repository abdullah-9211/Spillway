package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/runs"
)

// fakeRunStore keeps runs in memory, with the same visibility and idempotency rules as the real store.
type fakeRunStore struct {
	mu        sync.Mutex
	runs      map[uuid.UUID]*runs.Run
	steps     map[uuid.UUID][]runs.Step
	idem      map[string]uuid.UUID
	raws      map[uuid.UUID]string
	out       string
	decisions []runs.Decision
}

func newFakeRunStore() *fakeRunStore {
	return &fakeRunStore{runs: map[uuid.UUID]*runs.Run{}, steps: map[uuid.UUID][]runs.Step{}, idem: map[string]uuid.UUID{}, raws: map[uuid.UUID]string{}}
}

func (f *fakeRunStore) Create(_ context.Context, p runs.CreateParams) (runs.Run, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p.IdempotencyKey != "" {
		if id, ok := f.idem[p.KeyID.String()+p.IdempotencyKey]; ok {
			if f.raws[id] != string(p.Raw) {
				return runs.Run{}, false, runs.ErrIdempotencyConflict
			}
			return *f.runs[id], false, nil
		}
	}
	id, _ := uuid.NewV7()
	r := &runs.Run{ID: id, KeyID: p.KeyID, Status: runs.Queued, Request: p.Request, Limits: p.Limits, DeadlineAt: p.Now.Add(p.Limits.Deadline), CreatedAt: p.Now}
	f.runs[id], f.raws[id] = r, string(p.Raw)
	if p.IdempotencyKey != "" {
		f.idem[p.KeyID.String()+p.IdempotencyKey] = id
	}
	return *r, true, nil
}

func (f *fakeRunStore) GetForKey(_ context.Context, id, key uuid.UUID) (runs.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[id]
	if !ok || r.KeyID != key {
		return runs.Run{}, runs.ErrNotFound
	}
	return *r, nil
}

func (f *fakeRunStore) Steps(_ context.Context, run uuid.UUID, after int64, limit int) ([]runs.Step, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []runs.Step
	for _, s := range f.steps[run] {
		if s.ID > after && len(out) < limit {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeRunStore) FinalAnswer(context.Context, uuid.UUID) (string, error) { return f.out, nil }

func (f *fakeRunStore) RequestCancel(_ context.Context, id, key uuid.UUID, _ time.Time) (runs.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[id]
	if !ok || r.KeyID != key {
		return runs.Run{}, runs.ErrNotFound
	}
	if r.Status.Terminal() {
		return runs.Run{}, runs.ErrFinished
	}
	if r.Status == runs.Queued {
		r.Status, r.FailureReason = runs.Cancelled, "cancelled"
	} else {
		r.CancelRequested = true
	}
	return *r, nil
}

func (f *fakeRunStore) Decide(_ context.Context, id uuid.UUID, key *uuid.UUID, d runs.Decision, _ time.Time) (runs.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[id]
	if !ok || (key != nil && r.KeyID != *key) {
		return runs.Run{}, runs.ErrNotFound
	}
	if r.Status.Terminal() {
		return runs.Run{}, runs.ErrFinished
	}
	if r.Status != runs.WaitingHuman {
		return runs.Run{}, runs.ErrNotWaiting
	}
	f.decisions = append(f.decisions, d)
	if d.Decision == "approve" {
		r.Status = runs.Running
	} else {
		r.Status, r.FailureReason = runs.Failed, runs.ReasonRejected
	}
	return *r, nil
}

type multiAuth map[string]uuid.UUID

func (m multiAuth) Authenticate(_ context.Context, token string) (keys.Key, error) {
	if id, ok := m[token]; ok {
		return keys.Key{ID: id, Name: token}, nil
	}
	return keys.Key{}, keys.ErrInvalidKey
}

type runsRig struct {
	srv   *httptest.Server
	store *fakeRunStore
	a, b  uuid.UUID
	opts  RunsOptions
	t     *testing.T
}

func newRunsRig(t *testing.T) *runsRig {
	t.Helper()
	r := &runsRig{store: newFakeRunStore(), a: uuid.New(), b: uuid.New(), t: t}
	r.opts = RunsOptions{Store: r.store, Caps: runs.Caps{MaxSteps: 50, MaxCost: 1_000_000, Deadline: 15 * time.Minute},
		Known: func(m string) bool { return m == "default" || m == "mini" }, Now: func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }}
	r.start()
	return r
}

func (r *runsRig) start() {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := r.opts
	srv := httptest.NewServer(NewHandler(Options{Auth: multiAuth{"key-a": r.a, "key-b": r.b}, Log: log, Runs: &opts}))
	r.t.Cleanup(srv.Close)
	r.srv = srv
}

// srvOptions restarts the server with a live-events source.
func (r *runsRig) srvOptions(ev Events) {
	r.srv.Close()
	r.opts.Events = ev
	r.start()
}

func (r *runsRig) do(t *testing.T, method, path, token, body string, hdr ...string) (int, map[string]any, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(method, r.srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, resp.Header
}

func errCode(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func TestRunEndpointsNeedAKey(t *testing.T) {
	r := newRunsRig(t)
	for _, tc := range [][2]string{{"POST", "/v1/runs"}, {"GET", "/v1/runs/" + uuid.NewString()}, {"GET", "/v1/runs/" + uuid.NewString() + "/steps"}, {"POST", "/v1/runs/" + uuid.NewString() + "/cancel"}} {
		if code, body, _ := r.do(t, tc[0], tc[1], "", `{"input":"x"}`); code != 401 || errCode(body) != "invalid_api_key" {
			t.Errorf("%s %s without a key: %d %v", tc[0], tc[1], code, body)
		}
		if code, _, _ := r.do(t, tc[0], tc[1], "nope", `{"input":"x"}`); code != 401 {
			t.Errorf("%s %s with a bad key: %d", tc[0], tc[1], code)
		}
	}
}

func TestCreateRun(t *testing.T) {
	r := newRunsRig(t)
	code, body, _ := r.do(t, "POST", "/v1/runs", "key-a", `{"input":"hello","model":"mini","limits":{"max_steps":3}}`)
	if code != 202 || body["status"] != "queued" || body["id"] == nil {
		t.Fatalf("%d %v", code, body)
	}
	id := uuid.MustParse(body["id"].(string))
	stored := r.store.runs[id]
	if stored.KeyID != r.a || stored.Limits.MaxSteps != 3 || stored.Limits.MaxCost != 1_000_000 || stored.Request.ModelName() != "mini" {
		t.Errorf("stored = %+v", stored)
	}
}

func TestCreateRunValidation(t *testing.T) {
	r := newRunsRig(t)
	tests := []struct{ name, body, param string }{
		{"not json", `{`, ""},
		{"an unknown field", `{"input":"x","surprise":1}`, ""},
		{"no input", `{}`, "input"},
		{"an unknown model", `{"input":"x","model":"nope"}`, "model"},
		{"tools not ready", `{"input":"x","tools":["a"]}`, "tools"},
		{"bad limits", `{"input":"x","limits":{"max_steps":0}}`, "limits.max_steps"},
	}
	for _, tc := range tests {
		code, body, _ := r.do(t, "POST", "/v1/runs", "key-a", tc.body)
		if code != 400 || errCode(body) != "invalid_request" {
			t.Errorf("%s: %d %v", tc.name, code, body)
		}
		if tc.param != "" {
			if p := body["error"].(map[string]any)["param"]; p != tc.param {
				t.Errorf("%s: param = %v, want %s", tc.name, p, tc.param)
			}
		}
	}
	if len(r.store.runs) != 0 {
		t.Error("a refused request must not create a run")
	}
	big := `{"input":"` + strings.Repeat("x", maxRunBody) + `"}`
	if code, body, _ := r.do(t, "POST", "/v1/runs", "key-a", big); code != 413 {
		t.Errorf("oversized body: %d %v", code, body)
	}
	if code, _, _ := r.do(t, "POST", "/v1/runs", "key-a", `{"input":"x"}`, "Idempotency-Key", strings.Repeat("k", 300)); code != 400 {
		t.Errorf("long idempotency key: %d", code)
	}
}

func TestIdempotentCreate(t *testing.T) {
	r := newRunsRig(t)
	_, first, h1 := r.do(t, "POST", "/v1/runs", "key-a", `{"input":"x"}`, "Idempotency-Key", "k1")
	code, again, h2 := r.do(t, "POST", "/v1/runs", "key-a", `{"input":"x"}`, "Idempotency-Key", "k1")
	if code != 202 || again["id"] != first["id"] || h1.Get("Idempotent-Replayed") != "" || h2.Get("Idempotent-Replayed") != "true" || len(r.store.runs) != 1 {
		t.Errorf("replay: %d %v %v runs=%d", code, again, h2, len(r.store.runs))
	}
	if code, body, _ := r.do(t, "POST", "/v1/runs", "key-a", `{"input":"different"}`, "Idempotency-Key", "k1"); code != 409 || errCode(body) != "idempotency_key_reused" {
		t.Errorf("same key, other body: %d %v", code, body)
	}
	if code, other, _ := r.do(t, "POST", "/v1/runs", "key-b", `{"input":"x"}`, "Idempotency-Key", "k1"); code != 202 || other["id"] == first["id"] {
		t.Errorf("another API key is independent: %d %v", code, other)
	}
}

func TestGetRun(t *testing.T) {
	r := newRunsRig(t)
	_, c, _ := r.do(t, "POST", "/v1/runs", "key-a", `{"input":"x","metadata":{"ticket":"T-1"}}`)
	id := c["id"].(string)
	code, body, _ := r.do(t, "GET", "/v1/runs/"+id, "key-a", "")
	if code != 200 || body["status"] != "queued" || body["step_count"].(float64) != 0 || body["cost_usd"] != "0.000000" || body["output"] != nil || body["failure_reason"] != nil || body["model"] != "default" {
		t.Fatalf("%d %v", code, body)
	}
	if l := body["limits"].(map[string]any); l["max_steps"].(float64) != 50 || l["max_cost_usd"] != "1.000000" || l["deadline_seconds"].(float64) != 900 {
		t.Errorf("limits = %v", l)
	}
	if body["metadata"].(map[string]any)["ticket"] != "T-1" {
		t.Errorf("metadata = %v", body["metadata"])
	}

	rid := uuid.MustParse(id)
	r.store.runs[rid].Status, r.store.out = runs.Succeeded, "all done"
	if _, body, _ = r.do(t, "GET", "/v1/runs/"+id, "key-a", ""); body["output"] != "all done" {
		t.Errorf("a succeeded run has its output: %v", body)
	}
	r.store.runs[rid].Status, r.store.runs[rid].FailureReason = runs.Failed, "max_steps"
	if _, body, _ = r.do(t, "GET", "/v1/runs/"+id, "key-a", ""); body["failure_reason"] != "max_steps" || body["output"] != nil {
		t.Errorf("a failed run has a reason and no output: %v", body)
	}
}

func TestARunIsVisibleOnlyToItsKey(t *testing.T) {
	r := newRunsRig(t)
	_, c, _ := r.do(t, "POST", "/v1/runs", "key-a", `{"input":"x"}`)
	id := c["id"].(string)
	for _, tc := range [][2]string{{"GET", "/v1/runs/" + id}, {"GET", "/v1/runs/" + id + "/steps"}, {"POST", "/v1/runs/" + id + "/cancel"}} {
		code, body, _ := r.do(t, tc[0], tc[1], "key-b", "")
		if code != 404 || errCode(body) != "run_not_found" {
			t.Errorf("another key %s %s: %d %v", tc[0], tc[1], code, body)
		}
	}
	for _, p := range []string{"/v1/runs/not-a-uuid", "/v1/runs/" + uuid.NewString()} {
		if code, _, _ := r.do(t, "GET", p, "key-a", ""); code != 404 {
			t.Errorf("%s: %d", p, code)
		}
	}
}

func TestRunSteps(t *testing.T) {
	r := newRunsRig(t)
	_, c, _ := r.do(t, "POST", "/v1/runs", "key-a", `{"input":"x"}`)
	id := uuid.MustParse(c["id"].(string))
	one := 1
	for i := int64(1); i <= 5; i++ {
		r.store.steps[id] = append(r.store.steps[id], runs.Step{ID: i * 10, RunID: id, StepNo: &one, Type: runs.ModelCall, Phase: runs.PhaseStarted, Payload: json.RawMessage(`{"n":1}`), Cost: 1500, Epoch: 2, WorkerID: "w-1", At: time.Unix(1_700_000_000, 0).UTC()})
	}
	code, body, _ := r.do(t, "GET", "/v1/runs/"+id.String()+"/steps?limit=2", "key-a", "")
	steps := body["steps"].([]any)
	if code != 200 || len(steps) != 2 || body["next_after"].(float64) != 20 {
		t.Fatalf("%d %v", code, body)
	}
	first := steps[0].(map[string]any)
	if first["id"].(float64) != 10 || first["type"] != "model_call" || first["phase"] != "started" || first["cost_usd"] != "0.001500" || first["worker_id"] != "w-1" || first["lease_epoch"].(float64) != 2 {
		t.Errorf("step = %v", first)
	}
	if _, body, _ = r.do(t, "GET", "/v1/runs/"+id.String()+"/steps?after=20&limit=10", "key-a", ""); len(body["steps"].([]any)) != 3 || body["next_after"].(float64) != 50 {
		t.Errorf("page two: %v", body)
	}
	if _, body, _ = r.do(t, "GET", "/v1/runs/"+id.String()+"/steps?after=50", "key-a", ""); len(body["steps"].([]any)) != 0 || body["next_after"].(float64) != 50 {
		t.Errorf("past the end keeps the cursor: %v", body)
	}
	for _, q := range []string{"after=-1", "after=x", "limit=0", "limit=501", "limit=x"} {
		if code, _, _ := r.do(t, "GET", "/v1/runs/"+id.String()+"/steps?"+q, "key-a", ""); code != 400 {
			t.Errorf("?%s: %d", q, code)
		}
	}
}

func TestCancelRun(t *testing.T) {
	r := newRunsRig(t)
	_, c, _ := r.do(t, "POST", "/v1/runs", "key-a", `{"input":"x"}`)
	id := c["id"].(string)
	code, body, _ := r.do(t, "POST", "/v1/runs/"+id+"/cancel", "key-a", "")
	if code != 202 || body["status"] != "cancelled" || body["failure_reason"] != "cancelled" {
		t.Errorf("cancel queued: %d %v", code, body)
	}
	if code, body, _ = r.do(t, "POST", "/v1/runs/"+id+"/cancel", "key-a", ""); code != 409 || errCode(body) != "run_finished" {
		t.Errorf("cancel again: %d %v", code, body)
	}
	_, c, _ = r.do(t, "POST", "/v1/runs", "key-a", `{"input":"y"}`)
	id2 := uuid.MustParse(c["id"].(string))
	r.store.runs[id2].Status = runs.Running
	if code, body, _ = r.do(t, "POST", "/v1/runs/"+id2.String()+"/cancel", "key-a", ""); code != 202 || body["status"] != "running" || body["cancel_requested"] != true {
		t.Errorf("cancel running: %d %v", code, body)
	}
}

func TestRunsWithToolsMustNameRegisteredTools(t *testing.T) {
	r := newRunsRig(t)
	r.srv.Close()
	r.opts.ToolsReady = true
	r.opts.MissingTools = func(_ context.Context, names []string) ([]string, error) {
		var m []string
		for _, n := range names {
			if n != "send_email" {
				m = append(m, n)
			}
		}
		return m, nil
	}
	r.start()
	if code, body, _ := r.do(t, "POST", "/v1/runs", "key-a", `{"input":"x","tools":["send_email"]}`); code != 202 {
		t.Errorf("a registered tool: %d %v", code, body)
	}
	code, body, _ := r.do(t, "POST", "/v1/runs", "key-a", `{"input":"x","tools":["send_email","nope"]}`)
	if code != 400 || body["error"].(map[string]any)["param"] != "tools" || !strings.Contains(body["error"].(map[string]any)["message"].(string), `"nope"`) {
		t.Errorf("an unknown tool: %d %v", code, body)
	}
	if len(r.store.runs) != 1 {
		t.Errorf("a refused request must not create a run: %d runs", len(r.store.runs))
	}
}

func TestApproveAndRejectOverTheAPI(t *testing.T) {
	r := newRunsRig(t)
	mk := func() uuid.UUID {
		id := uuid.New()
		r.store.mu.Lock()
		r.store.runs[id] = &runs.Run{ID: id, KeyID: r.a, Status: runs.WaitingHuman, Request: runs.Request{Input: runs.Input{Text: "x"}}}
		r.store.mu.Unlock()
		return id
	}
	id := mk()
	if code, body, _ := r.do(t, "POST", "/v1/runs/"+id.String()+"/approve", "", `{}`); code != 401 {
		t.Errorf("no key: %d %v", code, body)
	}
	if code, body, _ := r.do(t, "POST", "/v1/runs/"+id.String()+"/approve", "key-b", `{}`); code != 404 || errCode(body) != "run_not_found" {
		t.Errorf("another key: %d %v", code, body)
	}
	if code, _, _ := r.do(t, "POST", "/v1/runs/"+id.String()+"/approve", "key-a", `{"note":`); code != 400 {
		t.Errorf("bad body: %d", code)
	}
	if code, body, _ := r.do(t, "POST", "/v1/runs/"+id.String()+"/approve", "key-a", `{"note":"looks fine"}`); code != 202 || body["status"] != "running" {
		t.Fatalf("approve: %d %v", code, body)
	}
	if got := r.store.decisions; len(got) != 1 || got[0].Decision != "approve" || got[0].Note != "looks fine" || got[0].By != "api key key-a" {
		t.Errorf("decision = %+v", got)
	}
	if code, body, _ := r.do(t, "POST", "/v1/runs/"+id.String()+"/approve", "key-a", ``); code != 409 || errCode(body) != "run_not_waiting" {
		t.Errorf("second approve: %d %v", code, body)
	}
	id2 := mk()
	if code, body, _ := r.do(t, "POST", "/v1/runs/"+id2.String()+"/reject", "key-a", `{"note":"no"}`); code != 202 || body["status"] != "failed" || body["failure_reason"] != "rejected" {
		t.Errorf("reject: %d %v", code, body)
	}
	if code, body, _ := r.do(t, "POST", "/v1/runs/"+id2.String()+"/reject", "key-a", ``); code != 409 || errCode(body) != "run_finished" {
		t.Errorf("reject finished: %d %v", code, body)
	}
}
