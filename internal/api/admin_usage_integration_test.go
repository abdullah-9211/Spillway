//go:build integration

package api

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/auth"
	"github.com/abdullah-9211/spillway/internal/db"
	"github.com/abdullah-9211/spillway/internal/gateway"
	"github.com/abdullah-9211/spillway/internal/provider/fake"
	"github.com/abdullah-9211/spillway/internal/telemetry"
	"github.com/abdullah-9211/spillway/internal/usage"
)

// Real requests through the gateway, then the report read back over the admin API: the summary for the key must
// agree with what was sent, a cache hit must show up as savings, and a fallback must count.
func TestUsageReportFromRealTraffic(t *testing.T) {
	metrics := telemetry.NewMetrics()
	s := newStackWith(t, func(pg *db.Postgres) gateway.Options {
		return gateway.Options{Exact: newMemCache(), Observer: metrics}
	})
	s.prov.Default = fake.Behavior{Text: "ok", InputTokens: 1000, OutputTokens: 100} // $0.0045 on model m
	s.prov2.Default = fake.Behavior{Text: "second", InputTokens: 1000, OutputTokens: 100}

	signer, _ := auth.NewSigner([]byte(strings.Repeat("k", 32)))
	users := memUsers{"viewer": {ID: uuid.New(), Username: "viewer", Hash: mustHash(t, "pw"), Role: auth.RoleViewer}}
	admin := NewAdmin(AdminDeps{Users: users, Keys: s.keys, Usage: usage.NewReader(s.pool), Signer: signer, DummyHashParams: testHashParams,
		Health: s.gw.ProviderHealth, Latency: metrics})
	srv := httptest.NewServer(NewHandler(Options{Gateway: s.gw, Auth: s.keys, Admin: admin}))
	defer srv.Close()
	r := &adminRig{srv: srv, admin: admin}
	tok := r.token(t, "viewer", "pw")

	k, secret := s.newKey(t, fmt.Sprintf("report-%d", time.Now().UnixNano()))
	chat := func(model string, temp float64) *http.Response {
		body := fmt.Sprintf(`{"model":%q,"temperature":%v,"messages":[{"role":"user","content":"report me"}]}`, model, temp)
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	chat("m", 0) // miss: costs $0.0045 and fills the cache
	s.gw.WaitForFills()
	chat("m", 0) // exact hit: costs nothing, saves $0.0045
	s.prov.Default = fake.Behavior{Status: 500}
	chat("chain", 1) // m fails (after retries), m2 answers: one fallback; costs $0.0015 (1000*$1/M + 100*$2/M... see the catalog)

	// Wait for the buffered usage writer.
	var sum map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, sum = r.json(t, "GET", "/admin/usage/summary?key_id="+k.ID.String()+"&group_by=model", tok, "")
		if tot, ok := sum["totals"].(map[string]any); ok && tot["requests"].(float64) == 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	tot := sum["totals"].(map[string]any)
	if tot["requests"].(float64) != 3 || tot["cache_hits"].(float64) != 1 || tot["saved_usd"] != "0.004500" {
		t.Fatalf("totals = %v", tot)
	}
	if c := tot["cache"].(map[string]any); c["hit_exact"].(float64) != 1 || c["miss"].(float64) != 1 || c["bypass"].(float64) != 1 {
		t.Errorf("cache = %v (the temperature-1 request is not cacheable)", c)
	}
	if share := tot["saved_share"].(float64); share <= 0 || share >= 1 {
		t.Errorf("saved share = %v", share)
	}
	if sum["fallbacks_fired"].(float64) < 1 {
		t.Errorf("a fallback happened, got %v", sum["fallbacks_fired"])
	}
	if lat, ok := sum["added_latency"].(map[string]any); !ok || lat["samples"].(float64) < 1 {
		t.Errorf("added latency should have samples from the exact hit: %v", sum["added_latency"])
	}

	// Every figure agrees with the request log for the same key.
	_, log := r.json(t, "GET", "/admin/usage/requests?key_id="+k.ID.String(), tok, "")
	rows := log["requests"].([]any)
	if len(rows) != 3 {
		t.Fatalf("request log has %d rows", len(rows))
	}
	var withFallback bool
	for _, row := range rows {
		if a := row.(map[string]any)["attempts"].([]any); len(a) >= 2 {
			withFallback = true
		}
	}
	if !withFallback {
		t.Error("the request log should show the attempts of the request that fell back")
	}

	// CSV for the key.
	resp := r.do(t, "GET", "/admin/usage/export.csv?key_id="+k.ID.String(), tok, "")
	raw, _ := io.ReadAll(resp.Body)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if resp.StatusCode != 200 || len(lines) < 3 || !strings.HasPrefix(lines[0], "day,key,model,requests") {
		t.Errorf("csv: %d\n%s", resp.StatusCode, raw)
	}
}
