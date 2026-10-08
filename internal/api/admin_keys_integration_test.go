//go:build integration

package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/auth"
	"github.com/abdullah-9211/spillway/internal/db"
	"github.com/abdullah-9211/spillway/internal/db/sqlcgen"
	"github.com/abdullah-9211/spillway/internal/gateway"
	"github.com/abdullah-9211/spillway/internal/provider/fake"
)

// TestKeyLifecycleOverHTTP creates a key through the admin API, uses it on the gateway, watches its spend, changes
// its budget, and revokes it, all against real Postgres.
func TestKeyLifecycleOverHTTP(t *testing.T) {
	s := newStackWith(t, func(pg *db.Postgres) gateway.Options {
		return gateway.Options{Spend: gateway.NewPGSpend(sqlcgen.New(pg.Pool))}
	})
	s.prov.Default = fake.Behavior{Text: "ok", InputTokens: 1000, OutputTokens: 100} // $0.0045 a request

	signer, _ := auth.NewSigner([]byte(strings.Repeat("k", 32)))
	users := memUsers{
		"admin":  {ID: uuid.New(), Username: "admin", Hash: mustHash(t, "pw-admin"), Role: auth.RoleAdmin},
		"viewer": {ID: uuid.New(), Username: "viewer", Hash: mustHash(t, "pw-viewer"), Role: auth.RoleViewer},
	}
	admin := NewAdmin(AdminDeps{Users: users, Keys: s.keys, Signer: signer, DummyHashParams: testHashParams})
	srv := httptest.NewServer(NewHandler(Options{Gateway: s.gw, Auth: s.keys, Admin: admin}))
	defer srv.Close()
	r := &adminRig{srv: srv, admin: admin}
	adminTok, viewerTok := r.token(t, "admin", "pw-admin"), r.token(t, "viewer", "pw-viewer")

	name := fmt.Sprintf("lifecycle-%d", time.Now().UnixNano())
	code, body := r.json(t, "POST", "/admin/keys", adminTok, `{"name":"`+name+`","rate_limit_rpm":100,"monthly_budget_usd":"1","semantic_cache":true}`)
	if code != 201 {
		t.Fatalf("create: %d %v", code, body)
	}
	secret := body["secret"].(string)
	id := body["key"].(map[string]any)["id"].(string)

	chat := func() int {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	find := func(token string) map[string]any {
		_, list := r.json(t, "GET", "/admin/keys", token, "")
		for _, k := range list["keys"].([]any) {
			if k.(map[string]any)["id"] == id {
				return k.(map[string]any)
			}
		}
		t.Fatalf("key %s missing from the list", id)
		return nil
	}

	if got := find(adminTok); got["spend_usd"] != "0.000000" || got["last_used_at"] != nil || got["semantic_cache"] != true {
		t.Fatalf("a new key has not spent anything: %v", got)
	}

	// Use it, then watch the spend and last-used time appear once the buffered usage row is written.
	if code := chat(); code != 200 {
		t.Fatalf("a new key should work on the gateway: %d", code)
	}
	deadline := time.Now().Add(5 * time.Second)
	var row map[string]any
	for time.Now().Before(deadline) {
		if row = find(viewerTok); row["spend_usd"] == "0.004500" && row["last_used_at"] != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if row["spend_usd"] != "0.004500" || row["last_used_at"] == nil {
		t.Fatalf("spend and last use should show up: %v", row)
	}
	for _, v := range row {
		if v == secret {
			t.Fatal("the list must never contain the key")
		}
	}

	// Lower the budget to exactly what it has spent: the next request is refused. Lifting it restores access.
	if code, b := r.json(t, "PATCH", "/admin/keys/"+id, adminTok, `{"monthly_budget_usd":"0.0045"}`); code != 200 {
		t.Fatalf("patch: %d %v", code, b)
	}
	if code := chat(); code != 402 {
		t.Errorf("at the budget the gateway should answer 402, got %d", code)
	}
	r.json(t, "PATCH", "/admin/keys/"+id, adminTok, `{"monthly_budget_usd":null}`)
	if code := chat(); code != 200 {
		t.Errorf("with no budget the key works again, got %d", code)
	}

	// A viewer can see it but not touch it.
	if code, _ := r.json(t, "DELETE", "/admin/keys/"+id, viewerTok, ""); code != 403 {
		t.Errorf("viewer revoke: %d", code)
	}
	if code := chat(); code != 200 {
		t.Errorf("a refused revoke must leave the key working, got %d", code)
	}

	// Revoke: the very next request is refused, and the list says so.
	if code, b := r.json(t, "DELETE", "/admin/keys/"+id, adminTok, ""); code != 200 || b["revoked_at"] == nil {
		t.Fatalf("revoke: %d %v", code, b)
	}
	if code := chat(); code != 401 {
		t.Errorf("a revoked key must get 401, got %d", code)
	}
	if got := find(adminTok); got["revoked_at"] == nil {
		t.Errorf("list should show it revoked: %v", got)
	}
	if code, _ := r.json(t, "PATCH", "/admin/keys/"+id, adminTok, `{"name":"x"}`); code != 409 {
		t.Errorf("editing a revoked key: %d", code)
	}
	if code, _ := r.json(t, "POST", "/admin/keys", adminTok, `{"name":"playground"}`); code != 400 {
		t.Errorf("the playground name is reserved: %d", code)
	}
}
