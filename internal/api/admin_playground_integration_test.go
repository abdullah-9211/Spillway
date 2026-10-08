//go:build integration

package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/auth"
	"github.com/abdullah-9211/spillway/internal/db"
	"github.com/abdullah-9211/spillway/internal/gateway"
	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/money"
	"github.com/abdullah-9211/spillway/internal/playground"
	"github.com/abdullah-9211/spillway/internal/provider/fake"
)

// TestPlaygroundAgainstRealStores runs the playground through the real gateway, key store and database: faults show up
// as ordered attempts, the history survives in Postgres, and the public /v1 endpoint ignores a faults field.
func TestPlaygroundAgainstRealStores(t *testing.T) {
	s := newStackWith(t, func(pg *db.Postgres) gateway.Options { return gateway.Options{FaultInjection: true} })
	s.prov.Default = fake.Behavior{Text: "from m"}
	s.prov2.Default = fake.Behavior{Text: "from m2"}

	rpm, budget := 1000, money.Micros(50_000_000)
	pkey, err := s.keys.EnsureBuiltin(context.Background(), &rpm, &budget)
	if err != nil {
		t.Fatal(err)
	}
	// Idempotent: a second start finds the same key.
	if again, err := s.keys.EnsureBuiltin(context.Background(), &rpm, &budget); err != nil || again.ID != pkey.ID {
		t.Fatalf("EnsureBuiltin twice: %v %v", again.ID, err)
	}
	svc := playground.NewService(s.gw, func(context.Context) (keys.Key, error) { return pkey, nil }, playground.NewStore(s.pool), true, slog.New(slog.NewTextHandler(io.Discard, nil)))

	signer, _ := auth.NewSigner([]byte(strings.Repeat("k", 32)))
	users := memUsers{
		"admin":  {ID: uuid.New(), Username: "admin", Hash: mustHash(t, "pw-admin"), Role: auth.RoleAdmin},
		"viewer": {ID: uuid.New(), Username: "viewer", Hash: mustHash(t, "pw-viewer"), Role: auth.RoleViewer},
	}
	admin := NewAdmin(AdminDeps{Users: users, Keys: s.keys, Signer: signer, DummyHashParams: testHashParams,
		Playground: &PlaygroundDeps{Service: svc, Catalog: s.gw.Catalog, Key: func(ctx context.Context, now time.Time) (keys.Stats, error) { return s.keys.Stats(ctx, pkey.ID, now) }}})
	srv := httptest.NewServer(NewHandler(Options{Gateway: s.gw, Auth: s.keys, Admin: admin}))
	defer srv.Close()
	r := &adminRig{srv: srv, admin: admin}
	adminTok, viewerTok := r.token(t, "admin", "pw-admin"), r.token(t, "viewer", "pw-viewer")

	code, body := r.json(t, "POST", "/admin/playground/chat", adminTok, `{"policy":"chain","prompt":"hello there","faults":[{"provider":"fake","kind":"rate_limit"}]}`)
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	att := body["attempts"].([]any)
	if body["answer"] != "from m2" || len(att) != 2 || att[0].(map[string]any)["injected"] != true || att[1].(map[string]any)["kind"] != "fallback" {
		t.Fatalf("fault should fail m and fall back to m2: %v", body)
	}
	if len(s.prov.Requests()) != 0 {
		t.Errorf("the faulted provider must not be called, got %d", len(s.prov.Requests()))
	}

	// History is stored in Postgres and visible to a viewer.
	code, hist := r.json(t, "GET", "/admin/playground/history", viewerTok, "")
	reqs := hist["requests"].([]any)
	if code != 200 || len(reqs) == 0 || reqs[0].(map[string]any)["prompt"] != "hello there" || reqs[0].(map[string]any)["id"] != body["id"] {
		t.Fatalf("history: %d %v", code, hist)
	}

	// A viewer cannot run it.
	if code, _ := r.json(t, "POST", "/admin/playground/chat", viewerTok, `{"prompt":"x"}`); code != 403 {
		t.Errorf("viewer chat: %d", code)
	}

	// The public endpoint ignores a faults field and a real key can't inject faults.
	_, secret := s.newKey(t, "not-the-playground-"+uuid.NewString()[:8])
	s.prov.Default = fake.Behavior{Text: "real"}
	before := len(s.prov.Requests())
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"faults":[{"provider":"fake","kind":"server_error"}]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 400 {
		t.Errorf("/v1 with a faults field: %d", resp.StatusCode)
	}
	if resp.StatusCode == 200 && len(s.prov.Requests()) != before+1 {
		t.Error("the fault must be ignored: the provider should have been called")
	}
	for p, st := range s.gw.BreakerStates() {
		if int(st) != 0 {
			t.Errorf("breaker for %s moved after injected faults: %v", p, st)
		}
	}
}
