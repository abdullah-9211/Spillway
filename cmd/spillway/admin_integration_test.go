//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestAdminSignInEndToEnd seeds the two accounts with the CLI, starts the real server, signs in as each, and
// checks the roles and the throttle over real HTTP.
func TestAdminSignInEndToEnd(t *testing.T) {
	dbURL := requireEnv(t, "DATABASE_URL")
	bin := buildBinary(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	adminName, viewerName := "e2e-admin-"+suffix, "e2e-viewer-"+suffix
	env := []string{
		"DATABASE_URL=" + dbURL, "SPILLWAY_CONFIG=../../config/models.yaml", "ADMIN_SESSION_SECRET=" + strings.Repeat("e", 40),
		"SEED_ADMIN_USER=" + adminName, "SEED_ADMIN_PASSWORD=admin-secret-1",
		"SEED_VIEWER_USER=" + viewerName, "SEED_VIEWER_PASSWORD=viewer-secret-1",
	}

	runCLI(t, bin, env, "migrate")
	out := runCLI(t, bin, env, "seed")
	if !strings.Contains(out, adminName) || !strings.Contains(out, viewerName) {
		t.Errorf("seed output:\n%s", out)
	}
	// Seeding again leaves existing accounts alone.
	if again := runCLI(t, bin, env, "seed"); strings.Contains(again, "seeded user") {
		t.Errorf("a second seed must not change anything:\n%s", again)
	}

	addr := freeAddr(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	srv := exec.CommandContext(ctx, bin, "serve", "--addr="+addr)
	srv.Env = append(os.Environ(), env...)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Process.Kill(); _ = srv.Wait() }()
	waitHealthy(t, addr)

	call := func(method, path, token, body string, hdr ...string) (int, map[string]any, http.Header) {
		req, _ := http.NewRequest(method, "http://"+addr+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m, resp.Header
	}
	login := func(user, pw string, hdr ...string) (int, map[string]any, http.Header) {
		b, _ := json.Marshal(map[string]string{"username": user, "password": pw})
		return call("POST", "/admin/login", "", string(b), hdr...)
	}

	for user, want := range map[string]string{adminName: "admin", viewerName: "viewer"} {
		pw := map[string]string{adminName: "admin-secret-1", viewerName: "viewer-secret-1"}[user]
		code, body, _ := login(user, pw)
		if code != 200 {
			t.Fatalf("%s: %d %v", user, code, body)
		}
		code, me, _ := call("GET", "/admin/me", body["token"].(string), "")
		if code != 200 || me["role"] != want || me["username"] != user {
			t.Errorf("%s: /admin/me = %d %v", user, code, me)
		}
	}

	// Five bad logins in a minute are throttled.
	for i := 0; i < 5; i++ {
		if code, _, _ := login(adminName, "wrong", "X-Forwarded-For", "10.9.8.7"); code != 401 {
			t.Fatalf("bad login %d: %d", i+1, code)
		}
	}
	code, _, hdr := login(adminName, "admin-secret-1", "X-Forwarded-For", "10.9.8.7")
	if code != 429 || hdr.Get("Retry-After") == "" {
		t.Errorf("sixth login: %d, Retry-After %q", code, hdr.Get("Retry-After"))
	}
}

func TestAdminAPIIsOffWithoutASecret(t *testing.T) {
	dbURL := requireEnv(t, "DATABASE_URL")
	bin := buildBinary(t)
	addr := freeAddr(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv := exec.CommandContext(ctx, bin, "serve", "--addr="+addr)
	srv.Env = append(os.Environ(), "DATABASE_URL="+dbURL, "SPILLWAY_CONFIG=../../config/models.yaml", "ADMIN_SESSION_SECRET=")
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Process.Kill(); _ = srv.Wait() }()
	waitHealthy(t, addr)
	resp, err := http.Post("http://"+addr+"/admin/login", "application/json", strings.NewReader(`{"username":"a","password":"b"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("without ADMIN_SESSION_SECRET the admin routes must not exist, got %d", resp.StatusCode)
	}
}
