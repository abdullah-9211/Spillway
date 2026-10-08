package faults_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/provider/fake"
	"github.com/abdullah-9211/spillway/internal/provider/faults"
)

func req() *provider.ChatRequest {
	return &provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: provider.TextContent("hi")}}}
}

func wrap(kind faults.Kind, p *fake.Provider) provider.Provider {
	return faults.Wrap(p, faults.Fault{Provider: "fake", Kind: kind}, faults.Settings{SlowDelay: 80 * time.Millisecond, CutAfter: 2})
}

func TestRateLimitAndServerErrorFailWithoutCallingTheProvider(t *testing.T) {
	for kind, want := range map[faults.Kind]struct {
		k      provider.ErrKind
		status int
	}{faults.RateLimit: {provider.KindRateLimited, 429}, faults.ServerError: {provider.KindServer, 503}} {
		p := fake.New("fake")
		w := wrap(kind, p)
		for name, call := range map[string]func() error{
			"chat":   func() error { _, err := w.Chat(context.Background(), req()); return err },
			"stream": func() error { _, err := w.ChatStream(context.Background(), req()); return err },
		} {
			err := call()
			var pe *provider.ProviderError
			if !errors.As(err, &pe) || pe.Kind != want.k || pe.Status != want.status || !pe.Injected {
				t.Errorf("%s/%s: %v, want an injected %s %d", kind, name, err, want.k, want.status)
			}
		}
		if n := len(p.Requests()); n != 0 {
			t.Errorf("%s: the real provider must not be called, got %d calls", kind, n)
		}
	}
}

func TestRateLimitAsksForASecondsWait(t *testing.T) {
	_, err := wrap(faults.RateLimit, fake.New("fake")).Chat(context.Background(), req())
	var pe *provider.ProviderError
	if !errors.As(err, &pe) || pe.RetryAfter != time.Second {
		t.Errorf("Retry-After = %v", pe.RetryAfter)
	}
}

func TestSlowDelaysThenAnswersForReal(t *testing.T) {
	p := fake.New("fake")
	p.Default = fake.Behavior{Text: "late but fine"}
	start := time.Now()
	resp, err := wrap(faults.Slow, p).Chat(context.Background(), req())
	if err != nil || resp.Choices[0].Message.Content.PlainText() != "late but fine" {
		t.Fatalf("%v %+v", err, resp)
	}
	if took := time.Since(start); took < 80*time.Millisecond {
		t.Errorf("answered after %v, want at least the 80ms delay", took)
	}
	if len(p.Requests()) != 1 {
		t.Error("slow still makes the real call")
	}
}

func TestSlowStopsWhenTheRequestIsCancelled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	p := fake.New("fake")
	start := time.Now()
	_, err := wrap(faults.Slow, p).Chat(ctx, req())
	if provider.KindOf(err) != provider.KindTimeout || time.Since(start) > 70*time.Millisecond {
		t.Errorf("err=%v after %v", err, time.Since(start))
	}
	if len(p.Requests()) != 0 {
		t.Error("a cancelled slow request must not reach the provider")
	}
}

func TestCutStreamDeliversChunksThenFails(t *testing.T) {
	p := fake.New("fake")
	p.Default = fake.Behavior{Text: "one two three four five"}
	rd, err := wrap(faults.CutStream, p).ChatStream(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	var content int
	for {
		c, err := rd.Next()
		if errors.Is(err, io.EOF) {
			t.Fatal("a cut stream must not end cleanly")
		}
		if err != nil {
			var pe *provider.ProviderError
			if !errors.As(err, &pe) || !pe.Injected || pe.Kind != provider.KindServer {
				t.Fatalf("err = %v", err)
			}
			break
		}
		for _, ch := range c.Choices {
			if ch.Delta.Content != nil && *ch.Delta.Content != "" {
				content++
			}
		}
	}
	if content != 2 {
		t.Errorf("delivered %d content chunks before the cut, want 2", content)
	}
}

func TestCutStreamLeavesNonStreamingCallsAlone(t *testing.T) {
	p := fake.New("fake")
	resp, err := wrap(faults.CutStream, p).Chat(context.Background(), req())
	if err != nil || resp == nil {
		t.Fatalf("cut_stream only affects streams: %v", err)
	}
}

func TestWrappedProviderKeepsItsName(t *testing.T) {
	if got := wrap(faults.Slow, fake.New("anthropic")).Name(); got != "anthropic" {
		t.Errorf("name = %q", got)
	}
}

func TestKindValidation(t *testing.T) {
	for _, k := range faults.Kinds {
		if !k.Valid() {
			t.Errorf("%s should be valid", k)
		}
	}
	if faults.Kind("explode").Valid() || faults.Kind("").Valid() {
		t.Error("unknown kinds are invalid")
	}
}
