package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/runs"
)

type memSource map[string]Tool

func (m memSource) Get(_ context.Context, names []string) ([]Tool, error) {
	var out []Tool
	for _, n := range names {
		t, ok := m[n]
		if !ok {
			return nil, ErrNotFound
		}
		out = append(out, t)
	}
	return out, nil
}

var runID = uuid.MustParse("01a121cc-ee68-74de-8171-8af2ee9ad8ff")

func inv(name string) runs.ToolInvocation {
	return runs.ToolInvocation{RunID: runID, StepNo: 4, Name: name, Arguments: json.RawMessage(`{"to":"a@b.c"}`), IdempotencyKey: runs.IdempotencyKey(runID, 4)}
}

func serve(t *testing.T, h http.HandlerFunc) (*httptest.Server, func(Tool) *Executor) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, func(tool Tool) *Executor {
		tool.Endpoint = srv.URL
		tool.Kind = HTTP
		if tool.Name == "" {
			tool.Name = "send_email"
		}
		return &Executor{Source: memSource{tool.Name: tool}, WebhookSecret: "global-secret"}
	}
}

func TestCallSendsTheBodyTheKeyAndASignature(t *testing.T) {
	var got struct {
		body []byte
		hdr  http.Header
	}
	_, mk := serve(t, func(w http.ResponseWriter, r *http.Request) {
		got.body, _ = io.ReadAll(r.Body)
		got.hdr = r.Header.Clone()
		io.WriteString(w, `{"sent":true}`)
	})
	ex := mk(Tool{Headers: map[string]string{"Authorization": "Bearer tool-token"}})
	out, err := ex.Call(context.Background(), inv("send_email"))
	if err != nil || out != `{"sent":true}` {
		t.Fatalf("%q %v", out, err)
	}
	var b callBody
	if err := json.Unmarshal(got.body, &b); err != nil || b.Tool != "send_email" || b.RunID != runID.String() || b.StepNo != 4 || string(b.Arguments) != `{"to":"a@b.c"}` {
		t.Errorf("body = %s", got.body)
	}
	if got.hdr.Get("Idempotency-Key") != runs.IdempotencyKey(runID, 4) {
		t.Errorf("Idempotency-Key = %q", got.hdr.Get("Idempotency-Key"))
	}
	if got.hdr.Get("Authorization") != "Bearer tool-token" || got.hdr.Get("Content-Type") != "application/json" {
		t.Errorf("headers = %v", got.hdr)
	}
	if sig := got.hdr.Get("X-Spillway-Signature"); !Verify("global-secret", got.body, sig) || !strings.HasPrefix(sig, "sha256=") {
		t.Errorf("signature %q does not verify against the body that arrived", sig)
	}
	if Verify("wrong", got.body, got.hdr.Get("X-Spillway-Signature")) {
		t.Error("a wrong secret must not verify")
	}
}

func TestTheSameStepSendsTheSameKeyEveryTime(t *testing.T) {
	var keys []string
	_, mk := serve(t, func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		io.WriteString(w, "ok")
	})
	ex := mk(Tool{})
	_, _ = ex.Call(context.Background(), inv("send_email"))
	_, _ = ex.Call(context.Background(), inv("send_email")) // the re-issue after a crash
	if len(keys) != 2 || keys[0] != keys[1] || keys[0] == "" {
		t.Errorf("keys = %v", keys)
	}
}

func TestAToolMayHaveItsOwnSigningSecretWhichIsNeverSent(t *testing.T) {
	var body []byte
	var hdr http.Header
	_, mk := serve(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		hdr = r.Header.Clone()
		io.WriteString(w, "ok")
	})
	ex := mk(Tool{Headers: map[string]string{"X-Spillway-Signing-Secret": "tool-secret"}})
	if _, err := ex.Call(context.Background(), inv("send_email")); err != nil {
		t.Fatal(err)
	}
	if !Verify("tool-secret", body, hdr.Get("X-Spillway-Signature")) || Verify("global-secret", body, hdr.Get("X-Spillway-Signature")) {
		t.Error("the tool's own secret signs")
	}
	for k := range hdr {
		if strings.EqualFold(k, SigningSecretHeader) {
			t.Error("the signing secret was sent to the tool")
		}
	}
}

func TestNoSecretMeansNoSignatureHeader(t *testing.T) {
	var hdr http.Header
	srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { hdr = r.Header.Clone(); io.WriteString(w, "ok") })
	ex := &Executor{Source: memSource{"t": {Name: "t", Kind: HTTP, Endpoint: srv.URL}}}
	if _, err := ex.Call(context.Background(), inv("t")); err != nil {
		t.Fatal(err)
	}
	if hdr.Get("X-Spillway-Signature") != "" {
		t.Error("nothing to sign with")
	}
}

func TestFailuresAreErrorsTheModelCanRead(t *testing.T) {
	t.Run("a non-2xx answer", func(t *testing.T) {
		_, mk := serve(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(502)
			io.WriteString(w, "upstream is down")
		})
		_, err := mk(Tool{}).Call(context.Background(), inv("send_email"))
		if err == nil || !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "upstream is down") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a timeout", func(t *testing.T) {
		_, mk := serve(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) })
		start := time.Now()
		_, err := mk(Tool{Timeout: 50 * time.Millisecond}).Call(context.Background(), inv("send_email"))
		if err == nil || !strings.Contains(err.Error(), "timed out") || time.Since(start) > 250*time.Millisecond {
			t.Errorf("err = %v after %v", err, time.Since(start))
		}
	})
	t.Run("a redirect is not followed", func(t *testing.T) {
		_, mk := serve(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://169.254.169.254/", http.StatusFound)
		})
		if _, err := mk(Tool{}).Call(context.Background(), inv("send_email")); err == nil || !strings.Contains(err.Error(), "302") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a cancelled run is not reported as a timeout", func(t *testing.T) {
		_, mk := serve(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(200 * time.Millisecond) })
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(30*time.Millisecond, cancel)
		_, err := mk(Tool{}).Call(ctx, inv("send_email"))
		if err == nil || strings.Contains(err.Error(), "timed out") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("an unknown tool and an unreachable mcp server", func(t *testing.T) {
		ex := &Executor{Source: memSource{"m": {Name: "m", Kind: MCP, Endpoint: "http://x"}}}
		if _, err := ex.Call(context.Background(), inv("nope")); err == nil {
			t.Error("unknown tool")
		}
		if _, err := ex.Call(context.Background(), inv("m")); err == nil {
			t.Error("an unreachable MCP server must be an error")
		}
	})
}

func TestBigAnswersAreTruncatedWithAMarker(t *testing.T) {
	_, mk := serve(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, strings.Repeat("x", 1000)) })
	ex := mk(Tool{})
	ex.MaxResponse = 100
	out, err := ex.Call(context.Background(), inv("send_email"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, strings.Repeat("x", 100)) || strings.Contains(out, strings.Repeat("x", 101)) || !strings.Contains(out, "[truncated") {
		t.Errorf("out = %q", out)
	}
	ex.MaxResponse = 1000 // exactly the limit is not truncated
	if out, _ := ex.Call(context.Background(), inv("send_email")); strings.Contains(out, "truncated") {
		t.Error("an answer exactly at the cap is whole")
	}
}

func TestSpecsDescribeTheToolsToTheModel(t *testing.T) {
	ex := &Executor{Source: memSource{
		"a": {Name: "a", Description: "does a", InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)},
		"b": {Name: "b"},
	}}
	specs, err := ex.Specs([]string{"b", "a"})
	if err != nil || len(specs) != 2 || specs[0].Function.Name != "b" || specs[1].Function.Description != "does a" || !strings.Contains(string(specs[1].Function.Parameters), `"q"`) {
		t.Fatalf("%+v %v", specs, err)
	}
	if !strings.Contains(string(specs[0].Function.Parameters), `"object"`) {
		t.Errorf("a tool with no schema still gets an object schema: %s", specs[0].Function.Parameters)
	}
	if _, err := ex.Specs([]string{"zzz"}); err == nil {
		t.Error("an unknown tool is an error")
	}
}

func TestNameValidation(t *testing.T) {
	for _, ok := range []string{"web_search", "send-email", "crm.lookup", "A1"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "has space", "sl/ash", strings.Repeat("a", 65), "é"} {
		if ValidateName(bad) == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}
