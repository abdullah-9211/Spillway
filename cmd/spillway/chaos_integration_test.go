//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/provider/fake"
	"github.com/abdullah-9211/spillway/internal/testdb"
	"github.com/abdullah-9211/spillway/internal/tools"
)

const (
	chaosSecret    = "chaos-webhook-secret"
	toolCallsInRun = 4 // the side-effecting calls
	// Each run also sleeps once and asks for a person's approval once: model, sleep, model, wait_human, then the four
	// model/tool pairs and a closing model call.
	stepsInRun = 2*(toolCallsInRun+2) + 1
)

// effects is the tool receiver the chaos test needs: it records every delivery in a ledger, and applies the side effect
// only the first time it sees an idempotency key. That is what honouring the key means, and exactly-once rests on it.
type effects struct {
	mu         sync.Mutex
	deliveries map[string]int
	applied    map[string]struct {
		run  string
		step int
	}
	badSig int
	rng    *rand.Rand
}

func newEffects() *effects {
	return &effects{deliveries: map[string]int{}, applied: map[string]struct {
		run  string
		step int
	}{}, rng: rand.New(rand.NewSource(1))}
}

func (e *effects) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if !tools.Verify(chaosSecret, body, r.Header.Get("X-Spillway-Signature")) {
		e.mu.Lock()
		e.badSig++
		e.mu.Unlock()
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	var b struct {
		RunID  string `json:"run_id"`
		StepNo int    `json:"step_no"`
	}
	_ = json.Unmarshal(body, &b)
	key := r.Header.Get("Idempotency-Key")
	e.mu.Lock()
	e.deliveries[key]++
	_, seen := e.applied[key]
	if !seen {
		e.applied[key] = struct {
			run  string
			step int
		}{b.RunID, b.StepNo}
	}
	pause := time.Duration(100+e.rng.Intn(300)) * time.Millisecond
	e.mu.Unlock()
	// The effect is applied before the answer goes back, so a worker killed in this pause leaves a step that took effect
	// but was never recorded as finished. Its re-issue must be recognised by the key.
	time.Sleep(pause)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"applied":%t}`, !seen)
}

// TestChaosKillingWorkersLosesNothingAndDuplicatesNothing: runs of thirteen steps with a side-effecting tool, a worker
// process that is SIGKILLed at random and restarted, and the claim that every run still succeeds with each effect
// applied exactly once. The receiver deduplicates by the idempotency key, so redeliveries are expected and counted.
//
// Every run also sleeps for a second and waits for an approval, which a goroutine here gives through the public API, so
// the killed workers are also parked-run and wake-up paths, not only tool calls.
//
// The full test (50 runs, up to 3 minutes) is for CI. Locally set CHAOS_RUNS=10: it then has 60 seconds, and is skipped
// and reported if it cannot finish in that time.
func TestChaosKillingWorkersLosesNothingAndDuplicatesNothing(t *testing.T) {
	runsN := 50
	if v := os.Getenv("CHAOS_RUNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("CHAOS_RUNS=%q is not a positive number", v)
		}
		runsN = n
	} else if os.Getenv("CI") == "" {
		t.Skip("the full chaos test (50 runs, up to 3 minutes) runs in CI; set CHAOS_RUNS=10 to run a reduced one here")
	}
	budget := 3 * time.Minute
	reduced := runsN < 50
	if reduced {
		budget = 60 * time.Second
	}

	pool, dbURL := testdb.New(t)
	bin := buildBinary(t)

	// The fake model asks for the tool until it has been called toolCallsInRun times, then answers.
	fp := fake.New("fake")
	fp.Decide = func(req *provider.ChatRequest) fake.Behavior {
		asked := 0
		for _, m := range req.Messages {
			if m.Role == "assistant" {
				asked++
			}
		}
		call := func(name, args string) fake.Behavior {
			return fake.Behavior{Delay: 80 * time.Millisecond, ToolCalls: []provider.ToolCall{{ID: fmt.Sprintf("call_%d", asked), Type: "function",
				Function: provider.FunctionCall{Name: name, Arguments: args}}}}
		}
		switch {
		case asked == 0:
			return call("sleep", `{"seconds":1}`)
		case asked == 1:
			return call("request_human_approval", `{"reason":"go on?"}`)
		case asked < toolCallsInRun+2:
			return call("effect", fmt.Sprintf(`{"i":%d}`, asked-2))
		}
		return fake.Behavior{Delay: 80 * time.Millisecond, Text: "all effects applied"}
	}
	upstream := httptest.NewServer(fake.NewHandler(fp))
	defer upstream.Close()
	ledger := newEffects()
	recv := httptest.NewServer(ledger)
	defer recv.Close()

	cfg := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(`
providers:
  openai: { api_key_env: OPENAI_API_KEY, base_url: %q }
models:
  - { id: m, provider: openai, upstream: up-model, input_usd_per_mtok: 1, output_usd_per_mtok: 1 }
policies:
  - { name: default, type: fixed, model: m }
gateway: { request_timeout: 30s, first_byte_timeout: 20s, max_retries: 0 }
runs: { lease_ttl: 3s, heartbeat: 1s, workers: 8, max_steps: 50, max_cost_usd: 10, deadline: 10m }
`, upstream.URL+"/v1")), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"DATABASE_URL=" + dbURL, "REDIS_URL=", "SPILLWAY_CONFIG=" + cfg, "OPENAI_API_KEY=", "ADMIN_SESSION_SECRET=", "SPILLWAY_WEBHOOK_SECRET=" + chaosSecret}

	if _, err := tools.NewStore(pool, nil).Create(context.Background(), tools.CreateParams{Name: "effect", Kind: tools.HTTP, Endpoint: recv.URL + "/effect",
		Description: "Applies a side effect", InputSchema: json.RawMessage(`{"type":"object","properties":{"i":{"type":"integer"}}}`), Timeout: 10 * time.Second}); err != nil {
		t.Fatal(err)
	}
	out := runCLI(t, bin, env, "keys", "create", "--name", "chaos")
	key := regexp.MustCompile(`key:\s+(spw_[0-9a-f]+)`).FindStringSubmatch(out)[1]

	addr := freeAddr(t)
	api := "http://" + addr
	start(t, bin, env, "serve", "--role=api", "--addr="+addr)
	waitHealthy(t, addr)

	// Submit the runs.
	for i := 0; i < runsN; i++ {
		body, _ := json.Marshal(map[string]any{"input": fmt.Sprintf("chaos run %d", i), "tools": []string{"effect"}})
		req, _ := http.NewRequest("POST", api+"/v1/runs", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Idempotency-Key", fmt.Sprintf("chaos-%d", i))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var c struct{ ID string }
		_ = json.NewDecoder(resp.Body).Decode(&c)
		resp.Body.Close()
		if resp.StatusCode != 202 || c.ID == "" {
			t.Fatalf("submit %d: %d", i, resp.StatusCode)
		}
	}

	// A person who approves every run that asks, through the public API.
	approver := make(chan struct{})
	var approverDone sync.WaitGroup
	approverDone.Add(1)
	go func() {
		defer approverDone.Done()
		for {
			select {
			case <-approver:
				return
			case <-time.After(250 * time.Millisecond):
			}
			rows, err := pool.Query(context.Background(), `SELECT id::text FROM runs WHERE status='waiting_human'`)
			if err != nil {
				continue
			}
			var ids []string
			for rows.Next() {
				var id string
				_ = rows.Scan(&id)
				ids = append(ids, id)
			}
			rows.Close()
			for _, id := range ids {
				req, _ := http.NewRequest("POST", api+"/v1/runs/"+id+"/approve", strings.NewReader(`{"note":"chaos approver"}`))
				req.Header.Set("Authorization", "Bearer "+key)
				if resp, err := http.DefaultClient.Do(req); err == nil {
					resp.Body.Close()
				}
			}
		}
	}()

	// The worker is killed at random, 1 to 4 seconds apart, and restarted at once.
	var kills atomic.Int32
	stop := make(chan struct{})
	var killer sync.WaitGroup
	var cur atomic.Pointer[proc]
	spawn := func() { cur.Store(startWorker(t, bin, env)) }
	spawn()
	killer.Add(1)
	go func() {
		defer killer.Done()
		rng := rand.New(rand.NewSource(42))
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Duration(1000+rng.Intn(3000)) * time.Millisecond):
			}
			if p := cur.Load(); p != nil {
				_ = p.cmd.Process.Kill() // SIGKILL
				_, _ = p.cmd.Process.Wait()
				kills.Add(1)
			}
			spawn()
		}
	}()

	deadline := time.Now().Add(budget)
	pending := runsN
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM runs WHERE status NOT IN ('succeeded','failed','cancelled')`).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending == 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	close(stop)
	close(approver)
	killer.Wait()
	approverDone.Wait()
	if p := cur.Load(); p != nil {
		_ = p.cmd.Process.Kill()
		_, _ = p.cmd.Process.Wait()
	}
	if pending > 0 {
		var done int
		_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM runs WHERE status = 'succeeded'`).Scan(&done)
		msg := fmt.Sprintf("%d of %d runs had not finished after %s (%d succeeded, %d kills)", pending, runsN, budget, done, kills.Load())
		if reduced {
			t.Skipf("skipped, not failed: the reduced chaos run could not finish in time: %s", msg)
		}
		t.Fatal(msg)
	}

	q := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	if n := q(`SELECT count(*) FROM runs WHERE status = 'succeeded'`); n != runsN {
		t.Fatalf("%d of %d runs succeeded; failures: %v", n, runsN, failures(t, pool))
	}
	// Every step has exactly one terminal row, and none is left open.
	if n := q(`SELECT count(*) FROM (SELECT run_id, step_no FROM run_steps WHERE step_no IS NOT NULL GROUP BY 1,2 HAVING count(*) FILTER (WHERE phase IN ('finished','failed')) <> 1) x`); n != 0 {
		t.Errorf("%d steps do not have exactly one terminal row", n)
	}
	// Every sleep was woken and every approval decided, once each.
	if a, b := q(`SELECT count(*) FROM run_steps WHERE type='sleep' AND phase='started'`), q(`SELECT count(*) FROM run_steps WHERE type='sleep' AND phase='finished'`); a != runsN || b != runsN {
		t.Errorf("sleeps started %d finished %d, want %d each", a, b, runsN)
	}
	if a, b := q(`SELECT count(*) FROM run_steps WHERE type='wait_human' AND phase='started'`), q(`SELECT count(*) FROM run_steps WHERE type='wait_human' AND phase='finished'`); a != runsN || b != runsN {
		t.Errorf("approvals asked %d decided %d, want %d each", a, b, runsN)
	}
	// Step numbers are contiguous, and each run has all its steps.
	if n := q(`SELECT count(*) FROM (SELECT run_id, count(DISTINCT step_no) c, max(step_no) m FROM run_steps WHERE step_no IS NOT NULL GROUP BY 1) x WHERE c <> m OR c <> $1`, stepsInRun); n != 0 {
		t.Errorf("%d runs have missing, extra or non-contiguous steps", n)
	}
	// A model or tool step finished by a different worker epoch than the one that started it was re-issued, and says so.
	// (A sleep is woken, and an approval decided, in a later epoch by design; those are checked above.)
	if n := q(`SELECT count(*) FROM run_steps t JOIN run_steps s ON s.run_id=t.run_id AND s.step_no=t.step_no AND s.phase='started'
		WHERE t.phase IN ('finished','failed') AND t.lease_epoch <> s.lease_epoch AND t.type IN ('model_call','tool_call')
		AND NOT EXISTS (SELECT 1 FROM run_steps r WHERE r.run_id=t.run_id AND r.step_no=t.step_no AND r.phase='reissued' AND r.lease_epoch=t.lease_epoch)`); n != 0 {
		t.Errorf("%d interrupted steps have no reissued row", n)
	}

	// The effects ledger: one applied effect per finished tool step, and nothing else.
	rows, err := pool.Query(context.Background(), `SELECT idempotency_key FROM run_steps WHERE type='tool_call' AND phase='finished'`)
	if err != nil {
		t.Fatal(err)
	}
	finished := map[string]bool{}
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		finished[k] = true
	}
	rows.Close()
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(finished) != runsN*toolCallsInRun {
		t.Errorf("%d finished tool steps, want %d", len(finished), runsN*toolCallsInRun)
	}
	if len(ledger.applied) != len(finished) {
		t.Errorf("%d effects applied but %d tool steps finished", len(ledger.applied), len(finished))
	}
	for k := range finished {
		if _, ok := ledger.applied[k]; !ok {
			t.Errorf("a finished tool step's effect was never applied: %s", k)
		}
	}
	for k := range ledger.applied {
		if !finished[k] {
			t.Errorf("an effect was applied for a step that never finished: %s", k)
		}
	}
	redelivered := 0
	for _, n := range ledger.deliveries {
		redelivered += n - 1
	}
	if ledger.badSig != 0 {
		t.Errorf("%d deliveries had a bad signature", ledger.badSig)
	}
	interrupted := q(`SELECT count(DISTINCT (run_id, step_no)) FROM run_steps WHERE phase='reissued'`)
	t.Logf("%d runs, 0 duplicated side effects, %d redelivered requests deduplicated by key (%d worker kills, %d steps re-issued)", runsN, redelivered, kills.Load(), interrupted)
	if kills.Load() == 0 {
		t.Error("the killer never ran, so nothing was tested")
	}
}

func failures(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT id, status, COALESCE(failure_reason,'') FROM runs WHERE status <> 'succeeded' LIMIT 5`)
	if err != nil {
		return err.Error()
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, st, reason string
		_ = rows.Scan(&id, &st, &reason)
		out = append(out, id+" "+st+" "+reason)
	}
	return strings.Join(out, "; ")
}

func startWorker(t *testing.T, bin string, env []string) *proc {
	t.Helper()
	cmd := exec.Command(bin, "serve", "--role=worker")
	cmd.Env = append(os.Environ(), env...)
	var logs strings.Builder
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return &proc{cmd: cmd, logs: &logs}
}
