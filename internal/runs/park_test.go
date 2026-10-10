package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/money"
	"github.com/abdullah-9211/spillway/internal/provider"
)

// fnModel answers from a function of the conversation, one assistant turn at a time.
type fnModel struct {
	mu    sync.Mutex
	turns func(assistant int, msgs []provider.Message) provider.Message
	seen  [][]provider.Message
	tools [][]provider.Tool
}

func (m *fnModel) Call(_ context.Context, _ Run, msgs []provider.Message, tools []provider.Tool, _ uuid.UUID) (ModelResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen = append(m.seen, msgs)
	m.tools = append(m.tools, tools)
	n := 0
	for _, msg := range msgs {
		if msg.Role == "assistant" {
			n++
		}
	}
	msg := m.turns(n, msgs)
	fr := "stop"
	if len(msg.ToolCalls) > 0 {
		fr = "tool_calls"
	}
	return ModelResult{Message: msg, Provider: "fake", Model: "m", FinishReason: fr, Cost: 1000}, nil
}

func toolCall(id, name, args string) provider.Message {
	return provider.Message{Role: "assistant", Content: provider.Content{Null: true},
		ToolCalls: []provider.ToolCall{{ID: id, Type: "function", Function: provider.FunctionCall{Name: name, Arguments: args}}}}
}

func lastToolText(msgs []provider.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "tool" {
			return msgs[i].Content.PlainText()
		}
	}
	return ""
}

// decide appends what Store.Decide writes, so the in-memory log can be resumed.
func decide(t *testing.T, log *memLog, d Decision) {
	t.Helper()
	var no *int
	for _, s := range log.all() {
		if s.Type == WaitHuman && s.Phase == PhaseStarted {
			no = s.StepNo
		}
	}
	if no == nil {
		t.Fatal("no wait_human step to decide")
	}
	if _, err := log.Append(context.Background(), NewStep{StepNo: no, Type: WaitHuman, Phase: PhaseFinished, Payload: d}); err != nil {
		t.Fatal(err)
	}
	if d.Decision == "reject" {
		_, _ = log.Append(context.Background(), NewStep{Type: RunStatus, Phase: PhaseFinished, Payload: StatusPayload{Status: Failed, Reason: ReasonRejected}})
	}
}

func TestApprovalBuiltinParksThenResumesWithTheDecision(t *testing.T) {
	m := &fnModel{turns: func(n int, msgs []provider.Message) provider.Message {
		if n == 0 {
			return toolCall("c0", BuiltinApproval, `{"reason":"delete the table?"}`)
		}
		return provider.Message{Role: "assistant", Content: provider.TextContent("answer: " + lastToolText(msgs))}
	}}
	run := testRun(t, req(), Limits{})
	log := newMemLog(run.ID, 1, "w-1", nil)
	e := testEngine(m, &echoTool{})
	if err := execute(t, e, run, log); !errors.Is(err, ErrParked) {
		t.Fatalf("first execution = %v, want parked", err)
	}
	got := outline(log.all())
	if last := got[len(got)-1]; last != "2 wait_human started" {
		t.Fatalf("last row = %q (%v)", last, got)
	}
	var w WaitStarted
	for _, s := range log.all() {
		if s.Type == WaitHuman {
			_ = json.Unmarshal(s.Payload, &w)
		}
	}
	if !w.Builtin || w.Reason != "delete the table?" || w.ToolCallID != "c0" {
		t.Errorf("wait payload = %+v", w)
	}
	if calls := len(m.seen); calls != 1 {
		t.Errorf("model calls while parked = %d", calls)
	}

	decide(t, log, Decision{Decision: "approve", By: "ana", Note: "go ahead"})
	if err := execute(t, e, run, log); err != nil {
		t.Fatal(err)
	}
	st := Replay(run.Request, log.all())
	final := st.Messages()[len(st.Messages())-1].Content.PlainText()
	if final != "answer: Approved by ana. Note: go ahead" {
		t.Errorf("final = %q", final)
	}
}

func TestRejectEndsTheRunAsRejected(t *testing.T) {
	m := &fnModel{turns: func(int, []provider.Message) provider.Message {
		return toolCall("c0", BuiltinApproval, `{"reason":"x"}`)
	}}
	run := testRun(t, req(), Limits{})
	log := newMemLog(run.ID, 1, "w-1", nil)
	e := testEngine(m, nil)
	if err := execute(t, e, run, log); !errors.Is(err, ErrParked) {
		t.Fatal(err)
	}
	decide(t, log, Decision{Decision: "reject", By: "ana", Note: "no"})
	if err := execute(t, e, run, log); err != nil {
		t.Fatalf("resume of a rejected run = %v", err)
	}
	if st := Replay(run.Request, log.all()); st.Terminal() == nil {
		t.Error("run should be terminal")
	}
	if len(m.seen) != 1 {
		t.Errorf("model called %d times after the reject", len(m.seen))
	}
}

func TestGatedToolRunsOnlyAfterApprovalAndExactlyOnce(t *testing.T) {
	m := &fnModel{turns: func(n int, msgs []provider.Message) provider.Message {
		if n == 0 {
			return toolCall("c0", "send_email", `{"to":"a@b.c"}`)
		}
		return provider.Message{Role: "assistant", Content: provider.TextContent(lastToolText(msgs))}
	}}
	tl := &echoTool{}
	run := testRun(t, Request{Input: Input{Text: "x"}, Tools: []string{"send_email"}, ApprovalRequired: []string{"send_email"}}, Limits{})
	log := newMemLog(run.ID, 1, "w-1", nil)
	e := testEngine(m, tl)
	if err := execute(t, e, run, log); !errors.Is(err, ErrParked) {
		t.Fatal(err)
	}
	if len(tl.keys) != 0 {
		t.Fatal("the tool ran before approval")
	}
	var w WaitStarted
	for _, s := range log.all() {
		if s.Type == WaitHuman {
			_ = json.Unmarshal(s.Payload, &w)
		}
	}
	if w.Builtin || w.Tool != "send_email" || string(w.Arguments) != `{"to":"a@b.c"}` {
		t.Errorf("gate payload = %+v", w)
	}
	decide(t, log, Decision{Decision: "approve", By: "ana"})
	if err := execute(t, e, run, log); err != nil {
		t.Fatal(err)
	}
	if len(tl.keys) != 1 {
		t.Errorf("tool calls = %d, want 1", len(tl.keys))
	}
	// A crash after the approval and before the tool ran must not ask again: replay the same log once more.
	if err := execute(t, e, run, newMemLog(run.ID, 2, "w-2", log.all()[:5])); err != nil && !errors.Is(err, ErrParked) {
		t.Fatal(err)
	}
}

func TestSleepParksAndWakes(t *testing.T) {
	m := &fnModel{turns: func(n int, msgs []provider.Message) provider.Message {
		if n == 0 {
			return toolCall("c0", BuiltinSleep, `{"seconds":300}`)
		}
		return provider.Message{Role: "assistant", Content: provider.TextContent(lastToolText(msgs))}
	}}
	run := testRun(t, req(), Limits{})
	log := newMemLog(run.ID, 1, "w-1", nil)
	now := time.Unix(1_700_000_000, 0).UTC()
	e := testEngine(m, nil)
	e.Now = func() time.Time { return now }
	if err := execute(t, e, run, log); !errors.Is(err, ErrParked) {
		t.Fatalf("got %v", err)
	}
	var s SleepStarted
	for _, row := range log.all() {
		if row.Type == Sleep {
			_ = json.Unmarshal(row.Payload, &s)
		}
	}
	if s.Seconds != 300 || !s.WakeAt.Equal(now.Add(300*time.Second)) {
		t.Fatalf("sleep payload = %+v", s)
	}
	// Woken early: parks again, no model call.
	if err := execute(t, e, run, log); !errors.Is(err, ErrParked) {
		t.Fatalf("early wake = %v", err)
	}
	now = now.Add(301 * time.Second)
	if err := execute(t, e, run, log); err != nil {
		t.Fatal(err)
	}
	st := Replay(run.Request, log.all())
	if got := st.Messages()[len(st.Messages())-1].Content.PlainText(); got != "Slept for 300 seconds." {
		t.Errorf("final = %q", got)
	}
}

func TestSleepRejectsBadDurationsAndFeedsBackTheError(t *testing.T) {
	for _, args := range []string{`{"seconds":0}`, `{"seconds":999999}`, `{}`} {
		m := &fnModel{turns: func(n int, msgs []provider.Message) provider.Message {
			if n == 0 {
				return toolCall("c0", BuiltinSleep, args)
			}
			return provider.Message{Role: "assistant", Content: provider.TextContent(lastToolText(msgs))}
		}}
		run := testRun(t, req(), Limits{})
		log := newMemLog(run.ID, 1, "w-1", nil)
		if err := execute(t, testEngine(m, nil), run, log); err != nil {
			t.Fatalf("%s: %v", args, err)
		}
		st := Replay(run.Request, log.all())
		if got := st.Messages()[len(st.Messages())-1].Content.PlainText(); !strings.HasPrefix(got, "error: seconds must be") {
			t.Errorf("%s: final = %q", args, got)
		}
	}
}

func TestBuiltinsAreOfferedAndDottedNamesResolve(t *testing.T) {
	tl := &echoTool{}
	m := &fnModel{turns: func(n int, msgs []provider.Message) provider.Message {
		switch n {
		case 0:
			return toolCall("c0", "files__read", `{"p":1}`)
		case 1:
			return toolCall("c1", "not_listed", `{}`)
		}
		return provider.Message{Role: "assistant", Content: provider.TextContent("ok")}
	}}
	var seen []string
	tl.hook = func(_ context.Context, inv ToolInvocation) error { seen = append(seen, inv.Name); return nil }
	run := testRun(t, req("files.read"), Limits{})
	log := newMemLog(run.ID, 1, "w-1", nil)
	if err := execute(t, testEngine(m, tl), run, log); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "files.read" {
		t.Errorf("registered name passed to the runner = %v", seen)
	}
	names := map[string]bool{}
	for _, tool := range m.tools[0] {
		names[tool.Function.Name] = true
	}
	if !names[BuiltinSleep] || !names[BuiltinApproval] {
		t.Errorf("built-ins not offered: %v", names)
	}
	st := Replay(run.Request, log.all())
	var rejected bool
	for _, msg := range st.Messages() {
		if msg.Role == "tool" && strings.Contains(msg.Content.PlainText(), "not allowed") {
			rejected = true
		}
	}
	if !rejected {
		t.Error("a tool outside the allowlist was not refused")
	}
}

// longModel keeps calling echo until `turns` assistant turns, then summarises; the conversation grows fat.
type fakeCompactor struct {
	mu    sync.Mutex
	calls int
	fail  bool
	seen  []int
}

func (f *fakeCompactor) Summarise(_ context.Context, _ Run, msgs []provider.Message) (string, money.Micros, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.seen = append(f.seen, len(msgs))
	if f.fail {
		return "", 0, errors.New("summariser down")
	}
	return fmt.Sprintf("SUMMARY of %d messages", len(msgs)), 500, nil
}

func fatRun(t *testing.T) (*fnModel, Run) {
	t.Helper()
	big := strings.Repeat("x", 4000)
	m := &fnModel{turns: func(n int, msgs []provider.Message) provider.Message {
		if n < 12 {
			return toolCall(fmt.Sprintf("c%d", n), "echo", fmt.Sprintf(`{"n":%d,"pad":"%s"}`, n, big))
		}
		return provider.Message{Role: "assistant", Content: provider.TextContent("finished")}
	}}
	r := testRun(t, Request{Input: Input{Text: "go"}, Tools: []string{"echo"}, Compaction: &CompactionConfig{ThresholdTokens: 3000, KeepLastTurns: 3}}, Limits{MaxSteps: 200, MaxCost: 10_000_000, Deadline: time.Hour})
	return m, r
}

func TestCompactionFoldsOldTurnsAndReplayGivesTheSameHistory(t *testing.T) {
	m, run := fatRun(t)
	c := &fakeCompactor{}
	e := testEngine(m, &echoTool{})
	e.Compactor = c
	log := newMemLog(run.ID, 1, "w-1", nil)
	if err := execute(t, e, run, log); err != nil {
		t.Fatal(err)
	}
	if c.calls == 0 {
		t.Fatal("compaction never ran")
	}
	steps := log.all()
	var started, finished int
	for _, s := range steps {
		if s.Type == Compaction && s.Phase == PhaseStarted {
			started++
		}
		if s.Type == Compaction && s.Phase == PhaseFinished {
			finished++
		}
	}
	if started != c.calls || finished != c.calls {
		t.Errorf("compaction rows started=%d finished=%d calls=%d", started, finished, c.calls)
	}
	a := msgsText(Replay(run.Request, steps).Messages())
	b := msgsText(Replay(run.Request, append([]Step(nil), steps...)).Messages())
	if strings.Join(a, "|") != strings.Join(b, "|") {
		t.Error("replay is not deterministic")
	}
	joined := strings.Join(a, "\n")
	if !strings.Contains(joined, "SUMMARY of") {
		t.Error("the summary is not in the history")
	}
	// The last model call saw far fewer messages than a run without compaction would have sent.
	if got, full := len(m.seen[len(m.seen)-1]), 2*12+2; got >= full {
		t.Errorf("last call saw %d messages, uncompacted would be %d", got, full)
	}
	// Cost of the compaction is on its step.
	if st := Replay(run.Request, steps); st.Cost() < money.Micros(500*c.calls) {
		t.Errorf("cost %d misses the compaction", st.Cost())
	}
}

func TestResumeInTheMiddleOfCompactionFinishesIt(t *testing.T) {
	m, run := fatRun(t)
	c := &fakeCompactor{}
	e := testEngine(m, &echoTool{})
	e.Compactor = c
	log := newMemLog(run.ID, 1, "w-1", nil)
	if err := execute(t, e, run, log); err != nil {
		t.Fatal(err)
	}
	all := log.all()
	cut := -1
	for i, s := range all {
		if s.Type == Compaction && s.Phase == PhaseStarted {
			cut = i + 1
			break
		}
	}
	if cut < 0 {
		t.Fatal("no compaction")
	}
	c2 := &fakeCompactor{}
	e.Compactor = c2
	resumed := newMemLog(run.ID, 2, "w-2", all[:cut])
	if err := execute(t, e, run, resumed); err != nil {
		t.Fatal(err)
	}
	if c2.calls == 0 {
		t.Error("the open compaction was not redone")
	}
	var reissued bool
	for _, s := range resumed.all() {
		if s.Type == Compaction && s.Phase == PhaseReissued {
			reissued = true
		}
	}
	if !reissued {
		t.Error("expected a reissued compaction row")
	}
}

func TestCompactionFailureDoesNotStopOrLoopTheRun(t *testing.T) {
	m, run := fatRun(t)
	c := &fakeCompactor{fail: true}
	e := testEngine(m, &echoTool{})
	e.Compactor = c
	log := newMemLog(run.ID, 1, "w-1", nil)
	if err := execute(t, e, run, log); err != nil {
		t.Fatal(err)
	}
	if c.calls != 1 {
		t.Errorf("compactor called %d times after failing, want 1", c.calls)
	}
	if Replay(run.Request, log.all()).Terminal() == nil {
		t.Error("run did not finish")
	}
	var failed bool
	for _, s := range log.all() {
		if s.Type == Compaction && s.Phase == PhaseFailed {
			failed = true
		}
	}
	if !failed {
		t.Error("no failed compaction row")
	}
}
