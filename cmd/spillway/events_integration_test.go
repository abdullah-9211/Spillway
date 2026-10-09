//go:build integration

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/abdullah-9211/spillway/internal/provider/fake"
	"github.com/abdullah-9211/spillway/internal/testdb"
)

type sseLine struct {
	id    int64
	event string
}

// readEvents reads events from an SSE response until the stream ends, the context ends, or stopAfter events have come.
func readEvents(ctx context.Context, t *testing.T, url, key, lastID string, stopAfter int) (events []sseLine, ended bool) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("events: %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	var cur sseLine
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			cur.id, _ = strconv.ParseInt(line[4:], 10, 64)
		case strings.HasPrefix(line, "event: "):
			cur.event = line[7:]
		case line == "" && cur.event != "":
			events = append(events, cur)
			cur = sseLine{}
			if stopAfter > 0 && len(events) >= stopAfter {
				return events, false
			}
		}
	}
	return events, ctx.Err() == nil
}

// TestRolesAreSeparateAndTheEventStreamResumes runs the API and a worker as separate processes. With only the API up a
// run waits, queued, and its event stream shows that. A client drops, the worker starts, and the client reconnects with
// Last-Event-ID: it gets exactly the rows it missed, in order, to the end of the run, and the stream then closes.
func TestRolesAreSeparateAndTheEventStreamResumes(t *testing.T) {
	_, dbURL := testdb.New(t)
	bin := buildBinary(t)

	fp := fake.New("fake")
	fp.Default = fake.Behavior{Text: "streamed answer", Delay: time.Second}
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
runs: { lease_ttl: 6s, heartbeat: 2s, workers: 2 }
`, upstream.URL+"/v1")), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"DATABASE_URL=" + dbURL, "REDIS_URL=", "SPILLWAY_CONFIG=" + cfg, "OPENAI_API_KEY=", "ADMIN_SESSION_SECRET="}
	out := runCLI(t, bin, env, "keys", "create", "--name", "events")
	key := regexp.MustCompile(`key:\s+(spw_[0-9a-f]+)`).FindStringSubmatch(out)[1]

	addr := freeAddr(t)
	url := "http://" + addr
	start(t, bin, env, "serve", "--role=api", "--addr="+addr)
	waitHealthy(t, addr)

	cli := func(args ...string) string {
		t.Helper()
		return runCLI(t, bin, nil, append([]string{"runs"}, append(args, "--url", url, "--key", key)...)...)
	}
	var c struct{ ID string }
	_ = json.Unmarshal([]byte(cli("create", "--json", "stream me")), &c)

	// Only the API is running: nobody claims the run.
	time.Sleep(2 * time.Second)
	if st := runStatus(t, cli("get", "--json", c.ID)); st != "queued" {
		t.Fatalf("with no worker the run must stay queued, got %s", st)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	first, _ := readEvents(ctx, t, url+"/v1/runs/"+c.ID+"/events", key, "", 1)
	if len(first) != 1 || first[0].event != "run.status" {
		t.Fatalf("the stream starts with the queued status: %+v", first)
	}

	// The client has dropped. Now a worker process starts and the run proceeds while nobody is watching.
	start(t, bin, env, "serve", "--role=worker")
	eventually(t, "the run to finish with no watcher", 30*time.Second, func() bool { return runStatus(t, cli("get", "--json", c.ID)) == "succeeded" })

	// Reconnect from the last id seen: exactly the missed rows, in order, then the stream closes by itself.
	rest, ended := readEvents(ctx, t, url+"/v1/runs/"+c.ID+"/events", key, strconv.FormatInt(first[0].id, 10), 0)
	if !ended {
		t.Error("the stream should end after the final run.status")
	}
	var names []string
	prev := first[0].id
	for _, e := range rest {
		if e.id <= prev {
			t.Errorf("ids must increase: %d after %d", e.id, prev)
		}
		prev = e.id
		names = append(names, e.event)
	}
	want := "run.status,step.started,step.finished,run.status" // running, model started, model finished, succeeded
	if strings.Join(names, ",") != want {
		t.Errorf("resumed events = %v, want %s", names, want)
	}

	// The whole history from the beginning is the first row plus the resumed ones: nothing lost, nothing repeated.
	all, _ := readEvents(ctx, t, url+"/v1/runs/"+c.ID+"/events", key, "0", 0)
	if len(all) != len(rest)+1 || all[0].id != first[0].id {
		t.Errorf("full replay has %d events, want %d", len(all), len(rest)+1)
	}
	// Another key cannot watch it.
	out2 := runCLI(t, bin, env, "keys", "create", "--name", "someone-else")
	key2 := regexp.MustCompile(`key:\s+(spw_[0-9a-f]+)`).FindStringSubmatch(out2)[1]
	req, _ := http.NewRequest("GET", url+"/v1/runs/"+c.ID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+key2)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 404 {
		t.Errorf("another key: %v %v", resp, err)
	}
}
