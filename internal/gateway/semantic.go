package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/abdullah-9211/spillway/internal/provider"
)

// SemanticCache finds a stored answer to a question that is close in meaning, not just identical.
type SemanticCache interface {
	Lookup(ctx context.Context, scope uuid.UUID, group string, embedding []float32, threshold float64) (e *Entry, similarity float64, err error)
	Store(ctx context.Context, scope uuid.UUID, group string, embedding []float32, e *Entry, ttl time.Duration) error
}

// IsHit is the threshold rule: a similarity at or above the threshold is a hit.
func IsHit(similarity, threshold float64) bool { return similarity >= threshold }

// SemanticText is what gets embedded: the last user message. The system prompt and tool list do not go into
// the vector; they are folded into the group key instead, so entries only ever match when those are identical.
// ok is false when the request is not eligible: no user message last, or tool calls anywhere in the history.
func SemanticText(req *provider.ChatRequest) (text string, ok bool) {
	for _, m := range req.Messages {
		if m.Role == "tool" || len(m.ToolCalls) > 0 || m.Content.HasNonText() {
			return "", false
		}
	}
	if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == "user" {
		text = strings.TrimSpace(req.Messages[n-1].Content.PlainText())
	}
	return text, text != ""
}

// SemanticGroup identifies which entries a request may match: the same policy, system prompt and tools.
func SemanticGroup(req *provider.ChatRequest) string {
	var system []string
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			system = append(system, m.Content.PlainText())
		}
	}
	tools, _ := json.Marshal(req.Tools)
	h := sha256.Sum256([]byte(strings.Join(system, "\x00") + "\x01" + string(tools)))
	return req.Model + ":" + hex.EncodeToString(h[:8])
}

// PGSemantic stores vectors in Postgres with pgvector, using cosine distance.
type PGSemantic struct {
	pool *pgxpool.Pool
}

func NewPGSemantic(pool *pgxpool.Pool) *PGSemantic { return &PGSemantic{pool: pool} }

func vectorLiteral(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

func (s *PGSemantic) Lookup(ctx context.Context, scope uuid.UUID, group string, emb []float32, threshold float64) (*Entry, float64, error) {
	var raw []byte
	var sim float64
	err := s.pool.QueryRow(ctx, `
		SELECT response, 1 - (embedding <=> $1::vector) AS similarity
		FROM semantic_cache
		WHERE scope = $2 AND model_group = $3 AND expires_at > now()
		ORDER BY embedding <=> $1::vector
		LIMIT 1`, vectorLiteral(emb), scope, group).Scan(&raw, &sim)
	if err == pgx.ErrNoRows {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("semantic lookup: %w", err)
	}
	if !IsHit(sim, threshold) {
		return nil, sim, nil
	}
	var e Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, sim, nil
	}
	return &e, sim, nil
}

func (s *PGSemantic) Store(ctx context.Context, scope uuid.UUID, group string, emb []float32, e *Entry, ttl time.Duration) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	id, _ := uuid.NewV7()
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO semantic_cache (id, scope, model_group, embedding, response, cost_usd, expires_at)
		VALUES ($1, $2, $3, $4::vector, $5, $6::numeric / 1000000, now() + make_interval(secs => $7))`,
		id, scope, group, vectorLiteral(emb), raw, int64(e.Cost), ttl.Seconds()); err != nil {
		return fmt.Errorf("semantic store: %w", err)
	}
	// Expired rows are never matched; sweep a few now and then so the table does not grow forever.
	if id[15]%16 == 0 {
		_, _ = s.pool.Exec(ctx, `DELETE FROM semantic_cache WHERE expires_at < now() - interval '1 hour'`)
	}
	return nil
}
