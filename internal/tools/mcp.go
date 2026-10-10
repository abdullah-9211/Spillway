package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// This file is a client for the MCP "streamable HTTP" transport: every message is a POST of one JSON-RPC message to a
// single endpoint, and the answer is either a JSON body or an SSE stream carrying it. The server may hand out a session
// id on initialize; the client sends it on every later request and starts a fresh session if the server forgot it.

const (
	mcpProtocolVersion = "2025-06-18"
	mcpMaxPages        = 20
)

// MCPToolInfo is one tool of an MCP server, as tools/list describes it.
type MCPToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// discoveredDoc is what a server row keeps in input_schema after a discover.
type discoveredDoc struct {
	Tools []MCPToolInfo `json:"tools"`
	At    time.Time     `json:"discovered_at"`
}

// DiscoveredTools reads the stored list; an undiscovered server has none.
func DiscoveredTools(stored json.RawMessage) ([]MCPToolInfo, error) {
	if len(stored) == 0 {
		return nil, nil
	}
	var d discoveredDoc
	if err := json.Unmarshal(stored, &d); err != nil {
		return nil, fmt.Errorf("unreadable discovered tool list: %w", err)
	}
	return d.Tools, nil
}

// MCPClient talks to MCP servers. Sessions are cached by endpoint and re-initialised when a server drops one, so a
// worker that picks up a run after a crash simply opens a new session.
type MCPClient struct {
	HTTP *http.Client

	mu       sync.Mutex
	sessions map[string]string // endpoint -> session id ("" when the server has none)
	seq      int64
}

func (c *MCPClient) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message) }

type rpcResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

var errSessionGone = errors.New("mcp: the session is gone")

func (c *MCPClient) nextID() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	return c.seq
}

// post sends one JSON-RPC message. A request (wantID > 0) returns its result; a notification returns nil.
func (c *MCPClient) post(ctx context.Context, endpoint string, headers map[string]string, session string, body any, wantID int64, extra map[string]string) (json.RawMessage, string, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, "", err
	}
	for k, v := range headers {
		if strings.EqualFold(k, SigningSecretHeader) {
			continue
		}
		req.Header.Set(k, v)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
		req.Header.Set("MCP-Protocol-Version", mcpProtocolVersion)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	newSession := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode == http.StatusNotFound && session != "" {
		return nil, "", errSessionGone
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, "", fmt.Errorf("the MCP server answered %d: %s", resp.StatusCode, snippet(string(b)))
	}
	if wantID == 0 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, newSession, nil
	}
	ct := resp.Header.Get("Content-Type")
	var msg *rpcResponse
	if strings.HasPrefix(ct, "text/event-stream") {
		msg, err = readSSE(resp.Body, wantID)
	} else {
		var r rpcResponse
		b, rerr := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes*2))
		if rerr != nil {
			return nil, "", rerr
		}
		if err = json.Unmarshal(b, &r); err == nil {
			msg = &r
		}
	}
	if err != nil {
		return nil, "", fmt.Errorf("unreadable MCP answer: %w", err)
	}
	if msg == nil {
		return nil, "", errors.New("the MCP server closed the stream without answering")
	}
	if msg.Error != nil {
		return nil, "", msg.Error
	}
	return msg.Result, newSession, nil
}

// readSSE reads events until the response carrying id arrives; other messages (progress, logs) are skipped.
func readSSE(r io.Reader, id int64) (*rpcResponse, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), MaxResponseBytes*2)
	var data []string
	flush := func() (*rpcResponse, bool) {
		if len(data) == 0 {
			return nil, false
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		var m rpcResponse
		if json.Unmarshal([]byte(payload), &m) != nil || len(m.ID) == 0 {
			return nil, false
		}
		var got int64
		if json.Unmarshal(m.ID, &got) == nil && got == id {
			return &m, true
		}
		return nil, false
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if m, ok := flush(); ok {
				return m, nil
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if m, ok := flush(); ok {
		return m, nil
	}
	return nil, sc.Err()
}

func (c *MCPClient) initialize(ctx context.Context, endpoint string, headers map[string]string) (string, error) {
	id := c.nextID()
	_, session, err := c.post(ctx, endpoint, headers, "", map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "initialize",
		"params": map[string]any{"protocolVersion": mcpProtocolVersion, "capabilities": map[string]any{},
			"clientInfo": map[string]any{"name": "spillway", "version": "1"}},
	}, id, nil)
	if err != nil {
		return "", fmt.Errorf("initialize: %w", err)
	}
	if _, _, err := c.post(ctx, endpoint, headers, session, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, 0, nil); err != nil {
		return "", fmt.Errorf("initialized: %w", err)
	}
	c.mu.Lock()
	if c.sessions == nil {
		c.sessions = map[string]string{}
	}
	c.sessions[endpoint] = session
	c.mu.Unlock()
	return session, nil
}

// request runs one request in a session, opening or re-opening the session as needed.
func (c *MCPClient) request(ctx context.Context, endpoint string, headers map[string]string, method string, params any, extra map[string]string) (json.RawMessage, error) {
	for attempt := 0; attempt < 2; attempt++ {
		c.mu.Lock()
		session, ok := c.sessions[endpoint]
		c.mu.Unlock()
		if !ok {
			var err error
			if session, err = c.initialize(ctx, endpoint, headers); err != nil {
				return nil, err
			}
		}
		id := c.nextID()
		res, _, err := c.post(ctx, endpoint, headers, session, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}, id, extra)
		if errors.Is(err, errSessionGone) {
			c.mu.Lock()
			delete(c.sessions, endpoint)
			c.mu.Unlock()
			continue
		}
		return res, err
	}
	return nil, errSessionGone
}

// ListTools returns every tool the server offers, following pagination.
func (c *MCPClient) ListTools(ctx context.Context, endpoint string, headers map[string]string) ([]MCPToolInfo, error) {
	var all []MCPToolInfo
	cursor := ""
	for page := 0; page < mcpMaxPages; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := c.request(ctx, endpoint, headers, "tools/list", params, nil)
		if err != nil {
			return nil, err
		}
		var out struct {
			Tools      []MCPToolInfo `json:"tools"`
			NextCursor string        `json:"nextCursor"`
		}
		if err := json.Unmarshal(res, &out); err != nil {
			return nil, fmt.Errorf("unreadable tools/list answer: %w", err)
		}
		all = append(all, out.Tools...)
		if out.NextCursor == "" {
			return all, nil
		}
		cursor = out.NextCursor
	}
	return all, nil
}

// CallTool calls one tool. The idempotency key goes in _meta so a server that supports it applies the effect once; as
// with HTTP tools, exactly-once rests on the server honouring it. A tool-level error (isError) comes back as an error
// the model can read.
func (c *MCPClient) CallTool(ctx context.Context, endpoint string, headers map[string]string, name string, args json.RawMessage, key string, extra map[string]string) (string, error) {
	var a any = map[string]any{}
	if len(args) > 0 {
		a = args
	}
	res, err := c.request(ctx, endpoint, headers, "tools/call", map[string]any{
		"name": name, "arguments": a, "_meta": map[string]any{"spillway/idempotencyKey": key},
	}, extra)
	if err != nil {
		return "", err
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Structured json.RawMessage `json:"structuredContent"`
		IsError    bool            `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", fmt.Errorf("unreadable tools/call answer: %w", err)
	}
	var parts []string
	for _, p := range out.Content {
		switch p.Type {
		case "text":
			parts = append(parts, p.Text)
		default:
			parts = append(parts, fmt.Sprintf("[%s content omitted]", p.Type))
		}
	}
	text := strings.Join(parts, "\n")
	if text == "" && len(out.Structured) > 0 {
		text = string(out.Structured)
	}
	if out.IsError {
		return "", fmt.Errorf("%s", snippet(text))
	}
	return text, nil
}
