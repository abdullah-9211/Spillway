package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/money"
	"github.com/abdullah-9211/spillway/internal/provider"
)

// Entry is a cached completion with what it originally cost, so a hit can report what it saved.
type Entry struct {
	Response provider.ChatResponse `json:"response"`
	Provider string                `json:"provider"`
	Model    string                `json:"model"` // catalog id of the model that answered
	Cost     money.Micros          `json:"cost_micros"`
}

// canonicalRequest holds only the fields that change the answer. stream, stream_options and user do not,
// so a streamed request can be served from an entry a non-streamed one created, and vice versa.
type canonicalRequest struct {
	Policy              string             `json:"policy"`
	Messages            []canonicalMessage `json:"messages"`
	Tools               []provider.Tool    `json:"tools,omitempty"`
	ToolChoice          json.RawMessage    `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool              `json:"parallel_tool_calls,omitempty"`
	Temperature         *float64           `json:"temperature,omitempty"`
	TopP                *float64           `json:"top_p,omitempty"`
	MaxTokens           *int               `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int               `json:"max_completion_tokens,omitempty"`
	Stop                []string           `json:"stop,omitempty"`
	ResponseFormat      json.RawMessage    `json:"response_format,omitempty"`
	Seed                *int               `json:"seed,omitempty"`
	PresencePenalty     *float64           `json:"presence_penalty,omitempty"`
	FrequencyPenalty    *float64           `json:"frequency_penalty,omitempty"`
}

type canonicalMessage struct {
	Role       string                 `json:"role"`
	Content    string                 `json:"content,omitempty"`
	Parts      []provider.ContentPart `json:"parts,omitempty"`
	Name       string                 `json:"name,omitempty"`
	ToolCalls  []provider.ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string                 `json:"tool_call_id,omitempty"`
}

// CacheKey returns the exact-cache key for a request. Two requests that mean the same thing get the
// same key: a plain string and a single text part are equal, JSON key order does not matter, and the
// excluded fields never matter. scope is the API key id, or "global" when entries are shared.
func CacheKey(scope string, req *provider.ChatRequest) (string, error) {
	c := canonicalRequest{
		Policy: req.Model, Tools: req.Tools, ToolChoice: compactJSON(req.ToolChoice), ParallelToolCalls: req.ParallelToolCalls,
		Temperature: req.Temperature, TopP: req.TopP, MaxTokens: req.MaxTokens, MaxCompletionTokens: req.MaxCompletionTokens,
		Stop: req.Stop, ResponseFormat: compactJSON(req.ResponseFormat), Seed: req.Seed,
		PresencePenalty: req.PresencePenalty, FrequencyPenalty: req.FrequencyPenalty,
	}
	for _, m := range req.Messages {
		cm := canonicalMessage{Role: m.Role, Name: m.Name, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID}
		if m.Content.HasNonText() {
			cm.Parts = m.Content.Parts
		} else {
			cm.Content = m.Content.PlainText()
		}
		c.Messages = append(c.Messages, cm)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	// Round trip through a generic value so map keys inside tool schemas are emitted in sorted order.
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return "", err
	}
	norm, err := json.Marshal(generic)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(norm)
	return scope + ":" + hex.EncodeToString(sum[:]), nil
}

func compactJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	b, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return b
}

// Cacheable says whether a request may be cached at all: its temperature must be exactly 0, because that
// is the only case where an answer is meant to be repeatable, unless the key opts in to caching anything.
// An unset temperature is not treated as 0: the providers' defaults are not 0.
func Cacheable(req *provider.ChatRequest, key keys.Key) bool {
	if key.CacheNonzeroTemp {
		return true
	}
	return req.Temperature != nil && *req.Temperature == 0
}

// StoreWorthy says whether a finished response should be cached.
func StoreWorthy(resp *provider.ChatResponse) bool {
	if len(resp.Choices) == 0 {
		return false
	}
	switch resp.Choices[0].FinishReason {
	case "stop", "tool_calls", "length":
		return true
	}
	return false
}

// ExactCache is the Redis-backed cache of full responses.
type ExactCache interface {
	Get(ctx context.Context, key string) (*Entry, error) // (nil, nil) on a miss
	Set(ctx context.Context, key string, e *Entry, ttl time.Duration) error
}

type RedisCache struct {
	rdb    *redis.Client
	prefix string
}

func NewRedisCache(rdb *redis.Client) *RedisCache {
	return &RedisCache{rdb: rdb, prefix: "spw:cache:v1:"}
}

func (c *RedisCache) Get(ctx context.Context, key string) (*Entry, error) {
	raw, err := c.rdb.Get(ctx, c.prefix+key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cache get: %w", err)
	}
	var e Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, nil // a corrupt entry is a miss; it will be overwritten
	}
	return &e, nil
}

func (c *RedisCache) Set(ctx context.Context, key string, e *Entry, ttl time.Duration) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := c.rdb.Set(ctx, c.prefix+key, raw, ttl).Err(); err != nil {
		return fmt.Errorf("cache set: %w", err)
	}
	return nil
}

// scopeFor is the cache scope for a key: per API key by default, shared when the catalog says global.
func scopeFor(global bool, id uuid.UUID) string {
	if global {
		return "global"
	}
	return id.String()
}
