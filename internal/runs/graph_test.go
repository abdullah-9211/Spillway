package runs

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/abdullah-9211/spillway/internal/usage"
)

var gBase = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// at stamps a row with a time `ms` after the base, and a unique id.
func at(s Step, id int64, ms int) Step {
	s.ID = id
	s.At = gBase.Add(time.Duration(ms) * time.Millisecond)
	return s
}

func modelStart(no int, epoch int64, w string) Step {
	return row(0, no, ModelCall, PhaseStarted, epoch, w, ModelStarted{Policy: "default"}, 0)
}

func modelDone(no int, epoch int64, w, usageID string, cost int64) Step {
	return row(0, no, ModelCall, PhaseFinished, epoch, w, ModelFinished{Message: text("an answer"), Provider: "anthropic", Model: "sonnet", UsageID: usageID}, cost)
}

func tStart(no int, epoch int64, w, call string) Step {
	s := toolStarted(0, no, epoch, w, call)
	s.IdempotencyKey = IdempotencyKey(testRunID, no)
	return s
}

func tRedo(no int, epoch int64, w, prevW string, prevE int64) Step {
	s := row(0, no, ToolCall, PhaseReissued, epoch, w, Reissued{PreviousWorker: prevW, PreviousEpoch: prevE}, 0)
	s.IdempotencyKey = IdempotencyKey(testRunID, no)
	return s
}

func tDone(no int, epoch int64, w string) Step {
	s := row(0, no, ToolCall, PhaseFinished, epoch, w, ToolFinished{Result: "page text"}, 0)
	s.IdempotencyKey = IdempotencyKey(testRunID, no)
	return s
}

func seq(steps ...Step) []Step {
	out := make([]Step, len(steps))
	for i, s := range steps {
		out[i] = at(s, int64(i+1), (i+1)*100)
	}
	return out
}

func brief(g Graph) []string {
	var out []string
	for _, n := range g.Nodes {
		redo := ""
		if n.Reissued {
			redo = "+redo"
		}
		out = append(out, fmt.Sprintf("%d/%s/%s/e%d/%s%s", n.StepNo, n.Type, n.Worker, n.Epoch, n.State, redo))
	}
	return out
}

func rec(g Graph) []string {
	var out []string
	for _, r := range g.Recoveries {
		out = append(out, fmt.Sprintf("after %d: %s -> %s e%d", r.AfterStep, r.FromWorker, r.ToWorker, r.Epoch))
	}
	return out
}

func eq(t *testing.T, name string, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\n got %q\nwant %q", name, got, want)
	}
}

var runLive = GraphRun{ID: testRunID, Status: Running}

func TestGraphWithNoRecovery(t *testing.T) {
	g := BuildGraph(runLive, seq(
		modelStart(1, 1, "w-1"), modelDone(1, 1, "w-1", "", 4800),
		tStart(2, 1, "w-1", "c0"), tDone(2, 1, "w-1"),
		modelStart(3, 1, "w-1"), modelDone(3, 1, "w-1", "", 7100),
	), nil, gBase.Add(time.Minute))
	eq(t, "nodes", brief(g), "1/model_call/w-1/e1/finished", "2/tool_call/w-1/e1/finished", "3/model_call/w-1/e1/finished")
	eq(t, "recoveries", rec(g))
	if len(g.Workers) != 1 || g.Workers[0].ID != "w-1" || len(g.Workers[0].Epochs) != 1 {
		t.Errorf("workers = %+v", g.Workers)
	}
	if g.LastEvent != 6 {
		t.Errorf("last event = %d", g.LastEvent)
	}
	if n := g.Nodes[0]; n.CostUSD != "0.004800" || n.DurationMs == nil || *n.DurationMs != 100 || n.Provider != "anthropic" || n.Message != "an answer" {
		t.Errorf("node 1 = %+v", n)
	}
	if n := g.Nodes[1]; n.Tool != "echo" || n.Arguments != `{"id":"c0"}` || n.Result != "page text" || n.IdempotencyKey != IdempotencyKey(testRunID, 2) {
		t.Errorf("tool node = %+v", n)
	}
}

func TestGraphWithOneRecovery(t *testing.T) {
	g := BuildGraph(runLive, seq(
		modelStart(1, 1, "w-2"), modelDone(1, 1, "w-2", "", 0),
		tStart(2, 1, "w-2", "c0"), // w-2 stops here
		tRedo(2, 2, "w-4", "w-2", 1), tDone(2, 2, "w-4"),
		modelStart(3, 2, "w-4"),
	), nil, gBase.Add(time.Minute))
	eq(t, "nodes", brief(g),
		"1/model_call/w-2/e1/finished",
		"2/tool_call/w-2/e1/stopped",
		"2/tool_call/w-4/e2/finished+redo",
		"3/model_call/w-4/e2/running",
	)
	eq(t, "recoveries", rec(g), "after 2: w-2 -> w-4 e2")
	redo := g.Nodes[2]
	if redo.Tool != "echo" || redo.Arguments != `{"id":"c0"}` {
		t.Errorf("the re-issued attempt knows its tool and arguments from the attempt before: %+v", redo)
	}
	if redo.PreviousWorker != "w-2" || redo.PreviousEpoch != 1 || redo.IdempotencyKey != g.Nodes[1].IdempotencyKey || redo.IdempotencyKey == "" {
		t.Errorf("the re-issued attempt uses the same key and says who it follows: %+v", redo)
	}
	if g.Nodes[1].DurationMs != nil {
		t.Error("a stopped attempt has no duration: it never ended")
	}
	if len(g.Workers) != 2 || g.Workers[0].ID != "w-2" || g.Workers[1].ID != "w-4" || g.Workers[1].Epochs[0] != 2 {
		t.Errorf("workers = %+v", g.Workers)
	}
}

func TestGraphWithTwoRecoveriesAndAStepReissuedTwice(t *testing.T) {
	g := BuildGraph(runLive, seq(
		modelStart(1, 1, "w-1"), modelDone(1, 1, "w-1", "", 0),
		tStart(2, 1, "w-1", "c0"),
		tRedo(2, 2, "w-2", "w-1", 1),
		tRedo(2, 3, "w-3", "w-2", 2), tDone(2, 3, "w-3"),
		modelStart(3, 3, "w-3"), modelDone(3, 3, "w-3", "", 0),
	), nil, gBase.Add(time.Minute))
	eq(t, "nodes", brief(g),
		"1/model_call/w-1/e1/finished",
		"2/tool_call/w-1/e1/stopped",
		"2/tool_call/w-2/e2/stopped+redo",
		"2/tool_call/w-3/e3/finished+redo",
		"3/model_call/w-3/e3/finished",
	)
	eq(t, "recoveries", rec(g), "after 2: w-1 -> w-2 e2", "after 2: w-2 -> w-3 e3")
	if got := fmt.Sprint(g.Workers); got != "[{w-1 [1]} {w-2 [2]} {w-3 [3]}]" {
		t.Errorf("workers = %s", got)
	}
}

func TestGraphShowsAFallbackOnAModelCall(t *testing.T) {
	uses := map[string]UsageInfo{"u1": {Provider: "anthropic", Model: "sonnet", Cache: "miss", Attempts: []usage.Attempt{
		{Provider: "openai", Model: "mini", Kind: "primary", Status: 503, ErrorKind: "server", LatencyMs: 340},
		{Provider: "anthropic", Model: "sonnet", Kind: "fallback", LatencyMs: 2560},
	}}}
	g := BuildGraph(runLive, seq(modelStart(1, 1, "w-1"), modelDone(1, 1, "w-1", "u1", 7100)), uses, gBase)
	n := g.Nodes[0]
	if len(n.Attempts) != 2 || n.Attempts[0].Status != 503 || n.Attempts[1].Kind != "fallback" || n.Cache != "miss" || n.Model != "sonnet" {
		t.Errorf("node = %+v", n)
	}
	// An unknown usage id leaves the attempts empty rather than failing.
	g = BuildGraph(runLive, seq(modelStart(1, 1, "w-1"), modelDone(1, 1, "w-1", "missing", 0)), uses, gBase)
	if len(g.Nodes[0].Attempts) != 0 || g.Nodes[0].Model != "sonnet" {
		t.Errorf("node = %+v", g.Nodes[0])
	}
}

func TestGraphRunningAndFailedAndStoppedByTheEndOfTheRun(t *testing.T) {
	steps := seq(modelStart(1, 1, "w-1"), modelDone(1, 1, "w-1", "", 0), tStart(2, 1, "w-1", "c0"))
	live := BuildGraph(runLive, steps, nil, gBase.Add(5*time.Second))
	if n := live.Nodes[1]; n.State != NodeRunning || n.DurationMs == nil || *n.DurationMs != 4700 {
		t.Errorf("a step in flight runs against the clock: %+v", n)
	}
	cancelled := BuildGraph(GraphRun{ID: testRunID, Status: Cancelled}, steps, nil, gBase.Add(5*time.Second))
	if n := cancelled.Nodes[1]; n.State != NodeStopped {
		t.Errorf("a step open when the run ended is stopped: %+v", n)
	}
	failed := seq(modelStart(1, 1, "w-1"), row(0, 1, ModelCall, PhaseFailed, 1, "w-1", ErrorPayload{Error: "after 4 attempts: all providers failed"}, 0))
	g := BuildGraph(GraphRun{ID: testRunID, Status: Failed}, failed, nil, gBase)
	if n := g.Nodes[0]; n.State != NodeFailed || n.Error != "after 4 attempts: all providers failed" || n.DurationMs == nil {
		t.Errorf("failed step = %+v", n)
	}
}

func TestGraphIgnoresStatusRowsAndTruncatesBigPayloads(t *testing.T) {
	big := strings.Repeat("x", 5000)
	d := row(0, 1, ToolCall, PhaseFinished, 1, "w-1", ToolFinished{Result: big}, 0)
	g := BuildGraph(runLive, seq(
		row(0, 0, RunStatus, PhaseFinished, 0, "", StatusPayload{Status: Queued}, 0),
		row(0, 0, RunStatus, PhaseFinished, 1, "w-1", StatusPayload{Status: Running}, 0),
		toolStarted(0, 1, 1, "w-1", "c0"), d,
	), nil, gBase)
	if len(g.Nodes) != 1 || len(g.Recoveries) != 0 {
		t.Fatalf("status rows are not steps: %v / %v", brief(g), rec(g))
	}
	if r := []rune(g.Nodes[0].Result); len(r) != maxDetail+1 || r[len(r)-1] != '…' {
		t.Errorf("result is cut to %d characters: %d", maxDetail, len(r))
	}
	if g.LastEvent != 4 {
		t.Errorf("the event cursor counts every row: %d", g.LastEvent)
	}
	b, _ := json.Marshal(g)
	if !strings.Contains(string(b), `"attempts":[]`) || !strings.Contains(string(b), `"recoveries":[]`) {
		t.Errorf("empty lists are [] in JSON, not null: %s", b)
	}
}

func TestGraphShowsAWaitingApprovalAndDoesNotCallItARecovery(t *testing.T) {
	wait := row(0, 2, WaitHuman, PhaseStarted, 1, "w-1", WaitStarted{Reason: "Approval needed to call send_email", Tool: "send_email", Arguments: json.RawMessage(`{"to":"a@b.c"}`), ToolCallID: "c1"}, 0)
	steps := seq(modelStart(1, 1, "w-1"), modelDone(1, 1, "w-1", "u1", 1000), wait)
	g := BuildGraph(GraphRun{Status: WaitingHuman}, steps, nil, gBase.Add(time.Hour))
	eq(t, "waiting", brief(g), "1/model_call/w-1/e1/finished", "2/wait_human/w-1/e1/waiting")
	a := g.Run.Approval
	if a == nil || a.StepNo != 2 || a.Tool != "send_email" || !a.Gate || a.Arguments != `{"to":"a@b.c"}` || a.Reason == "" {
		t.Fatalf("approval = %+v", a)
	}

	// Decided by the API at the same epoch, then another worker (epoch 2) goes on: one node, no recovery.
	decided := row(0, 2, WaitHuman, PhaseFinished, 1, "api", Decision{Decision: "approve", By: "ana", Note: "ok"}, 0)
	next := row(0, 3, ToolCall, PhaseStarted, 2, "w-2", ToolStarted{Tool: "send_email", Arguments: json.RawMessage(`{}`)}, 0)
	g = BuildGraph(GraphRun{Status: Running}, seq(append(steps, decided, next)...), nil, gBase.Add(time.Hour))
	eq(t, "decided", brief(g), "1/model_call/w-1/e1/finished", "2/wait_human/w-1/e1/finished", "3/tool_call/w-2/e2/running")
	eq(t, "no recovery", rec(g))
	if n := g.Nodes[1]; n.Decision != "approve" || n.By != "ana" || n.Note != "ok" || !n.Gate {
		t.Errorf("node = %+v", n)
	}
	if g.Run.Approval != nil {
		t.Error("nothing is pending any more")
	}
}

func TestGraphSleepWokenByAnotherWorkerIsOneNode(t *testing.T) {
	wake := gBase.Add(10 * time.Minute)
	started := row(0, 2, Sleep, PhaseStarted, 1, "w-1", SleepStarted{Seconds: 600, WakeAt: wake, ToolCallID: "c1"}, 0)
	steps := seq(modelStart(1, 1, "w-1"), modelDone(1, 1, "w-1", "u1", 1000), started)
	g := BuildGraph(GraphRun{Status: Sleeping}, steps, nil, gBase.Add(time.Minute))
	eq(t, "sleeping", brief(g), "1/model_call/w-1/e1/finished", "2/sleep/w-1/e1/sleeping")
	if g.Run.WakeAt == nil || !g.Run.WakeAt.Equal(wake) || g.Nodes[1].Seconds != 600 {
		t.Errorf("wake = %v node = %+v", g.Run.WakeAt, g.Nodes[1])
	}
	done := row(0, 2, Sleep, PhaseFinished, 2, "w-2", struct{}{}, 0)
	g = BuildGraph(GraphRun{Status: Running}, seq(append(steps, done)...), nil, gBase.Add(time.Hour))
	eq(t, "woken", brief(g), "1/model_call/w-1/e1/finished", "2/sleep/w-1/e1/finished")
	eq(t, "no recovery", rec(g))
}
