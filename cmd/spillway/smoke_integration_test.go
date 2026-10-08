//go:build integration

package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// buildBinary compiles the real binary so the test exercises flags, env and signal handling.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "spillway")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

func requireEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("%s is not set; run `make up` and use `make test`", key)
	}
	return v
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

type healthBody struct {
	Status       string            `json:"status"`
	Dependencies map[string]string `json:"dependencies"`
}

func startAndGetHealth(t *testing.T, bin string, env []string) (int, healthBody) {
	t.Helper()
	addr := freeAddr(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "serve", "--addr="+addr)
	// Tests run from cmd/spillway, so point at the shipped catalog explicitly.
	cmd.Env = append(append(os.Environ(), "SPILLWAY_CONFIG=../../config/models.yaml"), env...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			defer resp.Body.Close()
			var b healthBody
			if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
				t.Fatal(err)
			}
			return resp.StatusCode, b
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("server never answered /healthz")
	return 0, healthBody{}
}

func TestBinaryBootsAgainstPostgresAndRedis(t *testing.T) {
	dbURL := requireEnv(t, "DATABASE_URL")
	redisURL := requireEnv(t, "REDIS_URL")
	bin := buildBinary(t)

	// Migrate first, and twice: the second run must be a no-op.
	for i := 0; i < 2; i++ {
		cmd := exec.Command(bin, "migrate")
		cmd.Env = append(os.Environ(), "DATABASE_URL="+dbURL)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("migrate run %d: %v\n%s", i+1, err, out)
		}
	}

	code, h := startAndGetHealth(t, bin, []string{"DATABASE_URL=" + dbURL, "REDIS_URL=" + redisURL})
	if code != 200 || h.Status != "ok" || h.Dependencies["postgres"] != "up" || h.Dependencies["redis"] != "up" {
		t.Fatalf("got %d %+v", code, h)
	}
}

func TestBinaryStartsWithoutRedis(t *testing.T) {
	dbURL := requireEnv(t, "DATABASE_URL")
	bin := buildBinary(t)

	code, h := startAndGetHealth(t, bin, []string{"DATABASE_URL=" + dbURL, "REDIS_URL="})
	if code != 200 || h.Status != "ok" {
		t.Fatalf("got %d %+v", code, h)
	}
	if _, ok := h.Dependencies["redis"]; ok {
		t.Errorf("redis should not be reported when disabled: %+v", h.Dependencies)
	}
}

func TestBinaryRefusesToStartWithoutDatabase(t *testing.T) {
	bin := buildBinary(t)
	cmd := exec.Command(bin, "serve")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	if err := cmd.Run(); err == nil {
		t.Fatal("expected non-zero exit without DATABASE_URL")
	}
}
