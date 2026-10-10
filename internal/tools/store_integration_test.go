//go:build integration

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/abdullah-9211/spillway/internal/secret"
	"github.com/abdullah-9211/spillway/internal/testdb"
)

func TestRegistryRoundTripAndEncryptionAtRest(t *testing.T) {
	pool, _ := testdb.New(t)
	ctx := context.Background()
	box, _ := secret.New(bytes.Repeat([]byte{9}, 32))
	st := NewStore(pool, box)

	created, err := st.Create(ctx, CreateParams{Name: "send_email", Kind: HTTP, Endpoint: "https://tools.example/email", Description: "Sends an email",
		Headers: map[string]string{"Authorization": "Bearer super-secret-token"}, InputSchema: json.RawMessage(`{"type":"object"}`), Timeout: 5 * time.Second, RequiresApproval: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, CreateParams{Name: "plain", Kind: HTTP, Endpoint: "http://x"}); err != nil {
		t.Fatal(err)
	}

	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT headers_enc FROM tools WHERE name='send_email'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || bytes.Contains(raw, []byte("super-secret-token")) || bytes.Contains(raw, []byte("Authorization")) {
		t.Fatalf("headers must be stored encrypted, got %q", raw)
	}

	got, err := st.Get(ctx, []string{"plain", "send_email"})
	if err != nil || len(got) != 2 || got[0].Name != "plain" {
		t.Fatalf("%+v %v", got, err)
	}
	e := got[1]
	if e.ID != created.ID || e.Headers["Authorization"] != "Bearer super-secret-token" || e.Timeout != 5*time.Second || !e.RequiresApproval || e.Description != "Sends an email" || string(e.InputSchema) != `{"type": "object"}` && string(e.InputSchema) != `{"type":"object"}` {
		t.Errorf("tool = %+v", e)
	}
	if got[0].Timeout != DefaultTimeout || got[0].Headers != nil {
		t.Errorf("defaults = %+v", got[0])
	}

	if _, err := st.Create(ctx, CreateParams{Name: "plain", Kind: HTTP, Endpoint: "http://y"}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate: %v", err)
	}
	if _, err := st.Get(ctx, []string{"nope"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	if m, err := st.Missing(ctx, []string{"plain", "nope", "send_email", "also-nope"}); err != nil || len(m) != 2 || m[0] != "nope" || m[1] != "also-nope" {
		t.Errorf("missing = %v %v", m, err)
	}

	// The wrong key cannot read the headers, and a store with no key refuses to hold them.
	other, _ := secret.New(bytes.Repeat([]byte{1}, 32))
	if _, err := NewStore(pool, other).Get(ctx, []string{"send_email"}); !errors.Is(err, secret.ErrCorrupt) {
		t.Errorf("wrong key: %v", err)
	}
	if _, err := NewStore(pool, nil).Get(ctx, []string{"send_email"}); err == nil {
		t.Error("no key: should refuse")
	}
	if _, err := NewStore(pool, nil).Get(ctx, []string{"plain"}); err != nil {
		t.Errorf("a tool without headers needs no key: %v", err)
	}
	if _, err := NewStore(pool, nil).Create(ctx, CreateParams{Name: "x", Kind: HTTP, Endpoint: "http://x", Headers: map[string]string{"A": "b"}}); err == nil {
		t.Error("storing headers without a key must fail")
	}
	// A value copied to another row does not decrypt there (the tool id is bound in).
	if _, err := pool.Exec(ctx, `UPDATE tools SET headers_enc = (SELECT headers_enc FROM tools WHERE name='send_email') WHERE name='plain'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(ctx, []string{"plain"}); !errors.Is(err, secret.ErrCorrupt) {
		t.Errorf("copied ciphertext: %v", err)
	}
}

func TestCreateValidation(t *testing.T) {
	pool, _ := testdb.New(t)
	st := NewStore(pool, nil)
	for name, p := range map[string]CreateParams{
		"bad name":       {Name: "has space", Kind: HTTP, Endpoint: "http://x"},
		"bad kind":       {Name: "a", Kind: "grpc", Endpoint: "http://x"},
		"bad endpoint":   {Name: "a", Kind: HTTP, Endpoint: "ftp://x"},
		"bad schema":     {Name: "a", Kind: HTTP, Endpoint: "http://x", InputSchema: json.RawMessage(`{`)},
		"negative limit": {Name: "a", Kind: HTTP, Endpoint: "http://x", Timeout: -1},
	} {
		if _, err := st.Create(context.Background(), p); err == nil {
			t.Errorf("%s should be refused", name)
		}
	}
}

func TestMCPServerResolutionUpdateAndDelete(t *testing.T) {
	pool, _ := testdb.New(t)
	ctx := context.Background()
	box, _ := secret.New(bytes.Repeat([]byte{9}, 32))
	st := NewStore(pool, box)

	srv, err := st.Create(ctx, CreateParams{Name: "files", Kind: MCP, Endpoint: "http://mcp.example/mcp", Headers: map[string]string{"Authorization": "Bearer t"}, RequiresApproval: true})
	if err != nil {
		t.Fatal(err)
	}
	// Not discovered yet: its tools are not usable.
	if m, _ := st.Missing(ctx, []string{"files.read"}); len(m) != 1 {
		t.Errorf("undiscovered: missing = %v", m)
	}
	if err := st.SetDiscovered(ctx, srv.ID, []MCPToolInfo{{Name: "read", Description: "Reads", InputSchema: json.RawMessage(`{"type":"object"}`)}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, []string{"files.read"})
	if err != nil || got[0].Name != "files.read" || got[0].MCPTool != "read" || got[0].Kind != MCP || got[0].Headers["Authorization"] != "Bearer t" || !got[0].RequiresApproval || got[0].Description != "Reads" {
		t.Fatalf("resolved = %+v %v", got, err)
	}
	if m, _ := st.Missing(ctx, []string{"files.read", "files.write", "files", "other.x"}); len(m) != 3 {
		t.Errorf("missing = %v", m)
	}
	list, err := st.List(ctx)
	if err != nil || len(list) != 1 || list[0].Discovered.IsZero() {
		t.Errorf("list = %+v %v", list, err)
	}

	off := false
	desc := "now documented"
	up, err := st.Update(ctx, srv.ID, UpdateParams{RequiresApproval: &off, Description: &desc})
	if err != nil || up.RequiresApproval || up.Description != desc || up.Headers["Authorization"] != "Bearer t" {
		t.Errorf("update = %+v %v", up, err)
	}
	if err := st.Delete(ctx, srv.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, srv.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
}
