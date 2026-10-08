package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/gateway"
	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/provider/fake"
	"github.com/abdullah-9211/spillway/internal/usage"
)

var update = flag.Bool("update", false, "rewrite golden files")

type memRecorder struct {
	mu   sync.Mutex
	rows []usage.Row
}

func (m *memRecorder) Record(r usage.Row) { m.mu.Lock(); m.rows = append(m.rows, r); m.mu.Unlock() }

// waitRow waits for the n-th row; streaming handlers record just after the response ends.
func (m *memRecorder) waitRow(t *testing.T, n int) usage.Row {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		if len(m.rows) >= n {
			r := m.rows[n-1]
			m.mu.Unlock()
			return r
		}
		m.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("usage row %d never recorded", n)
	return usage.Row{}
}

type staticAuth struct{ valid string }

func (a staticAuth) Authenticate(_ context.Context, token string) (keys.Key, error) {
	if token == a.valid {
		return keys.Key{ID: uuid.MustParse("00000000-0000-7000-8000-000000000001"), Name: "test"}, nil
	}
	return keys.Key{}, keys.ErrInvalidKey
}

const apiCatalog = `
providers:
  fake:  { type: openai, base_url: "http://unused" }
  fake2: { type: openai, base_url: "http://unused" }
models:
  - { id: m,  provider: fake,  upstream: up-model, input_usd_per_mtok: 3, output_usd_per_mtok: 15 }
  - { id: m2, provider: fake2, upstream: up-two,   input_usd_per_mtok: 1, output_usd_per_mtok: 2 }
policies:
  - { name: default, type: fixed, model: m }
  - { name: chain, type: fallback, models: [m, m2] }
  - { name: ab, type: weighted, arms: [{model: m, weight: 1}, {model: m2, weight: 1}] }
gateway: { max_retries: 0, first_byte_timeout: 500ms }
`

type rig struct {
	srv   *httptest.Server
	prov  *fake.Provider // fake, behind model m
	prov2 *fake.Provider // fake2, behind model m2
	rec   *memRecorder
}

const goodToken = "spw_test"

func newRig(t *testing.T) *rig {
	t.Helper()
	cat, err := gateway.ParseCatalog([]byte(apiCatalog))
	if err != nil {
		t.Fatal(err)
	}
	p, p2 := fake.New("fake"), fake.New("fake2")
	rec := &memRecorder{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw := gateway.New(cat, map[string]provider.Provider{"fake": p, "fake2": p2}, nil, rec, log)
	srv := httptest.NewServer(NewHandler(Options{Gateway: gw, Auth: staticAuth{valid: goodToken}, Log: log}))
	t.Cleanup(srv.Close)
	return &rig{srv: srv, prov: p, prov2: p2, rec: rec}
}

func (r *rig) post(t *testing.T, path, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, r.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+goodToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

const simple = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

func readJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAuth(t *testing.T) {
	r := newRig(t)
	tests := []struct{ name, header string }{
		{"missing", ""},
		{"wrong scheme", "Basic abc"},
		{"empty bearer", "Bearer "},
		{"unknown key", "Bearer spw_nope"},
	}
	for _, tc := range tests {
		req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/v1/chat/completions", strings.NewReader(simple))
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body := readJSON(t, resp)
		resp.Body.Close()
		e, _ := body["error"].(map[string]any)
		if resp.StatusCode != 401 || e["code"] != "invalid_api_key" || e["type"] != "authentication_error" {
			t.Errorf("%s: %d %v", tc.name, resp.StatusCode, body)
		}
		if resp.Header.Get("X-Spillway-Request-Id") == "" {
			t.Errorf("%s: even a 401 carries a request id", tc.name)
		}
	}
	if n := len(r.prov.Requests()); n != 0 {
		t.Errorf("unauthenticated requests reached the provider %d times", n)
	}
}

func TestBadRequests(t *testing.T) {
	r := newRig(t)
	tests := []struct{ name, body, code, param string }{
		{"not json", `{`, "invalid_request", ""},
		{"unknown model", `{"model":"nope","messages":[{"role":"user","content":"x"}]}`, "invalid_request", "model"},
		{"no messages", `{"model":"m","messages":[]}`, "invalid_request", "messages"},
		{"content of the wrong type", `{"model":"m","messages":[{"role":"user","content":5}]}`, "invalid_request", ""},
	}
	for _, tc := range tests {
		resp := r.post(t, "/v1/chat/completions", tc.body)
		e, _ := readJSON(t, resp)["error"].(map[string]any)
		if resp.StatusCode != 400 || e["code"] != tc.code {
			t.Errorf("%s: %d %v", tc.name, resp.StatusCode, e)
		}
		if tc.param != "" && e["param"] != tc.param {
			t.Errorf("%s: param = %v, want %s", tc.name, e["param"], tc.param)
		}
	}
	if resp := r.post(t, "/v1/chat/completions", `{"model":"`+strings.Repeat("x", 11<<20)+`"}`); resp.StatusCode != 413 {
		t.Errorf("oversized body: %d", resp.StatusCode)
	}
}

func TestResponseHeaders(t *testing.T) {
	r := newRig(t)
	r.prov.Default = fake.Behavior{Text: "ok", InputTokens: 1000, OutputTokens: 100}
	resp := r.post(t, "/v1/chat/completions", simple)
	for k, want := range map[string]string{
		"X-Spillway-Provider": "fake", "X-Spillway-Model": "m", "X-Spillway-Cache": "bypass",
		"X-Spillway-Attempts": "1", "X-Spillway-Cost-Usd": "0.004500", // 1000*$3/M + 100*$15/M
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	id := resp.Header.Get("X-Spillway-Request-Id")
	if _, err := uuid.Parse(id); err != nil {
		t.Errorf("request id %q: %v", id, err)
	}
	if row := r.rec.waitRow(t, 1); row.ID.String() != id {
		t.Errorf("usage row id %s != request id %s", row.ID, id)
	}
}

// --- golden tests: the shape OpenAI SDKs expect ---

var (
	idRe      = regexp.MustCompile(`"id":\s*"chatcmpl-[^"]*"`)
	createdRe = regexp.MustCompile(`"created":\s*[0-9]+`)
)

func normalize(b []byte) []byte {
	b = idRe.ReplaceAll(b, []byte(`"id":"chatcmpl-X"`))
	return createdRe.ReplaceAll(b, []byte(`"created":0`))
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden file (run with -update): %v", err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(got)) {
		t.Errorf("%s differs from golden.\n got: %s\nwant: %s", name, got, want)
	}
}

func prettyJSON(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Indent(&buf, b, "", "  "); err != nil {
		t.Fatalf("not JSON: %s", b)
	}
	return buf.Bytes()
}

func TestGoldenChatCompletion(t *testing.T) {
	r := newRig(t)
	r.prov.Default = fake.Behavior{Text: "Hello there", InputTokens: 10, OutputTokens: 5}
	resp := r.post(t, "/v1/chat/completions", simple)
	raw, _ := io.ReadAll(resp.Body)
	golden(t, "chat_completion.golden.json", prettyJSON(t, normalize(raw)))
}

func TestGoldenToolCallResponse(t *testing.T) {
	r := newRig(t)
	r.prov.Default = fake.Behavior{ToolCalls: []provider.ToolCall{{ID: "call_1", Type: "function",
		Function: provider.FunctionCall{Name: "get_weather", Arguments: `{"city":"Paris"}`}}}}
	resp := r.post(t, "/v1/chat/completions", simple)
	raw, _ := io.ReadAll(resp.Body)
	golden(t, "chat_completion_tool_call.golden.json", prettyJSON(t, normalize(raw)))
}

func TestGoldenStream(t *testing.T) {
	for name, body := range map[string]string{
		"chat_stream.golden.txt":               `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		"chat_stream_include_usage.golden.txt": `{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`,
	} {
		r := newRig(t)
		r.prov.Default = fake.Behavior{Text: "Hello there", InputTokens: 10, OutputTokens: 5}
		resp := r.post(t, "/v1/chat/completions", body)
		if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
			t.Errorf("content-type = %q", ct)
		}
		if resp.Header.Get("Cache-Control") != "no-cache" || resp.Header.Get("X-Accel-Buffering") != "no" {
			t.Errorf("SSE headers: %v", resp.Header)
		}
		raw, _ := io.ReadAll(resp.Body)
		golden(t, name, normalize(raw))
		if !bytes.HasSuffix(raw, []byte("data: [DONE]\n\n")) {
			t.Errorf("%s: stream must end with [DONE]", name)
		}
	}
}

func TestStreamSendsOnlyDataLines(t *testing.T) {
	r := newRig(t)
	resp := r.post(t, "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if l := sc.Text(); l != "" && !strings.HasPrefix(l, "data: ") {
			t.Errorf("OpenAI streams only use data: lines, got %q", l)
		}
	}
}

// --- failure behaviour ---

func TestUpstreamFailureBeforeFirstByteIsAPlainHTTPError(t *testing.T) {
	r := newRig(t)
	r.prov.Default = fake.Behavior{Status: 500}
	for _, body := range []string{simple, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`} {
		resp := r.post(t, "/v1/chat/completions", body)
		e, _ := readJSON(t, resp)["error"].(map[string]any)
		if resp.StatusCode != 502 || e["code"] != "upstream_error" || resp.Header.Get("Content-Type") != "application/json" {
			t.Errorf("%d %v %s", resp.StatusCode, e, resp.Header.Get("Content-Type"))
		}
	}
}

func TestStreamBrokenMidwayEndsWithErrorChunk(t *testing.T) {
	r := newRig(t)
	r.prov.Default = fake.Behavior{Text: "one two three four", BreakAfter: 2}
	resp := r.post(t, "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d; once bytes are out the status cannot change", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n\n")
	if lines[len(lines)-1] != "data: [DONE]" {
		t.Fatalf("stream must still end with [DONE]: %q", raw)
	}
	var e struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[len(lines)-2], "data: ")), &e); err != nil || e.Error["code"] != "upstream_error" {
		t.Fatalf("second to last event should be the error chunk: %q (%v)", lines[len(lines)-2], err)
	}
	if row := r.rec.waitRow(t, 1); row.Outcome != usage.OutcomeUpstreamError || row.OutputTokens == 0 {
		t.Errorf("row: %+v", row)
	}
}

func TestClientDisconnectCancelsUpstream(t *testing.T) {
	r := newRig(t)
	r.prov.Default = fake.Behavior{Text: "hello", Hang: true}

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+goodToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(resp.Body)
	if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "data: ") {
		t.Fatalf("first line: %q %v", line, err)
	}
	cancel() // the client goes away mid-stream
	resp.Body.Close()

	row := r.rec.waitRow(t, 1)
	if row.Outcome != usage.OutcomeClientCancelled {
		t.Errorf("outcome = %s, want client_cancelled", row.Outcome)
	}
	deadline := time.Now().Add(3 * time.Second)
	for r.prov.Canceled() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if r.prov.Canceled() != 1 {
		t.Errorf("upstream request cancelled %d times, want 1", r.prov.Canceled())
	}
}

func TestModels(t *testing.T) {
	r := newRig(t)
	req, _ := http.NewRequest(http.MethodGet, r.srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+goodToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)
	ids := map[string]string{}
	for _, d := range list.Data {
		ids[d.ID] = d.OwnedBy
		if d.Object != "model" {
			t.Errorf("object = %q", d.Object)
		}
	}
	if list.Object != "list" || ids["m"] != "fake" || ids["default"] != "spillway" || ids["chain"] != "spillway" || ids["m2"] != "fake2" {
		t.Errorf("list: %+v", list)
	}

	noAuth, _ := http.Get(r.srv.URL + "/v1/models")
	noAuth.Body.Close()
	if noAuth.StatusCode != 401 {
		t.Errorf("models without a key: %d", noAuth.StatusCode)
	}
}

// --- reliability, as the client sees it ---

func TestFallbackIsInvisibleToTheClientExceptInHeaders(t *testing.T) {
	r := newRig(t)
	r.prov.Default = fake.Behavior{Status: 500}
	r.prov2.Default = fake.Behavior{Text: "from the second"}
	resp := r.post(t, "/v1/chat/completions", `{"model":"chain","messages":[{"role":"user","content":"hi"}]}`)
	body := readJSON(t, resp)
	if resp.StatusCode != 200 || resp.Header.Get("X-Spillway-Provider") != "fake2" || resp.Header.Get("X-Spillway-Model") != "m2" {
		t.Fatalf("status %d headers %v", resp.StatusCode, resp.Header)
	}
	if resp.Header.Get("X-Spillway-Attempts") != "2" {
		t.Errorf("attempts = %s, want 2", resp.Header.Get("X-Spillway-Attempts"))
	}
	if body["model"] != "m2" {
		t.Errorf("model = %v", body["model"])
	}
}

func TestAllProvidersFailedBodyListsTheAttempts(t *testing.T) {
	r := newRig(t)
	r.prov.Default = fake.Behavior{Status: 500}
	r.prov2.Default = fake.Behavior{Status: 429}
	resp := r.post(t, "/v1/chat/completions", `{"model":"chain","messages":[{"role":"user","content":"hi"}]}`)
	e, _ := readJSON(t, resp)["error"].(map[string]any)
	if resp.StatusCode != 503 || e["code"] != "all_providers_failed" {
		t.Fatalf("%d %v", resp.StatusCode, e)
	}
	attempts, _ := e["attempts"].([]any)
	if len(attempts) != 2 {
		t.Fatalf("attempts = %v", attempts)
	}
	first := attempts[0].(map[string]any)
	if first["provider"] != "fake" || first["error_kind"] != "server" || first["kind"] != "primary" {
		t.Errorf("first attempt = %v", first)
	}
}

func TestStreamFailoverBeforeFirstByte(t *testing.T) {
	r := newRig(t)
	r.prov.Default = fake.Behavior{Status: 503}
	r.prov2.Default = fake.Behavior{Text: "rescued"}
	resp := r.post(t, "/v1/chat/completions", `{"model":"chain","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("X-Spillway-Provider") != "fake2" || !strings.Contains(string(raw), "rescued") {
		t.Fatalf("status %d provider %s body %s", resp.StatusCode, resp.Header.Get("X-Spillway-Provider"), raw)
	}
}

func TestBucketHeaderMakesWeightedSticky(t *testing.T) {
	r := newRig(t)
	r.prov.Default = fake.Behavior{Text: "one"}
	r.prov2.Default = fake.Behavior{Text: "two"}
	for _, bucket := range []string{"alice", "bob", "carol", "dave", "erin", "frank"} {
		var first string
		for i := 0; i < 4; i++ {
			req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/v1/chat/completions",
				strings.NewReader(`{"model":"ab","messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Authorization", "Bearer "+goodToken)
			req.Header.Set("X-Spillway-Bucket", bucket)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			got := resp.Header.Get("X-Spillway-Model")
			if first == "" {
				first = got
			} else if got != first {
				t.Fatalf("bucket %s moved from %s to %s", bucket, first, got)
			}
		}
	}
}

// A client that stops reading must not hold the upstream request open forever.
func TestSlowClientIsTreatedAsDisconnected(t *testing.T) {
	old := writeTimeout
	writeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { writeTimeout = old })

	r := newRig(t)
	r.prov.Default = fake.Behavior{Text: strings.Repeat("word ", 400_000)} // ~40MB of SSE: far more than socket buffers hold

	req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+goodToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// Read the headers and nothing else: the server's writes will back up and hit the write deadline.
	row := r.rec.waitRow(t, 1)
	if row.Outcome != usage.OutcomeClientCancelled {
		t.Errorf("outcome = %s, want client_cancelled", row.Outcome)
	}
}
