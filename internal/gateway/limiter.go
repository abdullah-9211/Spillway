package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Limiter is a per-key request rate limit.
type Limiter interface {
	// Allow takes one token from the key's bucket. When it refuses, retryAfter says when a token will exist.
	Allow(ctx context.Context, keyID uuid.UUID, rpm int) (ok bool, retryAfter time.Duration, err error)
}

// tokenBucket refills and takes in one atomic step. Time comes from Redis, so several Spillway instances
// agree on it. Capacity is rpm and the refill rate is rpm/60 tokens per second.
var tokenBucket = redis.NewScript(`
local cap  = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])            -- tokens per second
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)   -- ms
local d = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(d[1])
local ts = tonumber(d[2])
if tokens == nil then tokens = cap; ts = now end
tokens = math.min(cap, tokens + math.max(0, now - ts) * rate / 1000)
local allowed = 0
local retry = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
else
  retry = math.ceil((1 - tokens) / rate * 1000)
end
redis.call('HSET', KEYS[1], 'tokens', tostring(tokens), 'ts', now)
redis.call('PEXPIRE', KEYS[1], math.ceil(cap / rate * 1000) + 1000)
return {allowed, retry}
`)

type RedisLimiter struct {
	rdb    *redis.Client
	prefix string
}

func NewRedisLimiter(rdb *redis.Client) *RedisLimiter {
	return &RedisLimiter{rdb: rdb, prefix: "spw:rl:"}
}

func (l *RedisLimiter) Allow(ctx context.Context, keyID uuid.UUID, rpm int) (bool, time.Duration, error) {
	if rpm <= 0 {
		return true, 0, nil
	}
	res, err := tokenBucket.Run(ctx, l.rdb, []string{l.prefix + keyID.String()}, rpm, float64(rpm)/60).Int64Slice()
	if err != nil {
		return false, 0, fmt.Errorf("rate limiter: %w", err)
	}
	return res[0] == 1, time.Duration(res[1]) * time.Millisecond, nil
}
