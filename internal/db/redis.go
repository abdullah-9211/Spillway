package db

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Redis wraps the optional Redis client. The service runs without it.
type Redis struct {
	Client *redis.Client
}

// OpenRedis connects and pings.
func OpenRedis(ctx context.Context, url string) (*Redis, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	c := redis.NewClient(opts)
	if err := c.Ping(ctx).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return &Redis{Client: c}, nil
}

func (r *Redis) Name() string                   { return "redis" }
func (r *Redis) Ping(ctx context.Context) error { return r.Client.Ping(ctx).Err() }
func (r *Redis) Close() error                   { return r.Client.Close() }
