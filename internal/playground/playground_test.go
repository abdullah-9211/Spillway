package playground

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/gateway"
	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/money"
	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/provider/fake"
	"github.com/abdullah-9211/spillway/internal/provider/faults"
	"github.com/abdullah-9211/spillway/internal/usage"
)

const catalogYAML = `
providers:
  p1: { type: openai, base_url: "http://unused" }
  p2: { type: openai, base_url: "http://unused" }
  p3: { type: openai, base_url: "http://unused" }
models:
  - { id: m1, provider: p1, upstream: u1, input_usd_per_mtok: 3, output_usd_per_mtok: 15 }
  - { id: m2, provider: p2, upstream: u2, input_usd_per_mtok: 1, output_usd_per_mtok: 2 }
  - { id: m3, provider: p3, upstream: u3 }
policies:
  - { name: default, type: fallback, models: [m1, m2] }
gateway: { max_retries: 0 }
`

type recorder struct {
	mu   sync.Mutex
	rows []usage.Row
}

func (r *recorder) Record(row usage.Row) { r.mu.Lock(); r.rows = append(r.rows, row); r.mu.Unlock() }

type spend struct{ n money.Micros }

func (s spend) MonthToDate(context.Context, uuid.UUID, time.Time) (money.Micros, error) {
	return s.n, nil
}

type rig struct {
	svc *Service
	p   map[string]*fake.Provider
	rec *recorder
	key keys.Key
}

func newRig(t *testing.T, faultInjection bool, opts ...func(*gateway.Options, *keys.Key)) *rig {
	t.Helper()
	cat, err := gateway.ParseCatalog([]byte(catalogYAML))
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{p: map[string]*fake.Provider{}, rec: &recorder{}, key: keys.Key{ID: uuid.New(), Name: keys.BuiltinName, Prefix: "spw_play"}}
	provs := map[string]provider.Provider{}
	for _, n := range []string{"p1", "p2", "p3"} {
		r.p[n] = fake.New(n)
		r.p[n].Default = fake.Behavior{Text: "the answer", InputTokens: 100, OutputTokens: 20}
		provs[n] = r.p[n]
	}
	gw := gateway.New(cat, provs, nil, r.rec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	o := gateway.Options{FaultInjection: faultInjection}
	for _, f := range opts {
		f(&o, &r.key)
	}
	gw.Use(o)
	r.svc = NewService(gw, func(context.Context) (keys.Key, error) { return r.key, nil }, nil, faultInjection, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.svc.SetFaultSettings(faults.Settings{SlowDelay: 50 * time.Millisecond, CutAfter: 2})
	return r
}

func in(prompt string, fs ...faults.Fault) Input {
	return Input{Policy: "default", Prompt: prompt, Faults: fs}
}

func TestRunAnswersThroughTheGatewayPath(t *testing.T) {
	r := newRig(t, true)
	out, err := r.svc.Run(context.Background(), in("hello"))
	if err != nil {
		t.Fatal(err)
	}
	// 100 in * $3/M + 20 out * $15/M = $0.0006
	if out.Answer != "the answer" || out.Provider != "p1" || out.Model != "m1" || out.Outcome != "ok" || out.InputTokens != 100 || out.OutputTokens != 20 || out.CostUSD != "0.000600" || out.Error != nil {
		t.Errorf("out = %+v", out)
	}
	if len(out.Attempts) != 1 || out.Attempts[0].Kind != "primary" {
		t.Errorf("attempts = %+v", out.Attempts)
	}
	if out.Faults == nil || out.UnusedFaults == nil || out.Attempts == nil {
		t.Error("lists are never null in the response")
	}
	row := r.rec.rows[0]
	if row.KeyID != r.key.ID || row.ID != out.ID {
		t.Errorf("the playground runs under its own key and the same request id: %+v", row)
	}
	if req := r.p["p1"].Requests()[0]; req.Messages[len(req.Messages)-1].Content.PlainText() != "hello" || req.MaxTokens == nil || *req.MaxTokens != DefaultMax {
		t.Errorf("the provider saw %+v", req)
	}
}

func TestSystemPromptTemperatureAndLimit(t *testing.T) {
	r := newRig(t, true)
	temp, max := 0.2, 64
	if _, err := r.svc.Run(context.Background(), Input{Policy: "default", Prompt: "q", System: "  be brief ", Temperature: &temp, MaxTokens: &max}); err != nil {
		t.Fatal(err)
	}
	req := r.p["p1"].Requests()[0]
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[0].Content.PlainText() != "be brief" || *req.Temperature != 0.2 || *req.MaxTokens != 64 {
		t.Errorf("request = %+v", req)
	}
}

func TestInjectedFaultsShowAsAttemptsInOrder(t *testing.T) {
	r := newRig(t, true)
	out, err := r.svc.Run(context.Background(), in("hello", faults.Fault{Provider: "p1", Kind: faults.RateLimit}))
	if err != nil {
		t.Fatal(err)
	}
	if out.Provider != "p2" || len(out.Attempts) != 2 {
		t.Fatalf("out = %+v", out)
	}
	a, b := out.Attempts[0], out.Attempts[1]
	if a.Provider != "p1" || !a.Injected || a.Status != 429 || b.Provider != "p2" || b.Injected || b.Kind != "fallback" {
		t.Errorf("attempts = %+v", out.Attempts)
	}
	if len(out.UnusedFaults) != 0 || len(out.Faults) != 1 {
		t.Errorf("faults %+v unused %+v", out.Faults, out.UnusedFaults)
	}
}

func TestEveryProviderFailingIsAResultNotAnError(t *testing.T) {
	r := newRig(t, true)
	out, err := r.svc.Run(context.Background(), in("hello", faults.Fault{Provider: "p1", Kind: faults.RateLimit}, faults.Fault{Provider: "p2", Kind: faults.ServerError}))
	if err != nil {
		t.Fatalf("the failed route is what the playground exists to show, got an error: %v", err)
	}
	if out.Outcome != "all_providers_failed" || out.Error == nil || out.Error.Code != "all_providers_failed" || out.Answer != "" || len(out.Attempts) != 2 {
		t.Errorf("out = %+v", out)
	}
	if !out.Attempts[0].Injected || !out.Attempts[1].Injected {
		t.Error("both failures are marked as made on purpose")
	}
}

func TestAFaultOnAProviderThePolicyWontTryIsReported(t *testing.T) {
	r := newRig(t, true)
	out, err := r.svc.Run(context.Background(), in("hello", faults.Fault{Provider: "p3", Kind: faults.ServerError}))
	if err != nil {
		t.Fatal(err)
	}
	if out.Provider != "p1" || out.Attempts[0].Injected || len(out.UnusedFaults) != 1 || out.UnusedFaults[0].Provider != "p3" {
		t.Errorf("out = %+v", out)
	}
}

func TestSlowFaultIsRealButLate(t *testing.T) {
	r := newRig(t, true)
	out, err := r.svc.Run(context.Background(), in("hello", faults.Fault{Provider: "p1", Kind: faults.Slow}))
	if err != nil || out.Provider != "p1" || !out.Attempts[0].Injected || out.Attempts[0].LatencyMs < 50 || out.Answer != "the answer" {
		t.Fatalf("%v %+v", err, out)
	}
}

func TestStreamingRunsReturnTheWholeAnswer(t *testing.T) {
	r := newRig(t, true)
	r.p["p1"].Default = fake.Behavior{Text: "one two three four", InputTokens: 100, OutputTokens: 20}
	i := in("hello")
	i.Stream = true
	out, err := r.svc.Run(context.Background(), i)
	if err != nil || out.Answer != "one two three four" || out.Outcome != "ok" || out.InputTokens != 100 || out.OutputTokens != 20 || out.CostUSD != "0.000600" || !out.Stream {
		t.Fatalf("%v %+v", err, out)
	}
}

func TestACutStreamKeepsThePartialAnswerAndSaysSo(t *testing.T) {
	r := newRig(t, true)
	r.p["p1"].Default = fake.Behavior{Text: "one two three four five six"}
	i := in("hello", faults.Fault{Provider: "p1", Kind: faults.CutStream})
	i.Stream = true
	out, err := r.svc.Run(context.Background(), i)
	if err != nil {
		t.Fatal(err)
	}
	if out.Answer == "" || strings.Contains(out.Answer, "six") || out.Outcome != "upstream_error" || out.FinishReason != "interrupted" || out.Error == nil || out.Error.Code != "stream_interrupted" {
		t.Errorf("out = %+v", out)
	}
	if len(out.Attempts) != 1 || !out.Attempts[0].Injected || out.Attempts[0].ErrorKind != "server" {
		t.Errorf("attempts = %+v", out.Attempts)
	}
	if len(r.p["p2"].Requests()) != 0 {
		t.Error("no failover once the answer had started")
	}
}

func TestValidation(t *testing.T) {
	r := newRig(t, true)
	hot, zero, huge := 3.0, 0, MaxTokensCap+1
	tests := map[string]Input{
		"empty prompt":           {Policy: "default", Prompt: "   "},
		"long prompt":            {Policy: "default", Prompt: strings.Repeat("x", MaxPrompt+1)},
		"long system":            {Policy: "default", Prompt: "x", System: strings.Repeat("x", MaxSystem+1)},
		"unknown policy":         {Policy: "nope", Prompt: "x"},
		"temperature":            {Policy: "default", Prompt: "x", Temperature: &hot},
		"zero tokens":            {Policy: "default", Prompt: "x", MaxTokens: &zero},
		"too many tokens":        {Policy: "default", Prompt: "x", MaxTokens: &huge},
		"unknown fault kind":     in("x", faults.Fault{Provider: "p1", Kind: "explode"}),
		"unknown fault provider": in("x", faults.Fault{Provider: "nobody", Kind: faults.Slow}),
		"cut without streaming":  in("x", faults.Fault{Provider: "p1", Kind: faults.CutStream}),
		"too many faults": in("x", func() []faults.Fault {
			f := make([]faults.Fault, MaxFaults+1)
			for i := range f {
				f[i] = faults.Fault{Provider: "p1", Kind: faults.Slow}
			}
			return f
		}()...),
	}
	for name, input := range tests {
		if _, err := r.svc.Run(context.Background(), input); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := len(r.p["p1"].Requests()); n != 0 {
		t.Errorf("an invalid request reached a provider %d times", n)
	}
}

func TestFaultsAreRefusedWhenSwitchedOff(t *testing.T) {
	r := newRig(t, false)
	_, err := r.svc.Run(context.Background(), in("x", faults.Fault{Provider: "p1", Kind: faults.Slow}))
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "switched off") {
		t.Errorf("err = %v", err)
	}
	if _, err := r.svc.Run(context.Background(), in("a plain prompt still works")); err != nil {
		t.Errorf("plain prompts are unaffected: %v", err)
	}
}

func TestBudgetUsedUpIsAnErrorBeforeAnyProvider(t *testing.T) {
	budget := money.Micros(1_000_000)
	r := newRig(t, true, func(o *gateway.Options, k *keys.Key) {
		o.Spend = spend{n: 1_000_000}
		k.MonthlyBudget = &budget
	})
	_, err := r.svc.Run(context.Background(), in("hello"))
	var ge *gateway.Error
	if !errors.As(err, &ge) || ge.Status != 402 {
		t.Fatalf("err = %v", err)
	}
	if len(r.p["p1"].Requests()) != 0 {
		t.Error("an over-budget playground request must not reach a provider")
	}
}
