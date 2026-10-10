// Package tools is the registry of tools a run may call, and the executor that calls them. A tool is registered once
// with its endpoint, its input schema and (encrypted) auth headers; a run names the tools it may use.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/abdullah-9211/spillway/internal/secret"
)

type Kind string

const (
	HTTP Kind = "http"
	MCP  Kind = "mcp"
)

// DefaultTimeout is how long a tool call may take unless the tool says otherwise.
const DefaultTimeout = 10 * time.Second

var (
	ErrNotFound  = errors.New("tools: no such tool")
	ErrDuplicate = errors.New("tools: a tool with that name already exists")
	// ErrInvalid wraps a problem with what the caller sent.
	ErrInvalid = errors.New("tools: invalid")
)

// DiscoverError: the MCP server could not be reached or answered badly.
type DiscoverError struct{ Err error }

func (e *DiscoverError) Error() string { return "Could not read the server's tools: " + e.Err.Error() }
func (e *DiscoverError) Unwrap() error { return e.Err }

type Tool struct {
	ID               uuid.UUID
	Name             string
	Kind             Kind
	Endpoint         string
	Headers          map[string]string // decrypted; never sent to a browser
	Description      string
	InputSchema      json.RawMessage
	Timeout          time.Duration
	RequiresApproval bool
	CreatedAt        time.Time
	// MCPTool is set on a tool resolved from an MCP server: the server's own name for it. Name is "server.tool".
	MCPTool string
	// Discovered is when an MCP server's tool list was last fetched; zero for http tools and undiscovered servers.
	Discovered time.Time
}

type CreateParams struct {
	Name             string
	Kind             Kind
	Endpoint         string
	Headers          map[string]string
	Description      string
	InputSchema      json.RawMessage
	Timeout          time.Duration
	RequiresApproval bool
}

type Store struct {
	pool *pgxpool.Pool
	box  *secret.Box // nil: tools with auth headers cannot be stored or read
}

func NewStore(pool *pgxpool.Pool, box *secret.Box) *Store { return &Store{pool: pool, box: box} }

// ValidateName: the name is shown to the model, so it must be a plain identifier.
func ValidateName(n string) error {
	if n == "sleep" || n == "request_human_approval" {
		return fmt.Errorf("%q is a built-in tool of every run and cannot be registered", n)
	}
	if n == "" || len(n) > 64 {
		return errors.New("a tool name is 1 to 64 characters")
	}
	for _, r := range n {
		ok := r == '_' || r == '-' || r == '.' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return fmt.Errorf("a tool name may use letters, digits, '_', '-' and '.', not %q", r)
		}
	}
	return nil
}

func (p CreateParams) validate() error {
	invalid := func(format string, a ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, a...)...)
	}
	if err := ValidateName(p.Name); err != nil {
		return invalid("%v", err)
	}
	if p.Kind != HTTP && p.Kind != MCP {
		return invalid("kind must be http or mcp, not %q", p.Kind)
	}
	if p.Kind == MCP && strings.Contains(p.Name, ".") {
		return invalid("an MCP server name may not contain '.', which separates the server from its tool")
	}
	if !strings.HasPrefix(p.Endpoint, "http://") && !strings.HasPrefix(p.Endpoint, "https://") {
		return invalid("endpoint must be an http:// or https:// URL")
	}
	if len(p.InputSchema) > 0 && !json.Valid(p.InputSchema) {
		return invalid("input_schema is not valid JSON")
	}
	if p.Timeout < 0 {
		return invalid("timeout must not be negative")
	}
	return nil
}

// Create registers a tool. Headers are encrypted with the secret key, bound to the tool's id.
func (s *Store) Create(ctx context.Context, p CreateParams) (Tool, error) {
	if err := p.validate(); err != nil {
		return Tool{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Tool{}, err
	}
	var enc []byte
	if len(p.Headers) > 0 {
		if s.box == nil {
			return Tool{}, errors.New("SPILLWAY_SECRET_KEY is not set, so auth headers cannot be stored")
		}
		raw, _ := json.Marshal(p.Headers)
		if enc, err = s.box.Encrypt(raw, id[:]); err != nil {
			return Tool{}, err
		}
	}
	timeout := p.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	var schema any
	if len(p.InputSchema) > 0 {
		schema = []byte(p.InputSchema)
	}
	var created time.Time
	err = s.pool.QueryRow(ctx, `INSERT INTO tools (id, name, kind, endpoint, headers_enc, description, input_schema, timeout_ms, requires_approval)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING created_at`,
		id, p.Name, string(p.Kind), p.Endpoint, enc, nullIfEmpty(p.Description), schema, int(timeout/time.Millisecond), p.RequiresApproval).Scan(&created)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return Tool{}, ErrDuplicate
	}
	if err != nil {
		return Tool{}, err
	}
	return Tool{ID: id, Name: p.Name, Kind: p.Kind, Endpoint: p.Endpoint, Headers: p.Headers, Description: p.Description,
		InputSchema: p.InputSchema, Timeout: timeout, RequiresApproval: p.RequiresApproval, CreatedAt: created}, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

const toolColumns = `id, name, kind, endpoint, headers_enc, COALESCE(description,''), input_schema, timeout_ms, requires_approval, created_at`

func (s *Store) scan(row pgx.Row) (Tool, error) {
	var t Tool
	var kind string
	var enc []byte
	var schema []byte
	var ms int
	if err := row.Scan(&t.ID, &t.Name, &kind, &t.Endpoint, &enc, &t.Description, &schema, &ms, &t.RequiresApproval, &t.CreatedAt); err != nil {
		return Tool{}, err
	}
	t.Kind, t.InputSchema, t.Timeout = Kind(kind), schema, time.Duration(ms)*time.Millisecond
	if t.Kind == MCP {
		var doc discoveredDoc
		if json.Unmarshal(schema, &doc) == nil {
			t.Discovered = doc.At
		}
	}
	if len(enc) > 0 {
		if s.box == nil {
			return Tool{}, fmt.Errorf("tool %q has auth headers but SPILLWAY_SECRET_KEY is not set", t.Name)
		}
		raw, err := s.box.Decrypt(enc, t.ID[:])
		if err != nil {
			return Tool{}, fmt.Errorf("tool %q: %w", t.Name, err)
		}
		if err := json.Unmarshal(raw, &t.Headers); err != nil {
			return Tool{}, fmt.Errorf("tool %q: unreadable headers: %w", t.Name, err)
		}
	}
	return t, nil
}

// Get returns the named tools, in the order asked. A name that is not registered is ErrNotFound. "server.tool" resolves
// to a tool of a registered MCP server, using the list its last discovery stored.
func (s *Store) Get(ctx context.Context, names []string) ([]Tool, error) {
	out := make([]Tool, 0, len(names))
	for _, n := range names {
		t, err := s.one(ctx, n)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func (s *Store) one(ctx context.Context, name string) (Tool, error) {
	t, err := s.scan(s.pool.QueryRow(ctx, `SELECT `+toolColumns+` FROM tools WHERE name=$1`, name))
	switch {
	case err == nil:
		if t.Kind == MCP {
			return Tool{}, fmt.Errorf("%w: %q is an MCP server; name its tools as %s.<tool>", ErrNotFound, name, name)
		}
		return t, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return Tool{}, err
	}
	for i := len(name) - 1; i > 0; i-- {
		if name[i] != '.' {
			continue
		}
		srv, err := s.scan(s.pool.QueryRow(ctx, `SELECT `+toolColumns+` FROM tools WHERE name=$1 AND kind='mcp'`, name[:i]))
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return Tool{}, err
		}
		remote := name[i+1:]
		list, err := DiscoveredTools(srv.InputSchema)
		if err != nil {
			return Tool{}, err
		}
		if len(list) == 0 {
			return Tool{}, fmt.Errorf("%w: %q: server %q has not been discovered yet", ErrNotFound, name, srv.Name)
		}
		for _, d := range list {
			if d.Name == remote {
				srv.Name, srv.MCPTool, srv.Description, srv.InputSchema = name, remote, d.Description, d.InputSchema
				return srv, nil
			}
		}
		return Tool{}, fmt.Errorf("%w: %q: server %q has no tool %q", ErrNotFound, name, srv.Name, remote)
	}
	return Tool{}, fmt.Errorf("%w: %q", ErrNotFound, name)
}

// Missing returns which of the names are not registered (or, for an MCP tool, not discovered).
func (s *Store) Missing(ctx context.Context, names []string) ([]string, error) {
	var missing []string
	for _, n := range names {
		if _, err := s.one(ctx, n); errors.Is(err, ErrNotFound) {
			missing = append(missing, n)
		} else if err != nil {
			return nil, err
		}
	}
	return missing, nil
}

// List returns every registered tool and server, by name.
func (s *Store) List(ctx context.Context) ([]Tool, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+toolColumns+` FROM tools ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tool
	for rows.Next() {
		t, err := s.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ByName returns one registered tool or MCP server exactly as stored.
func (s *Store) ByName(ctx context.Context, name string) (Tool, error) {
	t, err := s.scan(s.pool.QueryRow(ctx, `SELECT `+toolColumns+` FROM tools WHERE name=$1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return Tool{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return t, err
}

// ByID returns one registered tool or server exactly as stored.
func (s *Store) ByID(ctx context.Context, id uuid.UUID) (Tool, error) {
	t, err := s.scan(s.pool.QueryRow(ctx, `SELECT `+toolColumns+` FROM tools WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Tool{}, ErrNotFound
	}
	return t, err
}

// UpdateParams: a nil field is left alone. Headers, when set, replace the stored ones.
type UpdateParams struct {
	Endpoint         *string
	Description      *string
	Headers          map[string]string
	InputSchema      json.RawMessage
	Timeout          *time.Duration
	RequiresApproval *bool
}

func (s *Store) Update(ctx context.Context, id uuid.UUID, p UpdateParams) (Tool, error) {
	cur, err := s.ByID(ctx, id)
	if err != nil {
		return Tool{}, err
	}
	if p.Endpoint != nil {
		cur.Endpoint = *p.Endpoint
	}
	if p.Description != nil {
		cur.Description = *p.Description
	}
	if p.Headers != nil {
		cur.Headers = p.Headers
	}
	if len(p.InputSchema) > 0 && cur.Kind == HTTP {
		cur.InputSchema = p.InputSchema
	}
	if p.Timeout != nil {
		cur.Timeout = *p.Timeout
	}
	if p.RequiresApproval != nil {
		cur.RequiresApproval = *p.RequiresApproval
	}
	check := CreateParams{Name: cur.Name, Kind: cur.Kind, Endpoint: cur.Endpoint, InputSchema: cur.InputSchema, Timeout: cur.Timeout}
	if err := check.validate(); err != nil {
		return Tool{}, err
	}
	var enc []byte
	if len(cur.Headers) > 0 {
		if s.box == nil {
			return Tool{}, errors.New("SPILLWAY_SECRET_KEY is not set, so auth headers cannot be stored")
		}
		raw, _ := json.Marshal(cur.Headers)
		if enc, err = s.box.Encrypt(raw, id[:]); err != nil {
			return Tool{}, err
		}
	}
	var schema any
	if len(cur.InputSchema) > 0 {
		schema = []byte(cur.InputSchema)
	}
	_, err = s.pool.Exec(ctx, `UPDATE tools SET endpoint=$2, headers_enc=$3, description=$4, input_schema=$5, timeout_ms=$6, requires_approval=$7 WHERE id=$1`,
		id, cur.Endpoint, enc, nullIfEmpty(cur.Description), schema, int(cur.Timeout/time.Millisecond), cur.RequiresApproval)
	return cur, err
}

func (s *Store) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM tools WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetDiscovered stores the tool list fetched from an MCP server.
func (s *Store) SetDiscovered(ctx context.Context, id uuid.UUID, list []MCPToolInfo, now time.Time) error {
	raw, err := json.Marshal(discoveredDoc{Tools: list, At: now})
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE tools SET input_schema=$2 WHERE id=$1 AND kind='mcp'`, id, raw)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
