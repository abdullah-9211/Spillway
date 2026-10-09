package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type stub struct {
	srv   *httptest.Server
	last  *http.Request
	body  []byte
	polls atomic.Int32
}

func newStub(t *testing.T, h func(s *stub, w http.ResponseWriter, r *http.Request)) *stub {
	t.Helper()
	s := &stub{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.last = r.Clone(context.Background())
		s.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		h(s, w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func cli(t *testing.T, s *stub, stdin string, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	env := map[string]string{"SPILLWAY_URL": s.srv.URL, "SPILLWAY_API_KEY": "spw_test"}
	err := runsCmd(context.Background(), args, func(k string) string { return env[k] }, strings.NewReader(stdin), &out, &errOut)
	return out.String(), err
}

const runJSONBody = `{"id":"r1","status":"succeeded","model":"default","step_count":3,"cost_usd":"0.004500","failure_reason":null,"output":"the answer","cancel_requested":false,"created_at":"2026-10-09T12:00:00Z","finished_at":"2026-10-09T12:00:03Z"}`

func TestRunsCreateSendsTheRequestAndTheKey(t *testing.T) {
	s := newStub(t, func(s *stub, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(202)
		io.WriteString(w, `{"id":"r1","status":"queued"}`)
	})
	out, err := cli(t, s, "", "create", "--model", "mini", "--system", "be brief", "--max-steps", "4", "--max-cost-usd", "0.25", "--deadline-seconds", "60", "--idempotency-key", "k1", "write", "a", "haiku")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "id:     r1") || !strings.Contains(out, "queued") {
		t.Errorf("out = %q", out)
	}
	if s.last.Method != "POST" || s.last.URL.Path != "/v1/runs" || s.last.Header.Get("Authorization") != "Bearer spw_test" || s.last.Header.Get("Idempotency-Key") != "k1" {
		t.Errorf("request = %s %s %v", s.last.Method, s.last.URL.Path, s.last.Header)
	}
	var body struct {
		Input, Model, System string
		Limits               struct {
			MaxSteps   int         `json:"max_steps"`
			MaxCost    json.Number `json:"max_cost_usd"`
			DeadlineSe int         `json:"deadline_seconds"`
		}
	}
	if err := json.Unmarshal(s.body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Input != "write a haiku" || body.Model != "mini" || body.System != "be brief" || body.Limits.MaxSteps != 4 || body.Limits.MaxCost.String() != "0.25" || body.Limits.DeadlineSe != 60 {
		t.Errorf("body = %s", s.body)
	}
}

func TestRunsCreateReadsStdinAndNeedsATask(t *testing.T) {
	s := newStub(t, func(s *stub, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(202)
		io.WriteString(w, `{"id":"r1","status":"queued"}`)
	})
	if _, err := cli(t, s, "  summarise this \n", "create", "-"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(s.body), `"input":"summarise this"`) {
		t.Errorf("body = %s", s.body)
	}
	if _, err := cli(t, s, "", "create"); err == nil || !strings.Contains(err.Error(), "task") {
		t.Errorf("no task: %v", err)
	}
	if _, err := cli(t, s, "   ", "create", "-"); err == nil {
		t.Error("a blank stdin is no task")
	}
}

func TestRunsGetPrintsTheRunAndItsAnswer(t *testing.T) {
	s := newStub(t, func(s *stub, w http.ResponseWriter, r *http.Request) { io.WriteString(w, runJSONBody) })
	out, err := cli(t, s, "", "get", "r1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"status:   succeeded", "steps:    3", "cost:     $0.004500", "the answer"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if s.last.URL.Path != "/v1/runs/r1" {
		t.Errorf("path = %s", s.last.URL.Path)
	}
	raw, _ := cli(t, s, "", "get", "--json", "r1")
	if strings.TrimSpace(raw) != runJSONBody {
		t.Errorf("--json prints the response as is: %s", raw)
	}
	if _, err := cli(t, s, "", "get"); err == nil {
		t.Error("get needs an id")
	}
}

func TestRunsStepsShowsWorkerAndEpochAndTheReissuedRow(t *testing.T) {
	s := newStub(t, func(s *stub, w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"next_after":4,"steps":[
		 {"id":1,"step_no":null,"type":"run_status","phase":"finished","payload":{"status":"queued"},"cost_usd":"0.000000","worker_id":"","lease_epoch":0},
		 {"id":2,"step_no":1,"type":"model_call","phase":"started","payload":{"policy":"default"},"cost_usd":"0.000000","worker_id":"w-aaa","lease_epoch":1},
		 {"id":3,"step_no":1,"type":"model_call","phase":"reissued","payload":{"previous_worker":"w-aaa","previous_epoch":1},"cost_usd":"0.000000","worker_id":"w-bbb","lease_epoch":2},
		 {"id":4,"step_no":1,"type":"model_call","phase":"finished","payload":{"model":"mini"},"cost_usd":"0.001000","worker_id":"w-bbb","lease_epoch":2}]}`)
	})
	out, err := cli(t, s, "", "steps", "--after", "0", "r1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"reissued", "w-bbb", "after w-aaa (epoch 1)", "model=mini", "status=queued"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if !strings.Contains(s.last.URL.RawQuery, "after=0") {
		t.Errorf("query = %s", s.last.URL.RawQuery)
	}
}

func TestRunsCancel(t *testing.T) {
	s := newStub(t, func(s *stub, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(202)
		io.WriteString(w, strings.Replace(runJSONBody, `"succeeded"`, `"running"`, 1))
	})
	if _, err := cli(t, s, "", "cancel", "r1"); err != nil {
		t.Fatal(err)
	}
	if s.last.Method != "POST" || s.last.URL.Path != "/v1/runs/r1/cancel" {
		t.Errorf("%s %s", s.last.Method, s.last.URL.Path)
	}
}

func TestRunsWaitFollowsTheRunToTheEnd(t *testing.T) {
	s := newStub(t, func(s *stub, w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			w.WriteHeader(202)
			io.WriteString(w, `{"id":"r1","status":"queued"}`)
			return
		}
		if s.polls.Add(1) < 2 {
			io.WriteString(w, strings.Replace(runJSONBody, `"succeeded"`, `"running"`, 1))
			return
		}
		io.WriteString(w, runJSONBody)
	})
	out, err := cli(t, s, "", "create", "--wait", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "running") || !strings.Contains(out, "succeeded") || !strings.Contains(out, "the answer") {
		t.Errorf("out = %q", out)
	}
	// A run that ends badly makes the command fail, so scripts notice.
	f := newStub(t, func(s *stub, w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			w.WriteHeader(202)
			io.WriteString(w, `{"id":"r1","status":"queued"}`)
			return
		}
		io.WriteString(w, strings.Replace(strings.Replace(runJSONBody, `"succeeded"`, `"failed"`, 1), `"failure_reason":null`, `"failure_reason":"max_steps"`, 1))
	})
	out, err = cli(t, f, "", "create", "--wait", "hi")
	if err == nil || !strings.Contains(out, "failed (max_steps)") {
		t.Errorf("a failed run: err=%v out=%q", err, out)
	}
}

func TestRunsErrors(t *testing.T) {
	s := newStub(t, func(s *stub, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		io.WriteString(w, `{"error":{"message":"No run with that id.","type":"invalid_request_error","code":"run_not_found"}}`)
	})
	if _, err := cli(t, s, "", "get", "nope"); err == nil || !strings.Contains(err.Error(), "No run with that id.") || !strings.Contains(err.Error(), "404") {
		t.Errorf("an API error is shown with its message and status: %v", err)
	}
	var out, errOut bytes.Buffer
	if err := runsCmd(context.Background(), []string{"get", "r1"}, func(string) string { return "" }, nil, &out, &errOut); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Errorf("no key: %v", err)
	}
	if err := runsCmd(context.Background(), nil, func(string) string { return "" }, nil, &out, &errOut); err == nil {
		t.Error("no subcommand")
	}
	if err := runsCmd(context.Background(), []string{"explode"}, func(string) string { return "" }, nil, &out, &errOut); err == nil {
		t.Error("unknown subcommand")
	}
	// A service that is not there.
	dead := httptest.NewServer(nil)
	dead.Close()
	env := map[string]string{"SPILLWAY_URL": dead.URL, "SPILLWAY_API_KEY": "k"}
	if err := runsCmd(context.Background(), []string{"get", "r1"}, func(k string) string { return env[k] }, nil, &out, &errOut); err == nil || !strings.Contains(err.Error(), "cannot reach") {
		t.Errorf("unreachable: %v", err)
	}
}

func TestRunsFlagsMayFollowTheWords(t *testing.T) {
	s := newStub(t, func(s *stub, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(202)
		io.WriteString(w, `{"id":"r1","status":"queued"}`)
	})
	if _, err := cli(t, s, "", "create", "say", "hello", "--model", "mini", "again"); err != nil {
		t.Fatal(err)
	}
	var body struct{ Input, Model string }
	_ = json.Unmarshal(s.body, &body)
	if body.Input != "say hello again" || body.Model != "mini" {
		t.Errorf("body = %s", s.body)
	}
	if _, err := cli(t, s, "", "create", "--", "--not-a-flag"); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(s.body, &body)
	if body.Input != "--not-a-flag" {
		t.Errorf("after --, words are the task: %s", s.body)
	}
}
