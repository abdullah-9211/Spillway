package runs

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/money"
	"github.com/abdullah-9211/spillway/internal/provider"
)

var testRunID = uuid.MustParse("00000000-0000-7000-8000-000000000001")

// row builds a step row as a worker would have written it.
func row(id int64, no int, typ StepType, ph Phase, epoch int64, worker string, payload any, cost int64) Step {
	b, _ := json.Marshal(payload)
	s := Step{ID: id, RunID: testRunID, Type: typ, Phase: ph, Payload: b, Cost: money.Micros(cost), Epoch: epoch, WorkerID: worker}
	if typ != RunStatus {
		s.StepNo = ptr(no)
	}
	return s
}

func modelOK(id int64, no int, epoch int64, worker string, msg provider.Message) Step {
	return row(id, no, ModelCall, PhaseFinished, epoch, worker, ModelFinished{Message: msg, FinishReason: "stop"}, 1000)
}

func calls(ids ...string) provider.Message {
	m := provider.Message{Role: "assistant", Content: provider.Content{Null: true}}
	for _, id := range ids {
		m.ToolCalls = append(m.ToolCalls, provider.ToolCall{ID: id, Type: "function", Function: provider.FunctionCall{Name: "echo", Arguments: `{"id":"` + id + `"}`}})
	}
	return m
}

func text(s string) provider.Message {
	return provider.Message{Role: "assistant", Content: provider.TextContent(s)}
}

func toolStarted(id int64, no int, epoch int64, worker, callID string) Step {
	return row(id, no, ToolCall, PhaseStarted, epoch, worker, ToolStarted{Tool: "echo", Arguments: json.RawMessage(`{"id":"` + callID + `"}`), ToolCallID: callID}, 0)
}

func TestFrontier(t *testing.T) {
	two := calls("a", "b")
	tests := []struct {
		name  string
		steps []Step
		want  Frontier
		step  int
		call  string
		prev  string
		epoch int64
	}{
		{"a fresh run starts a model call", nil, StartModel, 1, "", "", 0},
		{"a started model call with no ending is re-issued", []Step{row(1, 1, ModelCall, PhaseStarted, 1, "w-1", ModelStarted{}, 0)}, ResumeModel, 1, "", "w-1", 1},
		{"a re-issued model call remembers its latest writer", []Step{
			row(1, 1, ModelCall, PhaseStarted, 1, "w-1", ModelStarted{}, 0),
			row(2, 1, ModelCall, PhaseReissued, 2, "w-2", Reissued{}, 0)}, ResumeModel, 1, "", "w-2", 2},
		{"a finished model call without tool calls is the end", []Step{
			row(1, 1, ModelCall, PhaseStarted, 1, "w-1", ModelStarted{}, 0), modelOK(2, 1, 1, "w-1", text("hi"))}, Succeed, 0, "", "", 0},
		{"a model call asking for two tools runs the first", []Step{
			row(1, 1, ModelCall, PhaseStarted, 1, "w-1", ModelStarted{}, 0), modelOK(2, 1, 1, "w-1", two)}, RunTool, 2, "a", "", 0},
		{"after the first tool the second runs", []Step{
			row(1, 1, ModelCall, PhaseStarted, 1, "w-1", ModelStarted{}, 0), modelOK(2, 1, 1, "w-1", two),
			toolStarted(3, 2, 1, "w-1", "a"), row(4, 2, ToolCall, PhaseFinished, 1, "w-1", ToolFinished{Result: "ok"}, 0)}, RunTool, 3, "b", "", 0},
		{"a tool that started and never ended is re-issued with its call", []Step{
			row(1, 1, ModelCall, PhaseStarted, 1, "w-1", ModelStarted{}, 0), modelOK(2, 1, 1, "w-1", two), toolStarted(3, 2, 1, "w-1", "a")}, ResumeTool, 2, "a", "w-1", 1},
		{"after both tools the model is called again", []Step{
			row(1, 1, ModelCall, PhaseStarted, 1, "w-1", ModelStarted{}, 0), modelOK(2, 1, 1, "w-1", two),
			toolStarted(3, 2, 1, "w-1", "a"), row(4, 2, ToolCall, PhaseFinished, 1, "w-1", ToolFinished{}, 0),
			toolStarted(5, 3, 1, "w-1", "b"), row(6, 3, ToolCall, PhaseFailed, 1, "w-1", ErrorPayload{Error: "x"}, 0)}, StartModel, 4, "", "", 0},
		{"a ended run is done", []Step{row(1, 0, RunStatus, PhaseFinished, 1, "w-1", StatusPayload{Status: Failed, Reason: ReasonMaxSteps}, 0)}, Done, 0, "", "", 0},
		{"a status row that is not final does not end the run", []Step{row(1, 0, RunStatus, PhaseFinished, 0, "", StatusPayload{Status: Running}, 0)}, StartModel, 1, "", "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := Replay(req("echo"), tc.steps).Next()
			if a.Kind != tc.want || a.StepNo != tc.step || a.Call.ID != tc.call || a.PreviousWorker != tc.prev || a.PreviousEpoch != tc.epoch {
				t.Errorf("Next() = %+v, want kind %v step %d call %q prev %q/%d", a, tc.want, tc.step, tc.call, tc.prev, tc.epoch)
			}
		})
	}
}

func TestMessagesAreRebuiltFromTheSteps(t *testing.T) {
	r := Request{System: "be brief", Input: Input{Text: "go"}, Tools: []string{"echo"}}
	steps := []Step{
		row(1, 1, ModelCall, PhaseStarted, 1, "w", ModelStarted{}, 0), modelOK(2, 1, 1, "w", calls("a", "b")),
		toolStarted(3, 2, 1, "w", "a"), row(4, 2, ToolCall, PhaseFinished, 1, "w", ToolFinished{Result: "A"}, 0),
		toolStarted(5, 3, 1, "w", "b"), row(6, 3, ToolCall, PhaseFailed, 1, "w", ErrorPayload{Error: "boom"}, 0),
	}
	msgs := Replay(r, steps).Messages()
	var got []string
	for _, m := range msgs {
		got = append(got, m.Role+":"+m.Content.PlainText()+":"+m.ToolCallID)
	}
	want := []string{"system:be brief:", "user:go:", "assistant::", "tool:A:a", "tool:error: boom:b"}
	if len(got) != len(want) {
		t.Fatalf("messages = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("message %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCountersFromTheSteps(t *testing.T) {
	steps := []Step{
		row(1, 1, ModelCall, PhaseStarted, 1, "w-1", ModelStarted{}, 0),
		row(2, 1, ModelCall, PhaseReissued, 2, "w-2", Reissued{}, 0), // a re-issue is the same step
		modelOK(3, 1, 2, "w-2", calls("a")),
		toolStarted(4, 2, 2, "w-2", "a"),
	}
	st := Replay(req("echo"), steps)
	if st.StepCount() != 2 || st.Cost() != 1000 || st.NextStepNo() != 3 {
		t.Errorf("steps %d cost %d next %d", st.StepCount(), st.Cost(), st.NextStepNo())
	}
}

func TestToolErrorsAreConsecutive(t *testing.T) {
	fail := func(id int64, no int, c string) []Step {
		return []Step{toolStarted(id, no, 1, "w", c), row(id+1, no, ToolCall, PhaseFailed, 1, "w", ErrorPayload{Error: "x"}, 0)}
	}
	ok := func(id int64, no int, c string) []Step {
		return []Step{toolStarted(id, no, 1, "w", c), row(id+1, no, ToolCall, PhaseFinished, 1, "w", ToolFinished{}, 0)}
	}
	steps := []Step{row(1, 1, ModelCall, PhaseStarted, 1, "w", ModelStarted{}, 0), modelOK(2, 1, 1, "w", calls("a", "b", "c", "d"))}
	steps = append(steps, fail(3, 2, "a")...)
	steps = append(steps, fail(5, 3, "b")...)
	if n := Replay(req("echo"), steps).ToolErrors(); n != 2 {
		t.Errorf("two failures in a row: %d", n)
	}
	steps = append(steps, ok(7, 4, "c")...)
	if n := Replay(req("echo"), steps).ToolErrors(); n != 0 {
		t.Errorf("a success resets the count: %d", n)
	}
	steps = append(steps, fail(9, 5, "d")...)
	if n := Replay(req("echo"), steps).ToolErrors(); n != 1 {
		t.Errorf("then one more: %d", n)
	}
}

func TestCompactionReplacesEarlierTurnsAndKeepsTheSystemPromptAndTheTask(t *testing.T) {
	r := Request{System: "sys", Input: Input{Text: "start"}}
	steps := []Step{
		row(1, 1, ModelCall, PhaseStarted, 1, "w", ModelStarted{}, 0), modelOK(2, 1, 1, "w", calls("a")),
		toolStarted(3, 2, 1, "w", "a"), row(4, 2, ToolCall, PhaseFinished, 1, "w", ToolFinished{Result: "R"}, 0),
		row(5, 3, ModelCall, PhaseStarted, 1, "w", ModelStarted{}, 0), modelOK(6, 3, 1, "w", calls("b")),
		row(7, 4, Compaction, PhaseStarted, 1, "w", CompactionStarted{ReplacesThroughStep: 2}, 0),
		row(8, 4, Compaction, PhaseFinished, 1, "w", CompactionFinished{Summary: "did a"}, 0),
	}
	var got []string
	for _, m := range Replay(r, steps).Messages() {
		got = append(got, m.Role+":"+m.Content.PlainText())
	}
	want := []string{"system:sys", "user:start", "user:Summary of the conversation so far:\ndid a", "assistant:"}
	if len(got) != len(want) {
		t.Fatalf("messages after compaction = %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("message %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestReplayIgnoresRowsWithoutAStepNumber(t *testing.T) {
	bad := Step{ID: 1, Type: ModelCall, Phase: PhaseStarted}
	if a := Replay(req(), []Step{bad}).Next(); a.Kind != StartModel {
		t.Errorf("a malformed row must not crash or block the run: %+v", a)
	}
}
