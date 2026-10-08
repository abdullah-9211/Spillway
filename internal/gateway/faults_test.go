package gateway

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/provider/fake"
	"github.com/abdullah-9211/spillway/internal/provider/faults"
	"github.com/abdullah-9211/spillway/internal/usage"
)

var playgroundKey = keys.Key{ID: uuid.New(), Name: keys.BuiltinName, Prefix: "spw_play"}

func faultRig(t *testing.T, enabled bool) (*rig, *recObserver) {
	t.Helper()
	r := newExecRig(t)
	obs := &recObserver{}
	r.gw.Use(Options{Observer: obs, FaultInjection: enabled})
	return r, obs
}

func withFault(kind faults.Kind, prov string) context.Context {
	return WithFaults(context.Background(), []faults.Fault{{Provider: prov, Kind: kind}}, faults.Settings{SlowDelay: 60 * time.Millisecond, CutAfter: 2})
}

func chatAs(r *rig, ctx context.Context, key keys.Key, model string) (*Result, error) {
	return r.gw.Chat(ctx, key, uuid.New(), "", userReq(model))
}

func TestInjectedFailureFallsBackAtOnceAndIsMarked(t *testing.T) {
	for kind, want := range map[faults.Kind]struct {
		status int
		ek     string
	}{faults.RateLimit: {429, "rate_limited"}, faults.ServerError: {503, "server"}} {
		t.Run(string(kind), func(t *testing.T) {
			r, _ := faultRig(t, true)
			res, err := chatAs(r, withFault(kind, "p1"), playgroundKey, "chain")
			if err != nil {
				t.Fatal(err)
			}
			if res.Provider != "p2" {
				t.Fatalf("answered by %s, want the fallback p2", res.Provider)
			}
			if got := attemptKinds(res.Trace); !eq(got, []string{"p1:primary", "p2:fallback"}) {
				t.Errorf("attempts = %v: an injected failure is not retried", got)
			}
			a := res.Trace[0]
			if !a.Injected || a.Status != want.status || a.ErrorKind != want.ek || a.Error == "" {
				t.Errorf("failed attempt = %+v", a)
			}
			if res.Trace[1].Injected {
				t.Errorf("the real fallback attempt is not injected: %+v", res.Trace[1])
			}
			if n := len(r.p["p1"].Requests()); n != 0 {
				t.Errorf("the real p1 must never be called, got %d", n)
			}
		})
	}
}

func TestInjectedFailuresNeverTouchTheBreakerOrMetrics(t *testing.T) {
	r, obs := faultRig(t, true)
	for i := 0; i < 12; i++ { // far more than the 5 failures that open a real breaker
		if _, err := chatAs(r, withFault(faults.ServerError, "p1"), playgroundKey, "chain"); err != nil {
			t.Fatal(err)
		}
	}
	if st := r.gw.breakers.For("p1").State(); st != BreakerClosed {
		t.Errorf("breaker = %s: injected failures must not open it", st)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.attempts) != 0 {
		t.Errorf("a request with faults stays out of the provider metrics, got %v", obs.attempts)
	}
	// The request itself is still counted, as an ordinary answered request.
	if len(obs.requests) != 12 || obs.requests[0].Outcome != "ok" {
		t.Errorf("requests = %d %+v", len(obs.requests), obs.requests[0])
	}
}

func TestInjectedFaultStillWorksWhenTheRealBreakerIsOpen(t *testing.T) {
	r, _ := faultRig(t, true)
	r.gw.exec.cfg.MaxRetries = 0
	r.p["p1"].Default = fake.Behavior{Status: 500}
	for i := 0; i < 5; i++ {
		chatAs(r, context.Background(), testKey, "chain")
	}
	if r.gw.breakers.For("p1").State() != BreakerOpen {
		t.Fatal("setup: breaker should be open")
	}
	res, err := chatAs(r, withFault(faults.RateLimit, "p1"), playgroundKey, "chain")
	if err != nil || !res.Trace[0].Injected || res.Trace[0].Kind == "skipped" {
		t.Errorf("the injected failure should show even though the real breaker is open: %v %+v", err, res.Trace)
	}
}

func TestSlowFaultDelaysARealAnswer(t *testing.T) {
	r, _ := faultRig(t, true)
	r.p["p1"].Default = fake.Behavior{Text: "slowly"}
	start := time.Now()
	res, err := chatAs(r, withFault(faults.Slow, "p1"), playgroundKey, "chain")
	if err != nil || res.Provider != "p1" {
		t.Fatalf("%v %v", res, err)
	}
	if time.Since(start) < 60*time.Millisecond {
		t.Error("the answer should be late by the injected delay")
	}
	if a := res.Trace[0]; !a.Injected || a.Error != "" || a.LatencyMs < 60 {
		t.Errorf("a slowed success is marked injected: %+v", a)
	}
	if len(r.p["p1"].Requests()) != 1 {
		t.Error("slow makes the real call")
	}
}

func TestFaultsAreIgnoredForAnyoneButThePlaygroundKey(t *testing.T) {
	r, _ := faultRig(t, true)
	res, err := chatAs(r, withFault(faults.ServerError, "p1"), testKey, "chain") // an ordinary key
	if err != nil || res.Provider != "p1" || res.Trace[0].Injected {
		t.Fatalf("an ordinary key must not be able to inject faults: %v %+v", err, res)
	}
	if len(r.p["p1"].Requests()) != 1 {
		t.Error("the real provider should have answered")
	}
}

func TestFaultsAreIgnoredWhenSwitchedOff(t *testing.T) {
	r, _ := faultRig(t, false)
	res, err := chatAs(r, withFault(faults.ServerError, "p1"), playgroundKey, "chain")
	if err != nil || res.Provider != "p1" || res.Trace[0].Injected {
		t.Fatalf("with playground.fault_injection off nothing is injected: %v %+v", err, res)
	}
}

func TestAFaultOnAProviderNotInThePlanDoesNothing(t *testing.T) {
	r, _ := faultRig(t, true)
	res, err := chatAs(r, withFault(faults.ServerError, "p3"), playgroundKey, "chain") // chain is m1 (p1), m2 (p2)
	if err != nil || res.Provider != "p1" || res.Trace[0].Injected {
		t.Errorf("%v %+v", err, res)
	}
	providers, err := r.gw.CandidateProviders("chain")
	if err != nil || len(providers) != 2 || providers[0] != "p1" || providers[1] != "p2" {
		t.Errorf("candidate providers = %v %v", providers, err)
	}
}

func TestInjectedAttemptsAreRecordedInUsage(t *testing.T) {
	r, _ := faultRig(t, true)
	if _, err := chatAs(r, withFault(faults.RateLimit, "p1"), playgroundKey, "chain"); err != nil {
		t.Fatal(err)
	}
	row := r.rec.only(t)
	if len(row.Attempts) != 2 || !row.Attempts[0].Injected || row.Attempts[1].Injected || row.Provider != "p2" {
		t.Errorf("row: %+v", row)
	}
}

func TestEveryProviderFaultedGivesAllProvidersFailedWithAttempts(t *testing.T) {
	r, _ := faultRig(t, true)
	ctx := WithFaults(context.Background(), []faults.Fault{{Provider: "p1", Kind: faults.RateLimit}, {Provider: "p2", Kind: faults.ServerError}}, faults.Settings{})
	_, err := chatAs(r, ctx, playgroundKey, "chain")
	var ge *Error
	if !errors.As(err, &ge) || ge.Code != "all_providers_failed" || len(ge.Attempts) != 2 || !ge.Attempts[0].Injected || !ge.Attempts[1].Injected {
		t.Fatalf("err = %v (%+v)", err, ge)
	}
	if row := r.rec.only(t); row.Outcome != usage.OutcomeAllFailed {
		t.Errorf("outcome = %s", row.Outcome)
	}
}

func TestCutStreamEndsTheStreamWithAnInjectedError(t *testing.T) {
	r, obs := faultRig(t, true)
	r.p["p1"].Default = fake.Behavior{Text: "one two three four five six"}
	req := userReq("chain")
	req.Stream = true
	st, err := r.gw.ChatStream(withFault(faults.CutStream, "p1"), playgroundKey, uuid.New(), "", req)
	if err != nil {
		t.Fatal(err)
	}
	var text string
	var derr error
	for {
		c, err := st.Next()
		if errors.Is(err, io.EOF) {
			t.Fatal("a cut stream must not end cleanly")
		}
		if err != nil {
			derr = err
			break
		}
		for _, ch := range c.Choices {
			if ch.Delta.Content != nil {
				text += *ch.Delta.Content
			}
		}
	}
	attempts := st.Finish(context.Background(), derr)
	if text == "" || text == "one two three four five six" {
		t.Errorf("text = %q: should be partial", text)
	}
	if len(attempts) != 1 || !attempts[0].Injected || attempts[0].ErrorKind != "server" {
		t.Errorf("attempts = %+v", attempts)
	}
	row := r.rec.only(t)
	if row.Outcome != usage.OutcomeUpstreamError || row.Provider != "p1" {
		t.Errorf("a cut stream is recorded as an upstream error from the provider that was cut: %+v", row)
	}
	if n := len(r.p["p2"].Requests()); n != 0 {
		t.Errorf("no failover once bytes were sent, p2 called %d times", n)
	}
	if r.gw.breakers.For("p1").State() != BreakerClosed || len(obs.attempts) != 0 {
		t.Error("a cut made on purpose must not reach the breaker or the metrics")
	}
}

func TestFaultsWithoutAnyFaultChangeNothing(t *testing.T) {
	r, _ := faultRig(t, true)
	res, err := chatAs(r, WithFaults(context.Background(), nil, faults.Settings{}), playgroundKey, "chain")
	if err != nil || res.Provider != "p1" || len(res.Trace) != 1 || res.Trace[0].Injected {
		t.Errorf("%v %+v", err, res)
	}
}
