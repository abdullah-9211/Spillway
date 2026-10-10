package runs

import (
	"encoding/json"
	"fmt"

	"github.com/abdullah-9211/spillway/internal/money"
	"github.com/abdullah-9211/spillway/internal/provider"
)

// ModelFinished is the payload of a finished model_call.
type ModelFinished struct {
	Message      provider.Message `json:"message"`
	FinishReason string           `json:"finish_reason"`
	Usage        *provider.Usage  `json:"usage,omitempty"`
	Provider     string           `json:"provider"`
	Model        string           `json:"model"`
	UsageID      string           `json:"usage_id"`
}

// Frontier says what a worker does next.
type Frontier int

const (
	StartModel       Frontier = iota // nothing is pending: begin a new model call
	ResumeModel                      // a model call started and never finished: re-issue it
	RunTool                          // the model asked for a tool and it has not run yet
	ResumeTool                       // a tool call started and never finished: re-issue it with the same key
	ResumeOther                      // a step type this worker cannot resume
	Succeed                          // the last model call had no tool calls
	Done                             // the run already ended
	ResumeSleep                      // a sleep started and its time has come (or the run was claimed early)
	ParkHuman                        // a wait_human step is open: nobody may work on the run until it is decided
	ResumeCompaction                 // a compaction started and never finished
)

func (f Frontier) String() string {
	return [...]string{"start_model", "resume_model", "run_tool", "resume_tool", "resume_other", "succeed", "done", "resume_sleep", "park_human", "resume_compaction"}[f]
}

type Action struct {
	Kind   Frontier
	StepNo int               // the open step, or the number the next step will take
	Call   provider.ToolCall // RunTool and ResumeTool: the call to make
	// For the Resume kinds: who wrote the last started or reissued row of the open step.
	PreviousWorker string
	PreviousEpoch  int64
	// ResumeSleep and ParkHuman: the open step's payload. ResumeCompaction: how far the summary reaches.
	Sleep    SleepStarted
	Wait     WaitStarted
	Replaces int
}

type entry struct {
	msg  provider.Message
	step int // -1 pinned (the system prompt), 0 the input, otherwise the step that produced it
}

type stepInfo struct {
	no         int
	typ        StepType
	started    *Step
	lastWriter *Step
	terminal   *Step
	replaces   int // compaction: replaces_through_step
}

// State is a run as its steps describe it. It holds nothing a worker needs besides these rows, which is what makes
// a run resumable by any worker: Replay rebuilds it, then Next says what to do.
type State struct {
	entries    []entry
	steps      map[int]*stepInfo
	maxStep    int
	stepCount  int
	cost       money.Micros
	pending    []provider.ToolCall
	noCalls    bool // the last finished model call asked for no tools
	toolErrors int
	terminal   *StatusPayload
	approved   map[string]bool // tool call ids a person has approved
}

// Replay rebuilds a run's state from its steps, oldest first.
func Replay(req Request, steps []Step) *State {
	st := NewState(req)
	for _, s := range steps {
		st.Apply(s)
	}
	return st
}

func NewState(req Request) *State {
	st := &State{steps: map[int]*stepInfo{}, approved: map[string]bool{}}
	// What the caller gave (the system prompt and the input) is never compacted: step -1 marks it as always kept.
	for _, m := range req.InitialMessages() {
		st.entries = append(st.entries, entry{msg: m, step: -1})
	}
	return st
}

func (st *State) info(s Step) *stepInfo {
	no := *s.StepNo
	in := st.steps[no]
	if in == nil {
		in = &stepInfo{no: no, typ: s.Type}
		st.steps[no] = in
		if no > st.maxStep {
			st.maxStep = no
		}
	}
	return in
}

// Apply folds one step row into the state.
func (st *State) Apply(s Step) {
	if s.Type == RunStatus {
		if s.Phase == PhaseFinished {
			var p StatusPayload
			if json.Unmarshal(s.Payload, &p) == nil && p.Status.Terminal() {
				st.terminal = &p
			}
		}
		return
	}
	if s.StepNo == nil {
		return
	}
	in := st.info(s)
	switch s.Phase {
	case PhaseStarted:
		if in.started == nil {
			st.stepCount++
			in.started = &s
		}
		in.lastWriter = &s
		if s.Type == Compaction {
			var p CompactionStarted
			_ = json.Unmarshal(s.Payload, &p)
			in.replaces = p.ReplacesThroughStep
		}
	case PhaseReissued:
		in.lastWriter = &s
	case PhaseFinished, PhaseFailed:
		in.terminal = &s
		st.cost += s.Cost
		st.applyTerminal(in, s)
	}
}

func (st *State) applyTerminal(in *stepInfo, s Step) {
	switch in.typ {
	case ModelCall:
		if s.Phase != PhaseFinished {
			return
		}
		var p ModelFinished
		if json.Unmarshal(s.Payload, &p) != nil {
			return
		}
		st.entries = append(st.entries, entry{msg: p.Message, step: in.no})
		st.pending = append([]provider.ToolCall(nil), p.Message.ToolCalls...)
		st.noCalls = len(p.Message.ToolCalls) == 0
	case ToolCall:
		var call ToolStarted
		if in.started != nil {
			_ = json.Unmarshal(in.started.Payload, &call)
		}
		text := ""
		if s.Phase == PhaseFinished {
			var p ToolFinished
			_ = json.Unmarshal(s.Payload, &p)
			text = p.Result
			st.toolErrors = 0
		} else {
			var p ErrorPayload
			_ = json.Unmarshal(s.Payload, &p)
			text = "error: " + p.Error // fed back so the model can recover
			st.toolErrors++
		}
		st.entries = append(st.entries, entry{msg: provider.Message{Role: "tool", ToolCallID: call.ToolCallID, Content: provider.TextContent(text)}, step: in.no})
		for i, c := range st.pending {
			if c.ID == call.ToolCallID {
				st.pending = append(st.pending[:i:i], st.pending[i+1:]...)
				break
			}
		}
		st.noCalls = false
	case Sleep:
		var started SleepStarted
		if in.started != nil {
			_ = json.Unmarshal(in.started.Payload, &started)
		}
		text := fmt.Sprintf("Slept for %d seconds.", started.Seconds)
		if s.Phase == PhaseFailed {
			var p ErrorPayload
			_ = json.Unmarshal(s.Payload, &p)
			text = "error: " + p.Error
		}
		st.answerCall(in.no, started.ToolCallID, text)
	case WaitHuman:
		var started WaitStarted
		if in.started != nil {
			_ = json.Unmarshal(in.started.Payload, &started)
		}
		if s.Phase == PhaseFailed {
			return
		}
		var d Decision
		_ = json.Unmarshal(s.Payload, &d)
		if d.Decision != "approve" {
			return // a rejection ends the run; the status row says so
		}
		if started.Builtin {
			text := "Approved by " + d.By + "."
			if d.Note != "" {
				text += " Note: " + d.Note
			}
			st.answerCall(in.no, started.ToolCallID, text)
		} else {
			st.approved[started.ToolCallID] = true // the tool call itself still runs
		}
	case Compaction:
		if s.Phase != PhaseFinished {
			return
		}
		var p CompactionFinished
		_ = json.Unmarshal(s.Payload, &p)
		kept := []entry{}
		for _, e := range st.entries {
			if e.step == -1 || e.step > in.replaces {
				kept = append(kept, e)
			}
		}
		summary := entry{msg: provider.Message{Role: "user", Content: provider.TextContent("Summary of the conversation so far:\n" + p.Summary)}, step: 0}
		var out []entry
		inserted := false
		for _, e := range kept {
			if !inserted && e.step != -1 {
				out = append(out, summary)
				inserted = true
			}
			out = append(out, e)
		}
		if !inserted {
			out = append(out, summary)
		}
		st.entries = out
	}
}

// answerCall adds the tool message that answers a built-in call, and takes the call off the pending list.
func (st *State) answerCall(step int, callID, text string) {
	st.entries = append(st.entries, entry{msg: provider.Message{Role: "tool", ToolCallID: callID, Content: provider.TextContent(text)}, step: step})
	for i, c := range st.pending {
		if c.ID == callID {
			st.pending = append(st.pending[:i:i], st.pending[i+1:]...)
			break
		}
	}
	st.noCalls = false
}

// Approved reports whether a person has approved this tool call.
func (st *State) Approved(callID string) bool { return st.approved[callID] }

// EstimateTokens is a rough size of a conversation: four characters to a token. It decides when to compact, nothing more.
func EstimateTokens(msgs []provider.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content.PlainText())
		for _, c := range m.ToolCalls {
			n += len(c.Function.Name) + len(c.Function.Arguments)
		}
		n += 16 // role and framing
	}
	return n / 4
}

// CompactionPlan says what a compaction would replace: everything before the last keepLast model turns. through is the
// last step number covered, and msgs the messages to summarise. ok is false when there is too little to be worth it.
func (st *State) CompactionPlan(keepLast int) (through int, msgs []provider.Message, ok bool) {
	var assistants []int
	for i, e := range st.entries {
		if e.msg.Role == "assistant" && e.step > 0 {
			assistants = append(assistants, i)
		}
	}
	if keepLast < 1 {
		keepLast = 1
	}
	if len(assistants) <= keepLast {
		return 0, nil, false
	}
	first := st.entries[assistants[len(assistants)-keepLast]]
	through = first.step - 1
	for _, e := range st.entries {
		if e.step != -1 && e.step <= through {
			msgs = append(msgs, e.msg)
		}
	}
	// Folding a lone message into a summary gains nothing, and a summary of a summary is a loop.
	return through, msgs, len(msgs) >= 3
}

// Messages is the conversation to send to the model next.
func (st *State) Messages() []provider.Message {
	out := make([]provider.Message, len(st.entries))
	for i, e := range st.entries {
		out[i] = e.msg
	}
	return out
}

func (st *State) StepCount() int           { return st.stepCount }
func (st *State) Cost() money.Micros       { return st.cost }
func (st *State) ToolErrors() int          { return st.toolErrors }
func (st *State) NextStepNo() int          { return st.maxStep + 1 }
func (st *State) Terminal() *StatusPayload { return st.terminal }

// Next is the frontier: what a worker does now. The rules are section 4.4 of the technical design.
func (st *State) Next() Action {
	if st.terminal != nil {
		return Action{Kind: Done}
	}
	var last *stepInfo
	for _, in := range st.steps {
		if last == nil || in.no > last.no {
			last = in
		}
	}
	if last != nil && last.terminal == nil {
		a := Action{StepNo: last.no}
		if w := last.lastWriter; w != nil {
			a.PreviousWorker, a.PreviousEpoch = w.WorkerID, w.Epoch
		}
		switch last.typ {
		case ModelCall:
			a.Kind = ResumeModel
		case ToolCall:
			a.Kind = ResumeTool
			var p ToolStarted
			if last.started != nil {
				_ = json.Unmarshal(last.started.Payload, &p)
			}
			a.Call = provider.ToolCall{ID: p.ToolCallID, Type: "function", Function: provider.FunctionCall{Name: p.Tool, Arguments: string(p.Arguments)}}
		case Sleep:
			a.Kind = ResumeSleep
			if last.started != nil {
				_ = json.Unmarshal(last.started.Payload, &a.Sleep)
			}
		case WaitHuman:
			a.Kind = ParkHuman
			if last.started != nil {
				_ = json.Unmarshal(last.started.Payload, &a.Wait)
			}
		case Compaction:
			a.Kind = ResumeCompaction
			a.Replaces = last.replaces
		default:
			a.Kind = ResumeOther
		}
		return a
	}
	if len(st.pending) > 0 {
		return Action{Kind: RunTool, StepNo: st.NextStepNo(), Call: st.pending[0]}
	}
	if last != nil && last.typ == ModelCall && last.terminal.Phase == PhaseFinished && st.noCalls {
		return Action{Kind: Succeed}
	}
	return Action{Kind: StartModel, StepNo: st.NextStepNo()}
}
