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
)

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
	if err := ValidateName(p.Name); err != nil {
		return err
	}
	if p.Kind != HTTP && p.Kind != MCP {
		return fmt.Errorf("kind must be http or mcp, not %q", p.Kind)
	}
	if !strings.HasPrefix(p.Endpoint, "http://") && !strings.HasPrefix(p.Endpoint, "https://") {
		return errors.New("endpoint must be an http:// or https:// URL")
	}
	if len(p.InputSchema) > 0 && !json.Valid(p.InputSchema) {
		return errors.New("input_schema is not valid JSON")
	}
	if p.Timeout < 0 {
		return errors.New("timeout must not be negative")
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

// Get returns the named tools, in the order asked. A name that is not registered is ErrNotFound.
func (s *Store) Get(ctx context.Context, names []string) ([]Tool, error) {
	out := make([]Tool, 0, len(names))
	for _, n := range names {
		t, err := s.scan(s.pool.QueryRow(ctx, `SELECT `+toolColumns+` FROM tools WHERE name=$1`, n))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: %q", ErrNotFound, n)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// Missing returns which of the names are not registered.
func (s *Store) Missing(ctx context.Context, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT name FROM tools WHERE name = ANY($1)`, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		have[n] = true
	}
	var missing []string
	for _, n := range names {
		if !have[n] {
			missing = append(missing, n)
		}
	}
	return missing, rows.Err()
}
