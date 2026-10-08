package provider

import (
	"encoding/json"
	"testing"
)

func TestContentRoundTrip(t *testing.T) {
	tests := []struct{ name, in, out string }{
		{"string", `"hi"`, `"hi"`},
		{"null", `null`, `null`},
		{"parts", `[{"type":"text","text":"a"}]`, `[{"type":"text","text":"a"}]`},
		{"empty string", `""`, `""`},
	}
	for _, tc := range tests {
		var c Content
		if err := json.Unmarshal([]byte(tc.in), &c); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		b, _ := json.Marshal(c)
		if string(b) != tc.out {
			t.Errorf("%s: got %s, want %s", tc.name, b, tc.out)
		}
	}
	var c Content
	if err := json.Unmarshal([]byte(`42`), &c); err == nil {
		t.Error("a number must be rejected")
	}
}

func TestStopAcceptsStringOrArray(t *testing.T) {
	var r ChatRequest
	_ = json.Unmarshal([]byte(`{"stop":"END"}`), &r)
	if len(r.Stop) != 1 || r.Stop[0] != "END" {
		t.Fatalf("string form: %v", r.Stop)
	}
	_ = json.Unmarshal([]byte(`{"stop":["a","b"]}`), &r)
	if len(r.Stop) != 2 {
		t.Fatalf("array form: %v", r.Stop)
	}
}

func TestClassifyStatus(t *testing.T) {
	tests := []struct {
		status int
		ctxLen bool
		want   ErrKind
	}{
		{429, false, KindRateLimited}, {500, false, KindServer}, {503, false, KindServer}, {529, false, KindServer},
		{400, false, KindBadRequest}, {404, false, KindBadRequest}, {400, true, KindContextLength},
		{401, false, KindAuth}, {403, false, KindAuth}, {408, false, KindTimeout},
	}
	for _, tc := range tests {
		if got := ClassifyStatus(tc.status, "", tc.ctxLen, "x").Kind; got != tc.want {
			t.Errorf("status %d ctxLen=%v: got %s, want %s", tc.status, tc.ctxLen, got, tc.want)
		}
	}
	if d := ClassifyStatus(429, "2", false, "x").RetryAfter; d.Seconds() != 2 {
		t.Errorf("Retry-After: got %v", d)
	}
	if d := ParseRetryAfter("garbage"); d != 0 {
		t.Errorf("garbage Retry-After: got %v", d)
	}
}
