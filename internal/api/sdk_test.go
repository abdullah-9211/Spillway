package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"

	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/provider/fake"
)

// These tests drive the gateway with the official OpenAI Go SDK, which is the practical definition of
// "OpenAI-compatible": if the SDK parses it, existing clients work unchanged.

func sdkClient(r *rig, token string) openai.Client {
	return openai.NewClient(option.WithBaseURL(r.srv.URL+"/v1/"), option.WithAPIKey(token), option.WithMaxRetries(0))
}

func TestSDKChat(t *testing.T) {
	r := newRig(t)
	r.prov.Default = fake.Behavior{Text: "Hello there", InputTokens: 10, OutputTokens: 5}
	c := sdkClient(r, goodToken)

	resp, err := c.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Model:    "m",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.SystemMessage("be brief"), openai.UserMessage("hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Choices[0].Message.Content; got != "Hello there" {
		t.Errorf("content = %q", got)
	}
	if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 5 || resp.Choices[0].FinishReason != "stop" {
		t.Errorf("usage/finish: %+v", resp)
	}
	// The system message reached the provider in the canonical shape.
	if got := r.prov.Requests()[0].Messages; len(got) != 2 || got[0].Role != "system" || got[0].Content.PlainText() != "be brief" {
		t.Errorf("provider saw %+v", got)
	}
}

func TestSDKStreamAccumulates(t *testing.T) {
	r := newRig(t)
	r.prov.Default = fake.Behavior{Text: "Hello there friend", InputTokens: 10, OutputTokens: 5}
	c := sdkClient(r, goodToken)

	stream := c.Chat.Completions.NewStreaming(context.Background(), openai.ChatCompletionNewParams{
		Model:         "m",
		Messages:      []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")},
		StreamOptions: openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)},
	})
	acc := openai.ChatCompletionAccumulator{}
	for stream.Next() {
		acc.AddChunk(stream.Current())
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if got := acc.Choices[0].Message.Content; got != "Hello there friend" {
		t.Errorf("accumulated = %q", got)
	}
	if acc.Usage.PromptTokens != 10 || acc.Usage.CompletionTokens != 5 {
		t.Errorf("usage = %+v", acc.Usage)
	}
}

func TestSDKStreamWithoutUsageOption(t *testing.T) {
	r := newRig(t)
	c := sdkClient(r, goodToken)
	stream := c.Chat.Completions.NewStreaming(context.Background(), openai.ChatCompletionNewParams{
		Model: "m", Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")},
	})
	n := 0
	for stream.Next() {
		if len(stream.Current().Choices) == 0 {
			t.Error("a client that did not ask for usage must never see a chunk with no choices")
		}
		n++
	}
	if stream.Err() != nil || n == 0 {
		t.Fatalf("err=%v chunks=%d", stream.Err(), n)
	}
}

func TestSDKToolCalls(t *testing.T) {
	r := newRig(t)
	r.prov.Default = fake.Behavior{ToolCalls: []provider.ToolCall{{ID: "call_1", Type: "function",
		Function: provider.FunctionCall{Name: "get_weather", Arguments: `{"city":"Paris"}`}}}}
	c := sdkClient(r, goodToken)

	resp, err := c.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Model:    "m",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("weather in Paris?")},
		Tools: []openai.ChatCompletionToolParam{{Function: openai.FunctionDefinitionParam{
			Name: "get_weather", Parameters: openai.FunctionParameters{"type": "object"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := resp.Choices[0].Message.ToolCalls
	if len(calls) != 1 || calls[0].ID != "call_1" || calls[0].Function.Name != "get_weather" || calls[0].Function.Arguments != `{"city":"Paris"}` {
		t.Fatalf("tool calls: %+v", calls)
	}
	if len(r.prov.Requests()[0].Tools) != 1 {
		t.Error("tools were not forwarded")
	}
}

func TestSDKErrors(t *testing.T) {
	r := newRig(t)
	msgs := []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")}

	badClient := sdkClient(r, "spw_wrong")
	_, err := badClient.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{Model: "m", Messages: msgs})
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized || apiErr.Code != "invalid_api_key" {
		t.Errorf("bad key: %v", err)
	}

	good := sdkClient(r, goodToken)
	_, err = good.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{Model: "nope", Messages: msgs})
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest || apiErr.Param != "model" {
		t.Errorf("unknown model: %v", err)
	}

	r.prov.Default = fake.Behavior{Status: 503}
	_, err = good.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{Model: "m", Messages: msgs})
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadGateway {
		t.Errorf("upstream failure: %v", err)
	}
}

func TestSDKModels(t *testing.T) {
	r := newRig(t)
	good := sdkClient(r, goodToken)
	page, err := good.Models.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 5 {
		t.Errorf("models: %+v", page.Data)
	}
}
