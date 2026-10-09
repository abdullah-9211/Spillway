package runs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/gateway"
	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/provider"
)

func parse(t *testing.T, body string) Request {
	t.Helper()
	var r Request
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	return r
}

var known = ValidateOptions{Known: func(m string) bool { return m == "default" || m == "mini" }}

func TestRequestValidation(t *testing.T) {
	tests := []struct {
		name, body, param string
		opts              ValidateOptions
	}{
		{"a string input", `{"input":"hi"}`, "", known},
		{"a messages input", `{"input":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`, "", known},
		{"a policy and limits", `{"input":"hi","model":"mini","limits":{"max_steps":3,"max_cost_usd":0.5,"deadline_seconds":60}}`, "", known},
		{"no input", `{}`, "input", known},
		{"blank input", `{"input":"   "}`, "input", known},
		{"empty messages", `{"input":[]}`, "input", known},
		{"a system role in messages", `{"input":[{"role":"system","content":"x"}]}`, "input", known},
		{"the last message from the assistant", `{"input":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}]}`, "input", known},
		{"an empty message", `{"input":[{"role":"user","content":""}]}`, "input", known},
		{"an unknown model", `{"input":"hi","model":"nope"}`, "model", known},
		{"tools before the registry exists", `{"input":"hi","tools":["web_search"]}`, "tools", known},
		{"tools once they exist", `{"input":"hi","tools":["web_search"]}`, "", ValidateOptions{Known: known.Known, ToolsReady: true}},
		{"an empty tool name", `{"input":"hi","tools":[" "]}`, "tools", ValidateOptions{Known: known.Known, ToolsReady: true}},
		{"zero steps", `{"input":"hi","limits":{"max_steps":0}}`, "limits.max_steps", known},
		{"a zero deadline", `{"input":"hi","limits":{"deadline_seconds":0}}`, "limits.deadline_seconds", known},
		{"a negative budget", `{"input":"hi","limits":{"max_cost_usd":-1}}`, "limits.max_cost_usd", known},
		{"a zero budget", `{"input":"hi","limits":{"max_cost_usd":0}}`, "limits.max_cost_usd", known},
		{"an oversized input", `{"input":"` + strings.Repeat("x", maxInputBytes+1) + `"}`, "input", known},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := parse(t, tc.body).Validate(tc.opts)
			var v *Validation
			switch {
			case tc.param == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.param != "" && !errors.As(err, &v):
				t.Errorf("want a validation error on %q, got %v", tc.param, err)
			case tc.param != "" && v.Param != tc.param:
				t.Errorf("param = %q, want %q", v.Param, tc.param)
			}
		})
	}
}

func TestServerCapsOverrideRequestLimits(t *testing.T) {
	caps := Caps{MaxSteps: 50, MaxCost: 1_000_000, Deadline: 15 * time.Minute}
	tests := []struct {
		name, body string
		want       Limits
	}{
		{"defaults are the caps", `{"input":"x"}`, Limits{50, 1_000_000, 15 * time.Minute}},
		{"a request may ask for less", `{"input":"x","limits":{"max_steps":5,"max_cost_usd":0.25,"deadline_seconds":30}}`, Limits{5, 250_000, 30 * time.Second}},
		{"never for more", `{"input":"x","limits":{"max_steps":500,"max_cost_usd":99,"deadline_seconds":99999}}`, Limits{50, 1_000_000, 15 * time.Minute}},
		{"each limit on its own", `{"input":"x","limits":{"max_steps":2}}`, Limits{2, 1_000_000, 15 * time.Minute}},
	}
	for _, tc := range tests {
		if got := parse(t, tc.body).ResolveLimits(caps); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestLimitsRoundTripThroughJSON(t *testing.T) {
	in := Limits{MaxSteps: 7, MaxCost: 123_456, Deadline: 90 * time.Second}
	b, _ := json.Marshal(in)
	var out Limits
	if err := json.Unmarshal(b, &out); err != nil || out != in {
		t.Errorf("%s -> %+v %v", b, out, err)
	}
}

func TestInitialMessages(t *testing.T) {
	m := parse(t, `{"system":"sys","input":"hi"}`).InitialMessages()
	if len(m) != 2 || m[0].Role != "system" || m[1].Content.PlainText() != "hi" {
		t.Errorf("%+v", m)
	}
	m = parse(t, `{"input":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`).InitialMessages()
	if len(m) != 3 || m[2].Content.PlainText() != "c" {
		t.Errorf("%+v", m)
	}
	// The request round-trips, so what is stored is what was sent.
	r := parse(t, `{"input":[{"role":"user","content":"a"}],"model":"mini"}`)
	b, _ := json.Marshal(r)
	if r2 := parse(t, string(b)); r2.ModelName() != "mini" || len(r2.Input.Messages) != 1 {
		t.Errorf("round trip: %s", b)
	}
}

type fakeChatter struct {
	res *gateway.Result
	err error
	got struct {
		ctx context.Context
		req *provider.ChatRequest
		id  uuid.UUID
		b   string
	}
}

func (f *fakeChatter) Chat(ctx context.Context, _ keys.Key, id uuid.UUID, bucket string, req *provider.ChatRequest) (*gateway.Result, error) {
	f.got.ctx, f.got.req, f.got.id, f.got.b = ctx, req, id, bucket
	return f.res, f.err
}

type fakeKeys struct {
	key keys.Key
	err error
}

func (f fakeKeys) Get(context.Context, uuid.UUID) (keys.Key, error) { return f.key, f.err }

func TestGatewayCallerMakesTheCallAndSortsFailures(t *testing.T) {
	run := testRun(t, Request{Input: Input{Text: "x"}, Model: "mini"}, Limits{})
	msgs := []provider.Message{{Role: "user", Content: provider.TextContent("x")}}
	ok := &gateway.Result{Provider: "openai", Model: "mini", Cost: 4200, Response: &provider.ChatResponse{
		Choices: []provider.Choice{{Message: provider.Message{Role: "assistant", Content: provider.TextContent("hi")}, FinishReason: "stop"}}, Usage: &provider.Usage{PromptTokens: 3}}}

	t.Run("a successful call", func(t *testing.T) {
		f := &fakeChatter{res: ok}
		c := &GatewayCaller{GW: f, Keys: fakeKeys{}}
		id := uuid.New()
		res, err := c.Call(context.Background(), run, msgs, nil, id)
		if err != nil || res.Message.Content.PlainText() != "hi" || res.Cost != 4200 || res.Provider != "openai" || res.Model != "mini" {
			t.Fatalf("%+v %v", res, err)
		}
		if f.got.req.Model != "mini" || f.got.id != id || f.got.b != run.ID.String() {
			t.Errorf("the gateway saw model %q id %v bucket %q", f.got.req.Model, f.got.id, f.got.b)
		}
	})
	t.Run("a revoked key", func(t *testing.T) {
		now := time.Now()
		_, err := (&GatewayCaller{GW: &fakeChatter{res: ok}, Keys: fakeKeys{key: keys.Key{RevokedAt: &now}}}).Call(context.Background(), run, msgs, nil, uuid.New())
		var ce *CallError
		if !errors.As(err, &ce) || ce.Kind != CallKeyRevoked {
			t.Errorf("err = %v", err)
		}
	})
	for name, tc := range map[string]struct {
		err  error
		kind CallErrorKind
	}{
		"budget exhausted":     {&gateway.Error{Status: 402, Code: "budget_exceeded"}, CallBudget},
		"rate limited":         {&gateway.Error{Status: 429, RetryAfter: 3 * time.Second}, CallTransient},
		"all providers failed": {&gateway.Error{Status: 503, Code: "all_providers_failed"}, CallTransient},
		"a bad request":        {&gateway.Error{Status: 400, Code: "invalid_request"}, CallPermanent},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := (&GatewayCaller{GW: &fakeChatter{err: tc.err}, Keys: fakeKeys{}}).Call(context.Background(), run, msgs, nil, uuid.New())
			var ce *CallError
			if !errors.As(err, &ce) || ce.Kind != tc.kind {
				t.Fatalf("err = %v", err)
			}
			if tc.kind == CallTransient && name == "rate limited" && ce.RetryAfter != 3*time.Second {
				t.Errorf("Retry-After is carried: %v", ce.RetryAfter)
			}
		})
	}
	t.Run("a context error passes through for the engine to read", func(t *testing.T) {
		_, err := (&GatewayCaller{GW: &fakeChatter{err: context.Canceled}, Keys: fakeKeys{}}).Call(context.Background(), run, msgs, nil, uuid.New())
		var ce *CallError
		if errors.As(err, &ce) || !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
	})
}
