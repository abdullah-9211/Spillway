package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/abdullah-9211/spillway/internal/runs"
)

// testMCP is a small MCP server over streamable HTTP: JSON answers, or SSE with a progress notification first.
type testMCP struct {
	mu        sync.Mutex
	sse       bool
	auth      string
	sessions  map[string]bool
	nextSess  int
	pageSize  int
	calls     []map[string]any
	inits     int
	callCount int
	expire    bool // forget every session once, to test re-initialisation
}

func (m *testMCP) handler(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.auth != "" && r.Header.Get("Authorization") != m.auth {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var msg struct {
		ID     *int64          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	_ = json.Unmarshal(body, &msg)
	if m.sessions == nil {
		m.sessions = map[string]bool{}
	}
	reply := func(result any) {
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		if m.sse {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", out)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}
	if msg.Method != "initialize" {
		if m.expire {
			m.sessions = map[string]bool{}
			m.expire = false
		}
		if !m.sessions[r.Header.Get("Mcp-Session-Id")] {
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
	}
	switch msg.Method {
	case "initialize":
		m.inits++
		m.nextSess++
		id := fmt.Sprintf("sess-%d", m.nextSess)
		m.sessions[id] = true
		w.Header().Set("Mcp-Session-Id", id)
		reply(map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "t", "version": "1"}})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		all := []map[string]any{
			{"name": "read", "description": "Reads a file", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}},
			{"name": "write", "description": "Writes a file", "inputSchema": map[string]any{"type": "object"}},
			{"name": "boom", "description": "Always fails", "inputSchema": map[string]any{"type": "object"}},
		}
		var p struct {
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		start := 0
		if p.Cursor != "" {
			fmt.Sscanf(p.Cursor, "c%d", &start)
		}
		end, next := len(all), ""
		if m.pageSize > 0 && start+m.pageSize < len(all) {
			end, next = start+m.pageSize, fmt.Sprintf("c%d", start+m.pageSize)
		}
		reply(map[string]any{"tools": all[start:end], "nextCursor": next})
	case "tools/call":
		m.callCount++
		var p map[string]any
		_ = json.Unmarshal(msg.Params, &p)
		p["_header_idempotency"] = r.Header.Get("Idempotency-Key")
		m.calls = append(m.calls, p)
		if p["name"] == "boom" {
			reply(map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": "disk is on fire"}}})
			return
		}
		reply(map[string]any{"content": []map[string]any{{"type": "text", "text": "contents of " + fmt.Sprint(p["arguments"].(map[string]any)["path"])}, {"type": "image"}}})
	default:
		http.Error(w, "nope", http.StatusBadRequest)
	}
}

func TestMCPClientAgainstATestServer(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%v", sse), func(t *testing.T) {
			srv := &testMCP{sse: sse, auth: "Bearer k", pageSize: 2}
			ts := httptest.NewServer(http.HandlerFunc(srv.handler))
			defer ts.Close()
			c := &MCPClient{}
			ctx := context.Background()
			h := map[string]string{"Authorization": "Bearer k"}

			list, err := c.ListTools(ctx, ts.URL, h)
			if err != nil || len(list) != 3 || list[0].Name != "read" || !strings.Contains(string(list[0].InputSchema), "path") {
				t.Fatalf("list = %+v %v", list, err)
			}
			out, err := c.CallTool(ctx, ts.URL, h, "read", json.RawMessage(`{"path":"/a"}`), "key-1", map[string]string{"Idempotency-Key": "key-1"})
			if err != nil || out != "contents of /a\n[image content omitted]" {
				t.Fatalf("call = %q %v", out, err)
			}
			last := srv.calls[len(srv.calls)-1]
			meta := last["_meta"].(map[string]any)
			if meta["spillway/idempotencyKey"] != "key-1" || last["_header_idempotency"] != "key-1" {
				t.Errorf("idempotency key not sent: %v", last)
			}
			if srv.inits != 1 {
				t.Errorf("initialised %d times; the session should be reused", srv.inits)
			}
			if _, err := c.CallTool(ctx, ts.URL, h, "boom", nil, "k", nil); err == nil || !strings.Contains(err.Error(), "disk is on fire") {
				t.Errorf("tool error = %v", err)
			}
			// The server forgets the session: the client opens a new one and the call still succeeds.
			srv.mu.Lock()
			srv.expire = true
			srv.mu.Unlock()
			if out, err := c.CallTool(ctx, ts.URL, h, "read", json.RawMessage(`{"path":"/b"}`), "key-2", nil); err != nil || !strings.HasPrefix(out, "contents of /b") {
				t.Errorf("after expiry: %q %v", out, err)
			}
			if srv.inits != 2 {
				t.Errorf("inits = %d, want 2", srv.inits)
			}
		})
	}
}

func TestMCPAuthFailureIsAReadableError(t *testing.T) {
	srv := &testMCP{auth: "Bearer right"}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()
	_, err := (&MCPClient{}).ListTools(context.Background(), ts.URL, map[string]string{"Authorization": "Bearer wrong"})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v", err)
	}
}

func TestExecutorCallsAnMCPToolAndAllowlistNamesResolve(t *testing.T) {
	srv := &testMCP{}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()
	server := Tool{Name: "files", Kind: MCP, Endpoint: ts.URL, RequiresApproval: true}
	tool := server
	tool.Name, tool.MCPTool, tool.Description = "files.read", "read", "Reads a file"
	ex := &Executor{Source: memSource{"files.read": tool}, WebhookSecret: "s"}

	out, err := ex.Call(context.Background(), runs.ToolInvocation{RunID: runID, StepNo: 2, Name: "files.read", Arguments: json.RawMessage(`{"path":"/x"}`), IdempotencyKey: "abc"})
	if err != nil || out != "contents of /x\n[image content omitted]" {
		t.Fatalf("%q %v", out, err)
	}
	if srv.calls[0]["name"] != "read" {
		t.Errorf("the server's own tool name must be sent, got %v", srv.calls[0]["name"])
	}
	specs, _ := ex.Specs([]string{"files.read"})
	if specs[0].Function.Name != "files__read" {
		t.Errorf("model-facing name = %q", specs[0].Function.Name)
	}
	if need, _ := ex.RequiresApproval(context.Background(), "files.read"); !need {
		t.Error("an MCP tool inherits its server's approval setting")
	}
	if _, err := ex.Discover(context.Background(), Tool{Kind: HTTP}); err == nil {
		t.Error("discover on a non-MCP tool")
	}
	got, err := ex.Discover(context.Background(), server)
	if err != nil || len(got) != 3 {
		t.Errorf("discover = %v %v", got, err)
	}
}
