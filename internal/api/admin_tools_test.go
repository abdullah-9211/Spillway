package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/tools"
)

type fakeTools struct {
	mu   sync.Mutex
	list map[uuid.UUID]tools.Tool
	down bool
}

func newFakeTools() *fakeTools { return &fakeTools{list: map[uuid.UUID]tools.Tool{}} }

func (f *fakeTools) List(context.Context) ([]tools.Tool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []tools.Tool
	for _, t := range f.list {
		out = append(out, t)
	}
	return out, nil
}

func (f *fakeTools) Create(_ context.Context, p tools.CreateParams) (tools.Tool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := tools.ValidateName(p.Name); err != nil {
		return tools.Tool{}, fmt.Errorf("%w: %v", tools.ErrInvalid, err)
	}
	for _, t := range f.list {
		if t.Name == p.Name {
			return tools.Tool{}, tools.ErrDuplicate
		}
	}
	t := tools.Tool{ID: uuid.New(), Name: p.Name, Kind: p.Kind, Endpoint: p.Endpoint, Headers: p.Headers, Description: p.Description,
		InputSchema: p.InputSchema, Timeout: p.Timeout, RequiresApproval: p.RequiresApproval, CreatedAt: time.Unix(1_700_000_000, 0).UTC()}
	if t.Timeout == 0 {
		t.Timeout = tools.DefaultTimeout
	}
	f.list[t.ID] = t
	return t, nil
}

func (f *fakeTools) Update(_ context.Context, id uuid.UUID, p tools.UpdateParams) (tools.Tool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.list[id]
	if !ok {
		return tools.Tool{}, tools.ErrNotFound
	}
	if p.RequiresApproval != nil {
		t.RequiresApproval = *p.RequiresApproval
	}
	if p.Headers != nil {
		t.Headers = p.Headers
	}
	f.list[id] = t
	return t, nil
}

func (f *fakeTools) Delete(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.list[id]; !ok {
		return tools.ErrNotFound
	}
	delete(f.list, id)
	return nil
}

func (f *fakeTools) Discover(_ context.Context, id uuid.UUID) (tools.Tool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.list[id]
	if !ok {
		return tools.Tool{}, tools.ErrNotFound
	}
	if t.Kind != tools.MCP {
		return tools.Tool{}, fmt.Errorf("%w: not an MCP server", tools.ErrInvalid)
	}
	if f.down {
		return tools.Tool{}, &tools.DiscoverError{Err: fmt.Errorf("connection refused")}
	}
	raw, _ := json.Marshal(map[string]any{"tools": []map[string]any{{"name": "read", "description": "Reads", "inputSchema": map[string]any{"type": "object"}}}, "discovered_at": time.Unix(1_700_000_100, 0).UTC()})
	t.InputSchema = raw
	t.Discovered = time.Unix(1_700_000_100, 0).UTC()
	f.list[id] = t
	return t, nil
}

func TestToolRegistryEndpoints(t *testing.T) {
	r, _ := newPlaygroundRig(t)
	ft := r.admin.d.Tools.(*fakeTools)
	admin, viewer := r.token(t, "admin", "admin-password"), r.token(t, "viewer", "viewer-password")

	code, body := r.json(t, "POST", "/admin/tools", admin, `{"name":"files","kind":"mcp","endpoint":"http://mcp.example/mcp","headers":{"Authorization":"Bearer secret-token"},"requires_approval":true}`)
	if code != 201 || body["kind"] != "mcp" || body["requires_approval"] != true {
		t.Fatalf("create: %d %v", code, body)
	}
	if s := fmt.Sprint(body); strings.Contains(s, "secret-token") {
		t.Errorf("a header value leaked: %s", s)
	}
	if names := body["header_names"].([]any); len(names) != 1 || names[0] != "Authorization" {
		t.Errorf("header names = %v", names)
	}
	id := body["id"].(string)
	if code, _ := r.json(t, "POST", "/admin/tools", viewer, `{"name":"x","endpoint":"http://x"}`); code != 403 {
		t.Errorf("viewer create: %d", code)
	}
	if code, body := r.json(t, "POST", "/admin/tools", admin, `{"name":"files","endpoint":"http://x"}`); code != 409 || body["error"].(map[string]any)["code"] != "tool_exists" {
		t.Errorf("duplicate: %d %v", code, body)
	}
	if code, body := r.json(t, "POST", "/admin/tools", admin, `{"name":"sleep","endpoint":"http://x"}`); code != 400 || !strings.Contains(fmt.Sprint(body), "built-in") {
		t.Errorf("reserved name: %d %v", code, body)
	}
	if code, _ := r.json(t, "POST", "/admin/tools", admin, `{"name":"x","endpoint":"http://x","surprise":1}`); code != 400 {
		t.Errorf("unknown field: %d", code)
	}

	code, body = r.json(t, "GET", "/admin/tools", viewer, "")
	if code != 200 || len(body["tools"].([]any)) != 1 {
		t.Errorf("list: %d %v", code, body)
	}
	if code, body := r.json(t, "POST", "/admin/tools/"+id+"/discover", viewer, ""); code != 403 {
		t.Errorf("viewer discover: %d %v", code, body)
	}
	code, body = r.json(t, "POST", "/admin/tools/"+id+"/discover", admin, "")
	mt, _ := body["mcp_tools"].([]any)
	if code != 200 || len(mt) != 1 || mt[0].(map[string]any)["name"] != "files.read" || body["discovered_at"] == nil {
		t.Errorf("discover: %d %v", code, body)
	}
	ft.down = true
	if code, body := r.json(t, "POST", "/admin/tools/"+id+"/discover", admin, ""); code != 502 || body["error"].(map[string]any)["code"] != "mcp_unreachable" {
		t.Errorf("unreachable: %d %v", code, body)
	}
	if code, body := r.json(t, "PUT", "/admin/tools/"+id, admin, `{"requires_approval":false}`); code != 200 || body["requires_approval"] != false {
		t.Errorf("update: %d %v", code, body)
	}
	if code, _ := r.json(t, "PUT", "/admin/tools/"+uuid.NewString(), admin, `{"requires_approval":false}`); code != 404 {
		t.Errorf("update unknown: %d", code)
	}
	del := func(token string) int { return r.do(t, "DELETE", "/admin/tools/"+id, token, "").StatusCode }
	if code := del(viewer); code != 403 {
		t.Errorf("viewer delete: %d", code)
	}
	if code := del(admin); code != 204 {
		t.Errorf("delete: %d", code)
	}
	if code := del(admin); code != 404 {
		t.Errorf("delete again: %d", code)
	}
}
