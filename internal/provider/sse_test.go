package provider

import (
	"io"
	"strings"
	"testing"
)

func TestSSEReader(t *testing.T) {
	in := ": keep-alive\n\nevent: ping\ndata: {\"a\":1}\n\ndata: line1\ndata: line2\n\ndata: [DONE]\n\n"
	r := NewSSEReader(strings.NewReader(in))
	want := []struct{ event, data string }{{"ping", `{"a":1}`}, {"", "line1\nline2"}, {"", "[DONE]"}}
	for i, w := range want {
		ev, data, err := r.Next()
		if err != nil || ev != w.event || data != w.data {
			t.Fatalf("event %d: got (%q, %q, %v), want (%q, %q)", i, ev, data, err, w.event, w.data)
		}
	}
	if _, _, err := r.Next(); err != io.EOF {
		t.Fatalf("want io.EOF at end, got %v", err)
	}
}

func TestSSEReaderCutMidEvent(t *testing.T) {
	r := NewSSEReader(strings.NewReader("data: {\"partial\""))
	if _, _, err := r.Next(); err == nil || err == io.EOF {
		t.Fatalf("want a truncation error, got %v", err)
	}
}

func TestSSEReaderCRLF(t *testing.T) {
	r := NewSSEReader(strings.NewReader("data: x\r\n\r\n"))
	if _, d, err := r.Next(); err != nil || d != "x" {
		t.Fatalf("got %q, %v", d, err)
	}
}
