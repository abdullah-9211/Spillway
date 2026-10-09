package runs

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func sum(no int, typ StepType, done, failed bool) stepSummary {
	return stepSummary{No: no, Type: typ, Done: done, Failed: failed}
}

func render(s Strip) string {
	var b strings.Builder
	for _, n := range s.Steps {
		c := "m"
		if n.Kind == "tool" {
			c = "t"
		}
		switch n.State {
		case "current":
			c += "*"
		case "failed":
			c += "!"
		case "waiting":
			c += "?"
		}
		b.WriteString(c + " ")
	}
	if s.More > 0 {
		b.WriteString(fmt.Sprintf("+%d", s.More))
	}
	return strings.TrimSpace(b.String())
}

func TestStripMapping(t *testing.T) {
	tests := []struct {
		name   string
		steps  []stepSummary
		status Status
		want   string
	}{
		{"nothing yet", nil, Queued, ""},
		{"a live run's open step is current", []stepSummary{sum(1, ModelCall, true, false), sum(2, ToolCall, true, false), sum(3, ModelCall, false, false)}, Running, "m t m*"},
		{"a person-wait is waiting", []stepSummary{sum(1, ModelCall, true, false), sum(2, WaitHuman, false, false)}, WaitingHuman, "m t?"},
		{"sleep and wait are tool-shaped", []stepSummary{sum(1, Sleep, true, false), sum(2, Compaction, true, false)}, Running, "t m"},
		{"a failed step is failed", []stepSummary{sum(1, ModelCall, true, false), sum(2, ToolCall, false, true), sum(3, ModelCall, true, false)}, Running, "m t! m"},
		{"a succeeded run is all done", []stepSummary{sum(1, ModelCall, true, false), sum(2, ModelCall, true, false)}, Succeeded, "m m"},
		{"a failed run ends on a failed mark even if its last step finished", []stepSummary{sum(1, ModelCall, true, false), sum(2, ToolCall, true, false)}, Failed, "m t!"},
		{"a failed run keeps an earlier failed step as it is", []stepSummary{sum(1, ToolCall, false, true), sum(2, ModelCall, true, false)}, Failed, "t! m!"},
		{"a cancelled run's open step shows failed, finished ones stay done", []stepSummary{sum(1, ModelCall, true, false), sum(2, ToolCall, false, false)}, Cancelled, "m t!"},
		{"a cancelled run with nothing open is untouched", []stepSummary{sum(1, ModelCall, true, false)}, Cancelled, "m"},
	}
	for _, tc := range tests {
		if got := render(buildStrip(tc.steps, tc.status)); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestLongRunsShowTheirLatestSteps(t *testing.T) {
	var steps []stepSummary
	for i := 1; i <= 40; i++ {
		typ := ModelCall
		if i%2 == 0 {
			typ = ToolCall
		}
		steps = append(steps, sum(i, typ, i < 40, false))
	}
	s := buildStrip(steps, Running)
	if len(s.Steps) != StripLen || s.More != 24 {
		t.Fatalf("%d steps shown, %d hidden", len(s.Steps), s.More)
	}
	if last := s.Steps[len(s.Steps)-1]; last.State != "current" || last.Kind != "tool" {
		t.Errorf("the latest step is shown last and current: %+v", last)
	}
	if exact := buildStrip(steps[:StripLen], Succeeded); len(exact.Steps) != StripLen || exact.More != 0 {
		t.Errorf("exactly the strip length hides nothing: %d / %d", len(exact.Steps), exact.More)
	}
}

func TestGoalIsTheTaskInOneLine(t *testing.T) {
	goal := func(r Request) string { b, _ := json.Marshal(r); return goalOf(b) }
	if g := goal(Request{Input: Input{Text: "  Research   X\nand email me  "}}); g != "Research X and email me" {
		t.Errorf("whitespace is collapsed: %q", g)
	}
	long := strings.Repeat("word ", 100)
	if g := goal(Request{Input: Input{Text: long}}); len([]rune(g)) != 160 || !strings.HasSuffix(g, "...") {
		t.Errorf("long goals are cut: %d %q", len([]rune(g)), g[len(g)-6:])
	}
	if g := goal(Request{Input: Input{Text: strings.Repeat("é", 200)}}); !strings.HasSuffix(g, "...") || len([]rune(g)) != 160 {
		t.Errorf("cut on runes, not bytes: %d", len([]rune(g)))
	}
	msgs := parse(t, `{"input":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"the real ask"}]}`)
	if g := goal(msgs); g != "the real ask" {
		t.Errorf("a conversation's goal is its last user message: %q", g)
	}
	if goalOf([]byte("not json")) != "" {
		t.Error("unreadable request gives no goal")
	}
}

func TestBucketSizes(t *testing.T) {
	for hours, want := range map[int]time.Duration{1: 5 * time.Minute, 24: time.Hour, 168: 24 * time.Hour} {
		if got := BucketFor(hours); got != want || int(time.Duration(hours)*time.Hour/got) != map[int]int{1: 12, 24: 24, 168: 7}[hours] {
			t.Errorf("%dh: bucket %v", hours, got)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	c := Cursor{At: time.Date(2026, 10, 9, 12, 30, 45, 123456000, time.UTC), ID: uuid.New()}
	got, err := DecodeCursor(EncodeCursor(c))
	if err != nil || !got.At.Equal(c.At) || got.ID != c.ID {
		t.Fatalf("%+v %v", got, err)
	}
	for _, bad := range []string{"", "!!!", "bm90YSBjdXJzb3I", EncodeCursor(c)[:10]} {
		if _, err := DecodeCursor(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}
