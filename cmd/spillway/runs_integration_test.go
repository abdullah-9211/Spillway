//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/abdullah-9211/spillway/internal/provider/fake"
	"github.com/abdullah-9211/spillway/internal/testdb"
)

type proc struct {
	cmd  *exec.Cmd
	logs *strings.Builder
}

func start(t *testing.T, bin string, env []string, args ...string) *proc {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	var logs strings.Builder
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &proc{cmd: cmd, logs: &logs}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return p
}

func eventually(t *testing.T, what string, d time.Duration, f func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if f() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type stepRow struct {
	ID         int64           `json:"id"`
	StepNo     *int            `json:"step_no"`
	Type       string          `json:"type"`
	Phase      string          `json:"phase"`
	Payload    json.RawMessage `json:"payload"`
	WorkerID   string          `json:"worker_id"`
	LeaseEpoch int64           `json:"lease_epoch"`
}

// TestKilledWorkerIsReplacedAndTheRunFinishes is the crash-recovery demo with real processes: one API process and one
// worker process. The worker is killed with SIGKILL while a model call is in flight; a second worker process takes the
// run over once the lease expires, re-issues the step, and the run succeeds. The step rows say who did what.
func TestKilledWorkerIsReplacedAndTheRunFinishes(t *testing.T) {
	pool, dbURL := testdb.New(t)
	bin := buildBinary(t)

	fp := fake.New("fake")
	fp.Default = fake.Behavior{Text: "recovered answer", InputTokens: 1000, OutputTokens: 100, Delay: 4 * time.Second}
	upstream := httptest.NewServer(fake.NewHandler(fp))
	defer upstream.Close()

	cfg := filepath.Join(t.TempDir(), "models.yaml")
	yaml := fmt.Sprintf(`
providers:
  openai: { api_key_env: OPENAI_API_KEY, base_url: %q }
models:
  - { id: m, provider: openai, upstream: up-model, input_usd_per_mtok: 3, output_usd_per_mtok: 15 }
policies:
  - { name: default, type: fixed, model: m }
gateway: { request_timeout: 30s, first_byte_timeout: 20s, max_retries: 0 }
runs: { lease_ttl: 3s, heartbeat: 1s, workers: 2, max_steps: 10, max_cost_usd: 1, deadline: 2m }
`, upstream.URL+"/v1")
	if err := os.WriteFile(cfg, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"DATABASE_URL=" + dbURL, "REDIS_URL=", "SPILLWAY_CONFIG=" + cfg, "OPENAI_API_KEY=", "ADMIN_SESSION_SECRET="}

	out := runCLI(t, bin, env, "keys", "create", "--name", "runs-e2e")
	key := regexp.MustCompile(`key:\s+(spw_[0-9a-f]+)`).FindStringSubmatch(out)[1]

	addr := freeAddr(t)
	url := "http://" + addr
	start(t, bin, env, "serve", "--role=api", "--addr="+addr)
	eventually(t, "the API to listen", 15*time.Second, func() bool {
		o, err := exec.Command(bin, "runs", "get", "--url", url, "--key", key, "00000000-0000-0000-0000-000000000000").CombinedOutput()
		return err != nil && strings.Contains(string(o), "404") // the service answered
	})

	workerA := start(t, bin, env, "serve", "--role=worker")

	cli := func(args ...string) string {
		t.Helper()
		return runCLI(t, bin, nil, append([]string{"runs"}, append(args, "--url", url, "--key", key)...)...)
	}
	created := cli("create", "--json", "--idempotency-key", "demo", "say hello")
	var c struct{ ID string }
	if err := json.Unmarshal([]byte(created), &c); err != nil || c.ID == "" {
		t.Fatalf("create: %v %s", err, created)
	}
	steps := func() []stepRow {
		var page struct{ Steps []stepRow }
		_ = json.Unmarshal([]byte(cli("steps", "--json", c.ID)), &page)
		return page.Steps
	}

	// Wait until worker A has started the model call, then kill it without any warning.
	var firstWorker string
	eventually(t, "worker A to start the model call", 20*time.Second, func() bool {
		for _, s := range steps() {
			if s.Type == "model_call" && s.Phase == "started" {
				firstWorker = s.WorkerID
				return true
			}
		}
		return false
	})
	if err := workerA.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_, _ = workerA.cmd.Process.Wait()
	killedAt := time.Now()

	// Nothing else is running, so the run cannot finish yet.
	time.Sleep(500 * time.Millisecond)
	if st := runStatus(t, cli("get", "--json", c.ID)); st == "succeeded" {
		t.Fatal("the run finished with its worker dead")
	}

	start(t, bin, env, "serve", "--role=worker")
	eventually(t, "worker B to finish the run", 40*time.Second, func() bool { return runStatus(t, cli("get", "--json", c.ID)) == "succeeded" })
	t.Logf("recovered %s after the kill", time.Since(killedAt).Round(100*time.Millisecond))

	var got []string
	var reissued, finished stepRow
	for _, s := range steps() {
		got = append(got, fmt.Sprintf("%s/%s/%s/e%d", s.Type, s.Phase, s.WorkerID, s.LeaseEpoch))
		switch {
		case s.Phase == "reissued":
			reissued = s
		case s.Type == "model_call" && s.Phase == "finished":
			finished = s
		}
	}
	if reissued.ID == 0 || finished.ID == 0 {
		t.Fatalf("expected a reissued and a finished row, got %v", got)
	}
	if reissued.WorkerID == firstWorker || reissued.WorkerID == "" || reissued.LeaseEpoch != 2 || finished.WorkerID != reissued.WorkerID || finished.LeaseEpoch != 2 {
		t.Errorf("the second worker should have a different id and epoch 2: %v", got)
	}
	var p struct {
		PreviousWorker string `json:"previous_worker"`
		PreviousEpoch  int64  `json:"previous_epoch"`
	}
	_ = json.Unmarshal(reissued.Payload, &p)
	if p.PreviousWorker != firstWorker || p.PreviousEpoch != 1 {
		t.Errorf("reissued payload = %s, want previous_worker %s epoch 1", reissued.Payload, firstWorker)
	}
	final := cli("get", c.ID)
	if !strings.Contains(final, "status:   succeeded") || !strings.Contains(final, "recovered answer") || !strings.Contains(final, "steps:    1") {
		t.Errorf("final run:\n%s", final)
	}

	// The model call went through the gateway in-process: its usage row is linked to the run and charged to the key,
	// and the run's cost is that row's cost. (The killed worker's request never wrote one.)
	var n int
	var usageCost, runCost string
	eventually(t, "the usage row to be flushed", 10*time.Second, func() bool { // usage rows are written in the background
		err := pool.QueryRow(context.Background(), `SELECT count(*), COALESCE(sum(u.cost_usd),0)::text, COALESCE(max(r.cost_usd),0)::text FROM usage u JOIN runs r ON r.id = u.run_id WHERE u.run_id = $1 AND u.outcome = 'ok'`, c.ID).Scan(&n, &usageCost, &runCost)
		return err == nil && n > 0
	})
	if n != 1 || usageCost != runCost || usageCost == "0" {
		t.Errorf("usage rows linked to the run: %d, cost %s, run cost %s", n, usageCost, runCost)
	}

	// The same Idempotency-Key returns the same run rather than starting a second one.
	again := cli("create", "--json", "--idempotency-key", "demo", "say hello")
	var c2 struct{ ID string }
	_ = json.Unmarshal([]byte(again), &c2)
	if c2.ID != c.ID {
		t.Errorf("idempotent create returned %s, want %s", c2.ID, c.ID)
	}
}

func runStatus(t *testing.T, js string) string {
	t.Helper()
	var r struct{ Status string }
	if err := json.Unmarshal([]byte(js), &r); err != nil {
		t.Fatalf("%v: %s", err, js)
	}
	return r.Status
}
