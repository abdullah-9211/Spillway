package runs

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abdullah-9211/spillway/internal/money"
	"github.com/abdullah-9211/spillway/internal/provider"
)

func execute(t *testing.T, e *Engine, run Run, log *memLog) error {
	t.Helper()
	return e.Execute(context.Background(), run, log, log.all())
}

func TestRunWithToolsCompletes(t *testing.T) {
	m, tl := &scriptedModel{Calls: 2}, &echoTool{}
	run := testRun(t, req("echo"), Limits{})
	log := newMemLog(run.ID, 1, "w-1", nil)
	if err := execute(t, testEngine(m, tl), run, log); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"1 model_call started", "1 model_call finished", "2 tool_call started", "2 tool_call finished",
		"3 model_call started", "3 model_call finished", "4 tool_call started", "4 tool_call finished",
		"5 model_call started", "5 model_call finished", "run_status succeeded",
	}
	if got := outline(log.all()); !reflect.DeepEqual(got, want) {
		t.Errorf("steps =\n%s", strings.Join(got, "\n"))
	}
	st := Replay(run.Request, log.all())
	if st.Cost() != 3000 || st.StepCount() != 5 {
		t.Errorf("cost %d steps %d", st.Cost(), st.StepCount())
	}
	last := st.Messages()[len(st.Messages())-1]
	if last.Content.PlainText() != `done: echo:{"n":0},echo:{"n":1}` {
		t.Errorf("final answer = %q", last.Content.PlainText())
	}
	if len(tl.keys) != 2 || tl.keys[0] != IdempotencyKey(run.ID, 2) || tl.keys[1] != IdempotencyKey(run.ID, 4) || tl.keys[0] == tl.keys[1] {
		t.Errorf("idempotency keys = %v", tl.keys)
	}
	for _, s := range log.all() {
		if s.Type == ToolCall && s.Phase == PhaseStarted && s.IdempotencyKey != IdempotencyKey(run.ID, *s.StepNo) {
			t.Errorf("the key is recorded on the tool step: %q", s.IdempotencyKey)
		}
	}
}

func TestEveryLimitFailsTheRunWithItsReason(t *testing.T) {
	tests := []struct {
		name   string
		limits Limits
		model  *scriptedModel
		now    time.Time
		want   string
		steps  int // steps that ran before the trip
	}{
		{"max_steps", Limits{MaxSteps: 3, MaxCost: 1_000_000, Deadline: time.Hour}, &scriptedModel{Calls: 5}, time.Unix(1_700_000_000, 0), ReasonMaxSteps, 3},
		{"max_cost", Limits{MaxSteps: 50, MaxCost: 2500, Deadline: time.Hour}, &scriptedModel{Calls: 5, Cost: 1000}, time.Unix(1_700_000_000, 0), ReasonMaxCost, 5},
		{"deadline before the first step", Limits{MaxSteps: 50, MaxCost: 1_000_000, Deadline: time.Hour}, &scriptedModel{Calls: 5}, time.Unix(1_900_000_000, 0), ReasonDeadline, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run := testRun(t, req("echo"), tc.limits)
			e := testEngine(tc.model, &echoTool{})
			e.Now = func() time.Time { return tc.now }
			log := newMemLog(run.ID, 1, "w-1", nil)
			if err := execute(t, e, run, log); err != nil {
				t.Fatal(err)
			}
			fs := finalStatus(log.all())
			if fs.Status != Failed || fs.Reason != tc.want {
				t.Fatalf("final = %+v, want failed:%s", fs, tc.want)
			}
			if n := Replay(run.Request, log.all()).StepCount(); n != tc.steps {
				t.Errorf("%d steps ran, want %d", n, tc.steps)
			}
			for _, s := range log.all() {
				if s.Phase == PhaseStarted && s.Type != RunStatus {
					if open := Replay(run.Request, log.all()).Next(); open.Kind != Done {
						t.Errorf("a step is left open: %+v", open)
					}
				}
			}
		})
	}
}

func TestDeadlineDuringAModelCallClosesTheStepAndFailsTheRun(t *testing.T) {
	m := &scriptedModel{hook: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}
	run := testRun(t, req(), Limits{})
	run.DeadlineAt = time.Now().Add(50 * time.Millisecond)
	e := testEngine(m, nil)
	e.Now = time.Now
	log := newMemLog(run.ID, 1, "w-1", nil)
	if err := execute(t, e, run, log); err != nil {
		t.Fatal(err)
	}
	want := []string{"1 model_call started", "1 model_call failed", "run_status failed:deadline"}
	if got := outline(log.all()); !reflect.DeepEqual(got, want) {
		t.Errorf("steps = %v", got)
	}
}

func TestModelCallRetries(t *testing.T) {
	fail := &CallError{Kind: CallTransient, Err: errors.New("all providers failed")}
	t.Run("recovers after transient failures, in the same step and with a wait each time", func(t *testing.T) {
		m := &scriptedModel{fail: []error{fail, fail}}
		run := testRun(t, req(), Limits{})
		e := testEngine(m, nil)
		var waits []time.Duration
		e.Sleep = func(ctx context.Context, d time.Duration) error { waits = append(waits, d); return nil }
		log := newMemLog(run.ID, 1, "w-1", nil)
		if err := execute(t, e, run, log); err != nil {
			t.Fatal(err)
		}
		if fs := finalStatus(log.all()); fs.Status != Succeeded {
			t.Errorf("final = %+v", fs)
		}
		if m.count() != 3 || len(waits) != 2 || waits[1] <= waits[0] {
			t.Errorf("calls %d waits %v (backoff should grow)", m.count(), waits)
		}
		if n := Replay(run.Request, log.all()).StepCount(); n != 1 {
			t.Errorf("a retry is not a new step: %d", n)
		}
	})
	t.Run("gives up after three retries with provider_failed", func(t *testing.T) {
		m := &scriptedModel{fail: []error{fail, fail, fail, fail, fail}}
		run := testRun(t, req(), Limits{})
		log := newMemLog(run.ID, 1, "w-1", nil)
		if err := execute(t, testEngine(m, nil), run, log); err != nil {
			t.Fatal(err)
		}
		if fs := finalStatus(log.all()); fs.Status != Failed || fs.Reason != ReasonProviderFailed {
			t.Errorf("final = %+v", fs)
		}
		if m.count() != 4 {
			t.Errorf("1 attempt + 3 retries = 4 calls, got %d", m.count())
		}
		if got := outline(log.all()); got[1] != "1 model_call failed" {
			t.Errorf("the step is closed as failed: %v", got)
		}
	})
	t.Run("uses the provider's Retry-After when it is short", func(t *testing.T) {
		m := &scriptedModel{fail: []error{&CallError{Kind: CallTransient, RetryAfter: 2 * time.Second, Err: errors.New("429")}}}
		run := testRun(t, req(), Limits{})
		e := testEngine(m, nil)
		var waits []time.Duration
		e.Sleep = func(ctx context.Context, d time.Duration) error { waits = append(waits, d); return nil }
		_ = execute(t, e, run, newMemLog(run.ID, 1, "w-1", nil))
		if len(waits) != 1 || waits[0] != 2*time.Second {
			t.Errorf("waits = %v", waits)
		}
	})
	for name, tc := range map[string]struct {
		kind   CallErrorKind
		reason string
		detail string
	}{
		"an exhausted key budget":   {CallBudget, ReasonMaxCost, "key_budget"},
		"a revoked key":             {CallKeyRevoked, ReasonKeyRevoked, ""},
		"a request that is refused": {CallPermanent, ReasonProviderFailed, ""},
	} {
		t.Run(name+" fails at once without retrying", func(t *testing.T) {
			m := &scriptedModel{fail: []error{&CallError{Kind: tc.kind, Err: errors.New("no")}}}
			run := testRun(t, req(), Limits{})
			log := newMemLog(run.ID, 1, "w-1", nil)
			if err := execute(t, testEngine(m, nil), run, log); err != nil {
				t.Fatal(err)
			}
			fs := finalStatus(log.all())
			if fs.Status != Failed || fs.Reason != tc.reason || !strings.Contains(fs.Detail, tc.detail) || m.count() != 1 {
				t.Errorf("final %+v after %d calls", fs, m.count())
			}
		})
	}
}

func TestToolFailuresAreFedBackAndAFewInARowFailTheRun(t *testing.T) {
	t.Run("the model sees the error and recovers", func(t *testing.T) {
		tl := &echoTool{fail: []error{errors.New("upstream 500")}}
		run := testRun(t, req("echo"), Limits{})
		log := newMemLog(run.ID, 1, "w-1", nil)
		if err := execute(t, testEngine(&scriptedModel{Calls: 2}, tl), run, log); err != nil {
			t.Fatal(err)
		}
		if fs := finalStatus(log.all()); fs.Status != Succeeded {
			t.Fatalf("final = %+v", fs)
		}
		msgs := Replay(run.Request, log.all()).Messages()
		if got := msgs[2].Content.PlainText(); got != "error: upstream 500" {
			t.Errorf("tool message = %q", got)
		}
	})
	t.Run("three failures in a row fail the run with tool_failed", func(t *testing.T) {
		boom := errors.New("boom")
		tl := &echoTool{fail: []error{boom, boom, boom, boom}}
		run := testRun(t, req("echo"), Limits{})
		log := newMemLog(run.ID, 1, "w-1", nil)
		if err := execute(t, testEngine(&scriptedModel{Calls: 9}, tl), run, log); err != nil {
			t.Fatal(err)
		}
		if fs := finalStatus(log.all()); fs.Status != Failed || fs.Reason != ReasonToolFailed {
			t.Errorf("final = %+v", fs)
		}
		if len(tl.keys) != 3 {
			t.Errorf("tool calls = %d", len(tl.keys))
		}
	})
	t.Run("a tool outside the allowlist is refused and the refusal is fed back", func(t *testing.T) {
		tl := &echoTool{}
		run := testRun(t, req("other"), Limits{}) // the model asks for echo, which this run did not allow
		log := newMemLog(run.ID, 1, "w-1", nil)
		if err := execute(t, testEngine(&scriptedModel{Calls: 1}, tl), run, log); err != nil {
			t.Fatal(err)
		}
		if len(tl.keys) != 0 {
			t.Error("a disallowed tool must never be called")
		}
		msgs := Replay(run.Request, log.all()).Messages()
		if !strings.Contains(msgs[2].Content.PlainText(), "not allowed") {
			t.Errorf("tool message = %q", msgs[2].Content.PlainText())
		}
	})
	t.Run("with no tool runner the call fails the same way", func(t *testing.T) {
		run := testRun(t, req("echo"), Limits{})
		log := newMemLog(run.ID, 1, "w-1", nil)
		if err := execute(t, testEngine(&scriptedModel{Calls: 9}, nil), run, log); err != nil {
			t.Fatal(err)
		}
		if fs := finalStatus(log.all()); fs.Reason != ReasonToolFailed {
			t.Errorf("final = %+v", fs)
		}
	})
}

func TestCancelAndShutdown(t *testing.T) {
	t.Run("a cancel during a model call closes the step and cancels the run", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		m := &scriptedModel{hook: func(c context.Context) error { cancel(ErrCancelRequested); <-c.Done(); return c.Err() }}
		run := testRun(t, req(), Limits{})
		log := newMemLog(run.ID, 1, "w-1", nil)
		if err := testEngine(m, nil).Execute(ctx, run, log, nil); err != nil {
			t.Fatal(err)
		}
		want := []string{"1 model_call started", "1 model_call failed", "run_status cancelled:cancelled"}
		if got := outline(log.all()); !reflect.DeepEqual(got, want) {
			t.Errorf("steps = %v", got)
		}
	})
	t.Run("a cancel that was already pending ends the run before any step", func(t *testing.T) {
		run := testRun(t, req(), Limits{})
		run.CancelRequested = true
		log := newMemLog(run.ID, 1, "w-1", nil)
		m := &scriptedModel{}
		if err := testEngine(m, nil).Execute(context.Background(), run, log, nil); err != nil {
			t.Fatal(err)
		}
		if got := outline(log.all()); !reflect.DeepEqual(got, []string{"run_status cancelled:cancelled"}) || m.count() != 0 {
			t.Errorf("steps = %v, model calls %d", got, m.count())
		}
	})
	t.Run("a shutdown abandons the run and writes nothing after the step opened", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		m := &scriptedModel{hook: func(c context.Context) error { cancel(ErrShutdown); <-c.Done(); return c.Err() }}
		run := testRun(t, req(), Limits{})
		log := newMemLog(run.ID, 1, "w-1", nil)
		if err := testEngine(m, nil).Execute(ctx, run, log, nil); !errors.Is(err, ErrAbandoned) {
			t.Fatalf("err = %v, want ErrAbandoned", err)
		}
		if got := outline(log.all()); !reflect.DeepEqual(got, []string{"1 model_call started"}) {
			t.Errorf("steps = %v (the open step is what the next worker re-issues)", got)
		}
	})
	t.Run("a lost lease abandons the run", func(t *testing.T) {
		run := testRun(t, req(), Limits{})
		log := newMemLog(run.ID, 1, "w-1", nil)
		log.lost = true
		if err := testEngine(&scriptedModel{}, nil).Execute(context.Background(), run, log, nil); !errors.Is(err, ErrAbandoned) {
			t.Errorf("err = %v", err)
		}
		if len(log.all()) != 0 {
			t.Error("a zombie wrote a row")
		}
	})
	t.Run("a duplicate terminal row also abandons the run", func(t *testing.T) {
		run := testRun(t, req(), Limits{})
		prior := []Step{row(1, 1, ModelCall, PhaseStarted, 1, "w-1", ModelStarted{}, 0), modelOK(2, 1, 1, "w-1", text("x"))}
		log := newMemLog(run.ID, 2, "w-2", prior)
		// The state handed to the engine is stale: it believes step 1 is still open.
		err := testEngine(&scriptedModel{}, nil).Execute(context.Background(), run, log, prior[:1])
		if !errors.Is(err, ErrAbandoned) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestReissue(t *testing.T) {
	t.Run("a model call interrupted by a crash is re-issued under the new worker and epoch", func(t *testing.T) {
		run := testRun(t, req(), Limits{})
		prior := []Step{row(1, 1, ModelCall, PhaseStarted, 1, "w-1", ModelStarted{Policy: "default"}, 0)}
		log := newMemLog(run.ID, 2, "w-2", prior)
		if err := testEngine(&scriptedModel{}, nil).Execute(context.Background(), run, log, prior); err != nil {
			t.Fatal(err)
		}
		all := log.all()
		if got := outline(all); !reflect.DeepEqual(got, []string{"1 model_call started", "1 model_call reissued", "1 model_call finished", "run_status succeeded"}) {
			t.Fatalf("steps = %v", got)
		}
		re := all[1]
		if re.WorkerID != "w-2" || re.Epoch != 2 || !strings.Contains(string(re.Payload), `"previous_worker":"w-1"`) || !strings.Contains(string(re.Payload), `"previous_epoch":1`) {
			t.Errorf("reissued row = %+v %s", re, re.Payload)
		}
		if all[0].WorkerID != "w-1" || all[0].Epoch != 1 {
			t.Error("the first attempt stays as history")
		}
	})
	t.Run("a step interrupted twice has two reissued rows, one per epoch", func(t *testing.T) {
		run := testRun(t, req(), Limits{})
		log := newMemLog(run.ID, 1, "w-1", nil)
		for epoch := int64(1); epoch <= 3; epoch++ {
			log.epoch, log.worker = epoch, fmt.Sprintf("w-%d", epoch)
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			m := &scriptedModel{}
			if epoch < 3 { // the first two workers die with the call in flight
				m.hook = func(c context.Context) error { cancel(ErrShutdown); <-c.Done(); return c.Err() }
			}
			err := testEngine(m, nil).Execute(ctx, run, log, log.all())
			if epoch < 3 && !errors.Is(err, ErrAbandoned) {
				t.Fatalf("epoch %d: %v", epoch, err)
			}
			if epoch == 3 && err != nil {
				t.Fatal(err)
			}
		}
		want := []string{"1 model_call started", "1 model_call reissued", "1 model_call reissued", "1 model_call finished", "run_status succeeded"}
		if got := outline(log.all()); !reflect.DeepEqual(got, want) {
			t.Errorf("steps = %v", got)
		}
		if r := log.all()[2]; !strings.Contains(string(r.Payload), `"previous_worker":"w-2"`) || r.Epoch != 3 && r.Epoch != 2 {
			t.Errorf("the second reissue points at the previous writer: %s", r.Payload)
		}
	})
	t.Run("a tool re-issued after a crash sends the identical idempotency key", func(t *testing.T) {
		run := testRun(t, req("echo"), Limits{})
		log := newMemLog(run.ID, 1, "w-1", nil)
		tl := &echoTool{}
		ctx, cancel := context.WithCancelCause(context.Background())
		tl.hook = func(c context.Context, inv ToolInvocation) error { cancel(ErrShutdown); <-c.Done(); return c.Err() }
		if err := testEngine(&scriptedModel{Calls: 1}, tl).Execute(ctx, run, log, nil); !errors.Is(err, ErrAbandoned) {
			t.Fatal(err)
		}
		log.epoch, log.worker = 2, "w-2"
		tl.hook = nil
		if err := testEngine(&scriptedModel{Calls: 1}, tl).Execute(context.Background(), run, log, log.all()); err != nil {
			t.Fatal(err)
		}
		if len(tl.keys) != 2 || tl.keys[0] != tl.keys[1] {
			t.Errorf("keys = %v: a re-issued step must send the same key so the receiver can deduplicate", tl.keys)
		}
		if fs := finalStatus(log.all()); fs.Status != Succeeded {
			t.Errorf("final = %+v", fs)
		}
	})
}

// TestAnyPrefixOfARunResumesToTheSameResult is the property that makes replay trustworthy: take a run's rows, cut them
// off at any point (as if a worker died right there), and a fresh worker finishes it with the same conversation, the same
// final answer and the same step numbers as the uninterrupted run.
func TestAnyPrefixOfARunResumesToTheSameResult(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for trial := 0; trial < 40; trial++ {
		toolCalls := rng.Intn(5)
		withFailures := rng.Intn(2) == 0
		newTool := func() *echoTool {
			tl := &echoTool{}
			if withFailures {
				// A tool that fails on certain arguments, deterministically, so every replay agrees.
				tl.hook = func(_ context.Context, inv ToolInvocation) error {
					if strings.Contains(string(inv.Arguments), `"n":1`) {
						return errors.New("flaky")
					}
					return nil
				}
			}
			return tl
		}
		r := Request{System: "sys", Input: Input{Text: fmt.Sprintf("trial %d", trial)}, Tools: []string{"echo"}}
		run := testRun(t, r, Limits{})
		full := newMemLog(run.ID, 1, "w-1", nil)
		if err := testEngine(&scriptedModel{Calls: toolCalls}, newTool()).Execute(context.Background(), run, full, nil); err != nil {
			t.Fatal(err)
		}
		whole := full.all()
		wantMsgs := Replay(r, whole).Messages()
		wantStatus := finalStatus(whole)
		if wantStatus.Status != Succeeded {
			t.Fatalf("trial %d: the uninterrupted run ended %+v", trial, wantStatus)
		}

		for k := 0; k <= len(whole); k++ {
			prefix := whole[:k]
			if Replay(r, prefix).Terminal() != nil {
				continue // already finished: nothing to resume
			}
			log := newMemLog(run.ID, 2, "w-2", prefix)
			if err := testEngine(&scriptedModel{Calls: toolCalls}, newTool()).Execute(context.Background(), run, log, prefix); err != nil {
				t.Fatalf("trial %d prefix %d: %v", trial, k, err)
			}
			got := log.all()
			if gm := Replay(r, got).Messages(); !reflect.DeepEqual(msgsText(gm), msgsText(wantMsgs)) {
				t.Fatalf("trial %d prefix %d: conversation differs\n got %v\nwant %v", trial, k, msgsText(gm), msgsText(wantMsgs))
			}
			if fs := finalStatus(got); fs != wantStatus {
				t.Fatalf("trial %d prefix %d: final %+v, want %+v", trial, k, fs, wantStatus)
			}
			checkStepNumbers(t, got)
			if n := Replay(r, got).StepCount(); n != Replay(r, whole).StepCount() {
				t.Fatalf("trial %d prefix %d: %d logical steps, want %d (a re-issue must not add a step)", trial, k, n, Replay(r, whole).StepCount())
			}
		}
	}
}

func msgsText(ms []provider.Message) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Role+"|"+m.Content.PlainText()+"|"+m.ToolCallID+"|"+fmt.Sprint(len(m.ToolCalls)))
	}
	return out
}

// checkStepNumbers: step numbers are contiguous from 1, and every step has exactly one terminal row.
func checkStepNumbers(t *testing.T, steps []Step) {
	t.Helper()
	terminals := map[int]int{}
	maxNo := 0
	for _, s := range steps {
		if s.StepNo == nil {
			continue
		}
		maxNo = max(maxNo, *s.StepNo)
		if s.Phase.Terminal() {
			terminals[*s.StepNo]++
		}
	}
	for n := 1; n <= maxNo; n++ {
		if terminals[n] != 1 {
			t.Fatalf("step %d has %d terminal rows: %v", n, terminals[n], outline(steps))
		}
	}
}

func TestResumingFromAnyPrefixNeverStartsAStepBeyondALimit(t *testing.T) {
	// A run that was one step from its limit when its worker died must still stop at the limit, not run past it.
	r := req("echo")
	run := testRun(t, r, Limits{MaxSteps: 3, MaxCost: 1_000_000, Deadline: time.Hour})
	full := newMemLog(run.ID, 1, "w-1", nil)
	_ = testEngine(&scriptedModel{Calls: 9}, &echoTool{}).Execute(context.Background(), run, full, nil)
	whole := full.all()
	for k := 0; k <= len(whole); k++ {
		prefix := whole[:k]
		if Replay(r, prefix).Terminal() != nil {
			continue
		}
		log := newMemLog(run.ID, 2, "w-2", prefix)
		if err := testEngine(&scriptedModel{Calls: 9}, &echoTool{}).Execute(context.Background(), run, log, prefix); err != nil {
			t.Fatal(err)
		}
		if n := Replay(r, log.all()).StepCount(); n != 3 {
			t.Fatalf("prefix %d: %d steps, the limit is 3", k, n)
		}
		if fs := finalStatus(log.all()); fs.Reason != ReasonMaxSteps {
			t.Fatalf("prefix %d: %+v", k, fs)
		}
	}
}

var _ = money.Micros(0)
