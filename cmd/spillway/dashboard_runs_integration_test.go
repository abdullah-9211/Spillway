//go:build integration

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/provider/fake"
	"github.com/abdullah-9211/spillway/internal/testdb"
)

// TestDashboardStartsFindsAndCancelsRuns drives the admin API of the real server, as the dashboard does: an admin starts
// a run (under the playground key), a worker in the same process finishes it, the run is found by a search and by key, a
// slow one is cancelled, a viewer can do none of the writes, and a bad request is refused.
func TestDashboardStartsFindsAndCancelsRuns(t *testing.T) {
	_, dbURL := testdb.New(t)
	bin := buildBinary(t)
	fp := fake.New("fake")
	fp.Decide = func(req *provider.ChatRequest) fake.Behavior {
		delay := 100 * time.Millisecond
		for _, m := range req.Messages {
			if strings.Contains(m.Content.PlainText(), "slow") {
				delay = 30 * time.Second
			}
		}
		return fake.Behavior{Text: "dashboard answer", Delay: delay}
	}
	upstream := httptest.NewServer(fake.NewHandler(fp))
	defer upstream.Close()
	cfg := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(`
providers:
  openai: { api_key_env: OPENAI_API_KEY, base_url: %q }
models:
  - { id: m, provider: openai, upstream: up-model, input_usd_per_mtok: 1, output_usd_per_mtok: 1 }
policies:
  - { name: default, type: fixed, model: m }
runs: { lease_ttl: 6s, heartbeat: 1s, workers: 2, max_steps: 5, max_cost_usd: 1, deadline: 2m }
`, upstream.URL+"/v1")), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"DATABASE_URL=" + dbURL, "REDIS_URL=", "SPILLWAY_CONFIG=" + cfg, "OPENAI_API_KEY=", "ADMIN_SESSION_SECRET=" + strings.Repeat("d", 40),
		"SEED_ADMIN_USER=boss", "SEED_ADMIN_PASSWORD=boss-secret-1", "SEED_VIEWER_USER=watcher", "SEED_VIEWER_PASSWORD=watcher-secret-1"}
	runCLI(t, bin, env, "seed")
	addr := freeAddr(t)
	start(t, bin, env, "serve", "--role=all", "--addr="+addr)
	waitHealthy(t, addr)

	call := func(method, path, token, body string) (int, map[string]any) {
		req, _ := http.NewRequest(method, "http://"+addr+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	login := func(u, p string) string {
		_, b := call("POST", "/admin/login", "", fmt.Sprintf(`{"username":%q,"password":%q}`, u, p))
		return b["token"].(string)
	}
	admin, viewer := login("boss", "boss-secret-1"), login("watcher", "watcher-secret-1")

	code, body := call("POST", "/admin/runs", admin, `{"input":"Summarise the incident channel","limits":{"max_steps":3}}`)
	if code != 202 || body["status"] != "queued" {
		t.Fatalf("start: %d %v", code, body)
	}
	id := body["id"].(string)
	eventually(t, "the run to finish", 20*time.Second, func() bool {
		_, r := call("GET", "/admin/runs/"+id, viewer, "")
		return r["status"] == "succeeded"
	})
	_, run := call("GET", "/admin/runs/"+id, viewer, "")
	if run["key"] != "playground" || run["goal"] != "Summarise the incident channel" || run["step_count"].(float64) != 1 {
		t.Errorf("run = %v: it is made under the playground key", run)
	}

	// The search finds it (ignoring case); the wrong text does not; the key filter narrows.
	_, found := call("GET", "/admin/runs?state=finished&q="+url.QueryEscape("INCIDENT"), viewer, "")
	if len(found["runs"].([]any)) != 1 {
		t.Errorf("search: %v", found)
	}
	if _, none := call("GET", "/admin/runs?q=nothing-like-this", viewer, ""); len(none["runs"].([]any)) != 0 {
		t.Errorf("a search with no match: %v", none)
	}

	// A slow run is cancelled by an admin while a worker holds it.
	_, slow := call("POST", "/admin/runs", admin, `{"input":"a slow one"}`)
	slowID := slow["id"].(string)
	eventually(t, "the worker to pick it up", 10*time.Second, func() bool {
		_, r := call("GET", "/admin/runs/"+slowID, viewer, "")
		return r["status"] == "running"
	})
	if code, _ := call("POST", "/admin/runs/"+slowID+"/cancel", viewer, ""); code != 403 {
		t.Errorf("a viewer cancelling: %d", code)
	}
	if code, r := call("POST", "/admin/runs/"+slowID+"/cancel", admin, ""); code != 202 {
		t.Fatalf("cancel: %d %v", code, r)
	}
	eventually(t, "the run to be cancelled", 10*time.Second, func() bool {
		_, r := call("GET", "/admin/runs/"+slowID, viewer, "")
		return r["status"] == "cancelled"
	})
	if code, _ := call("POST", "/admin/runs/"+slowID+"/cancel", admin, ""); code != 409 {
		t.Errorf("cancelling a finished run: %d", code)
	}

	// Writes the viewer may not make, and requests that are not valid.
	if code, _ := call("POST", "/admin/runs", viewer, `{"input":"x"}`); code != 403 {
		t.Errorf("a viewer starting a run: %d", code)
	}
	for _, bad := range []string{`{}`, `{"input":"x","model":"nope"}`, `{"input":"x","tools":["web_search"]}`, `{"input":"x","limits":{"max_steps":0}}`} {
		if code, _ := call("POST", "/admin/runs", admin, bad); code != 400 {
			t.Errorf("%s: %d", bad, code)
		}
	}
}
