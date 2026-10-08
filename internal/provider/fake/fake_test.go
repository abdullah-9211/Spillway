package fake_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/provider/fake"
	"github.com/abdullah-9211/spillway/internal/provider/providertest"
)

// The fake is itself a provider, so it must pass the same conformance suite as the real adapters.
func TestConformance(t *testing.T) {
	tool := provider.ToolCall{ID: "call_1", Type: "function", Function: provider.FunctionCall{Name: "get_weather", Arguments: `{"city":"Paris"}`}}
	providertest.Run(t, providertest.Harness{New: func(t *testing.T, s providertest.Scenario) (provider.Provider, <-chan struct{}) {
		p := fake.New("fake")
		b := fake.Behavior{Text: "Hello there"}
		switch s {
		case providertest.ChatToolCall, providertest.StreamToolCall:
			b = fake.Behavior{ToolCalls: []provider.ToolCall{tool}}
		case providertest.StreamHang:
			b.Hang = true
		case providertest.StreamBroken:
			b.BreakAfter = 1
		case providertest.Err429:
			b = fake.Behavior{Status: 429, RetryAfter: 2 * time.Second}
		case providertest.Err500:
			b = fake.Behavior{Status: 500}
		case providertest.Err400:
			b = fake.Behavior{Status: 400}
		case providertest.Err401:
			b = fake.Behavior{Status: 401}
		case providertest.ErrContext:
			t.Skip("the fake has no way to say a prompt was too long")
		}
		p.Default = b
		canceled := make(chan struct{})
		if s == providertest.StreamHang {
			go func() {
				for p.Canceled() == 0 {
					time.Sleep(5 * time.Millisecond)
				}
				close(canceled)
			}()
		}
		return p, canceled
	}})
}

func TestScriptIsConsumedInOrder(t *testing.T) {
	p := fake.New("fake")
	p.Script(fake.Behavior{Status: 429}, fake.Behavior{Text: "second"})
	req := &provider.ChatRequest{Model: "m"}
	if _, err := p.Chat(context.Background(), req); provider.KindOf(err) != provider.KindRateLimited {
		t.Fatalf("first call: %v", err)
	}
	resp, err := p.Chat(context.Background(), req)
	if err != nil || resp.Choices[0].Message.Content.PlainText() != "second" {
		t.Fatalf("second call: %v %+v", err, resp)
	}
	resp, _ = p.Chat(context.Background(), req) // script exhausted: the default answers
	if resp.Choices[0].Message.Content.PlainText() != "Hello from the fake provider" {
		t.Fatalf("default: %+v", resp)
	}
	if len(p.Requests()) != 3 {
		t.Errorf("requests recorded = %d, want 3", len(p.Requests()))
	}
}

func TestBreakAfterCountsContentChunks(t *testing.T) {
	p := fake.New("fake")
	p.Default = fake.Behavior{Text: "a b c d", BreakAfter: 2}
	rd, _ := p.ChatStream(context.Background(), &provider.ChatRequest{Model: "m"})
	defer rd.Close()
	var content int
	for {
		c, err := rd.Next()
		if errors.Is(err, io.EOF) {
			t.Fatal("stream should have broken")
		}
		if err != nil {
			break
		}
		if len(c.Choices) > 0 && c.Choices[0].Delta.Content != nil && *c.Choices[0].Delta.Content != "" {
			content++
		}
	}
	if content != 2 {
		t.Errorf("content chunks before break = %d, want 2", content)
	}
	_ = json.Valid
}
