//go:build integration

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"net/http/httptest"

	"github.com/abdullah-9211/spillway/internal/db"
	"github.com/abdullah-9211/spillway/internal/db/sqlcgen"
	"github.com/abdullah-9211/spillway/internal/provider/fake"
)

func runCLI(t *testing.T, bin string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("spillway %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// TestGatewayEndToEnd drives the real binary: CLI creates a key, the gateway serves a chat through a fake
// provider, usage is written, and a SIGTERM shuts down cleanly with the usage flushed.
func TestGatewayEndToEnd(t *testing.T) {
	dbURL := requireEnv(t, "DATABASE_URL")
	bin := buildBinary(t)

	fp := fake.New("fake")
	fp.Default = fake.Behavior{Text: "Hello there", InputTokens: 1000, OutputTokens: 100}
	upstream := httptest.NewServer(fake.NewHandler(fp))
	defer upstream.Close()

	cfg := filepath.Join(t.TempDir(), "models.yaml")
	yaml := fmt.Sprintf(`
providers:
  openai: { api_key_env: OPENAI_API_KEY, base_url: %q }
models:
  - { id: m, provider: openai, upstream: up-model, input_usd_per_mtok: 3, output_usd_per_mtok: 15 }
`, upstream.URL+"/v1")
	if err := os.WriteFile(cfg, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"DATABASE_URL=" + dbURL, "REDIS_URL=", "SPILLWAY_CONFIG=" + cfg, "OPENAI_API_KEY="}

	runCLI(t, bin, env, "migrate")
	out := runCLI(t, bin, env, "keys", "create", "--name", "e2e", "--rpm", "30", "--budget-usd", "2.50")
	m := regexp.MustCompile(`key:\s+(spw_[0-9a-f]+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no key in output:\n%s", out)
	}
	key := m[1]
	if list := runCLI(t, bin, env, "keys", "list"); !strings.Contains(list, "e2e") || !strings.Contains(list, "2.500000") || !strings.Contains(list, key[:8]) {
		t.Errorf("keys list:\n%s", list)
	}

	addr := freeAddr(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	srv := exec.CommandContext(ctx, bin, "serve", "--addr="+addr)
	srv.Env = append(os.Environ(), env...)
	var logs strings.Builder
	srv.Stderr = &logs
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- srv.Wait() }()
	defer func() { _ = srv.Process.Kill() }()

	waitHealthy(t, addr)

	post := func(token string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}

	resp := post(key)
	if resp.StatusCode != 200 || resp.Header.Get("X-Spillway-Cost-Usd") != "0.004500" {
		t.Fatalf("status %d, cost header %q\n%s", resp.StatusCode, resp.Header.Get("X-Spillway-Cost-Usd"), logs.String())
	}
	id := uuid.MustParse(resp.Header.Get("X-Spillway-Request-Id"))

	// Shut down straight away: the usage row must still arrive, because the writer drains on shutdown.
	_ = srv.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("server did not exit cleanly: %v\n%s", err, logs.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("server did not exit after SIGTERM")
	}

	pg, err := db.OpenPostgres(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	row, err := sqlcgen.New(pg.Pool).GetUsage(context.Background(), id)
	if err != nil {
		t.Fatalf("usage row missing after shutdown: %v\n%s", err, logs.String())
	}
	cost, _ := db.MicrosFromNumeric(row.CostUsd)
	if row.InputTokens != 1000 || row.OutputTokens != 100 || cost != 4500 || row.Outcome != "ok" {
		t.Errorf("row: %+v cost=%d", row, cost)
	}

	// Revoke through the CLI and confirm a fresh server rejects the key.
	runCLI(t, bin, env, "keys", "revoke", key[:8])
	addr2 := freeAddr(t)
	srv2 := exec.CommandContext(ctx, bin, "serve", "--addr="+addr2)
	srv2.Env = append(os.Environ(), env...)
	if err := srv2.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv2.Process.Kill(); _ = srv2.Wait() }()
	waitHealthy(t, addr2)
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr2+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != 401 {
		t.Errorf("revoked key: status %d, want 401", r2.StatusCode)
	}
}

func waitHealthy(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get("http://" + addr + "/healthz"); err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("server never became healthy")
}

func TestKeysCommandErrors(t *testing.T) {
	dbURL := requireEnv(t, "DATABASE_URL")
	bin := buildBinary(t)
	for name, args := range map[string][]string{
		"create without a name": {"keys", "create"},
		"bad budget":            {"keys", "create", "--name", "x", "--budget-usd", "lots"},
		"revoke without an id":  {"keys", "revoke"},
		"revoke unknown":        {"keys", "revoke", "nope"},
		"unknown subcommand":    {"keys", "frobnicate"},
	} {
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "DATABASE_URL="+dbURL)
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Errorf("%s: expected a non-zero exit, got:\n%s", name, out)
		}
	}
}
