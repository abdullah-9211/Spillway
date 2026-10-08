//go:build integration

package gateway

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Fatal("REDIS_URL is not set; run `make up` and use `make test`")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	c := redis.NewClient(opts)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestBucketBurstThenRefuse(t *testing.T) {
	l := NewRedisLimiter(testRedis(t))
	id := uuid.New()
	ctx := context.Background()

	// 60 rpm: a burst of 60 is allowed, the 61st is refused with a wait of about a second.
	for i := 0; i < 60; i++ {
		ok, _, err := l.Allow(ctx, id, 60)
		if err != nil || !ok {
			t.Fatalf("request %d: ok=%v err=%v", i+1, ok, err)
		}
	}
	ok, retry, err := l.Allow(ctx, id, 60)
	if err != nil || ok {
		t.Fatalf("request 61 should be refused: ok=%v err=%v", ok, err)
	}
	if retry < 500*time.Millisecond || retry > 1100*time.Millisecond {
		t.Errorf("retry after = %v, want about 1s at 1 token/s", retry)
	}
}

func TestBucketRefillsOverTime(t *testing.T) {
	l := NewRedisLimiter(testRedis(t))
	id := uuid.New()
	ctx := context.Background()
	for i := 0; i < 120; i++ { // 120 rpm = 2 tokens/s
		l.Allow(ctx, id, 120)
	}
	if ok, _, _ := l.Allow(ctx, id, 120); ok {
		t.Fatal("bucket should be empty")
	}
	time.Sleep(600 * time.Millisecond) // about 1.2 tokens
	if ok, _, _ := l.Allow(ctx, id, 120); !ok {
		t.Error("a token should have refilled")
	}
	if ok, _, _ := l.Allow(ctx, id, 120); ok {
		t.Error("only one token should have refilled")
	}
}

func TestBucketsAreIndependentPerKey(t *testing.T) {
	l := NewRedisLimiter(testRedis(t))
	a, b := uuid.New(), uuid.New()
	ctx := context.Background()
	l.Allow(ctx, a, 1)
	if ok, _, _ := l.Allow(ctx, a, 1); ok {
		t.Error("a is spent")
	}
	if ok, _, _ := l.Allow(ctx, b, 1); !ok {
		t.Error("b must not be affected by a")
	}
}

func TestBucketIsAtomicUnderConcurrency(t *testing.T) {
	l := NewRedisLimiter(testRedis(t))
	id := uuid.New()
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _, err := l.Allow(context.Background(), id, 50); err == nil && ok {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	// Capacity 50 plus whatever refilled during the run (50/60 per second, well under one token here).
	if n := allowed.Load(); n < 50 || n > 51 {
		t.Errorf("allowed %d of 200 against a bucket of 50", n)
	}
}

func TestZeroRPMMeansNoLimit(t *testing.T) {
	l := NewRedisLimiter(testRedis(t))
	if ok, _, err := l.Allow(context.Background(), uuid.New(), 0); !ok || err != nil {
		t.Errorf("ok=%v err=%v", ok, err)
	}
}

func TestRedisDownIsAnErrorNotAPanic(t *testing.T) {
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
	defer c.Close()
	if _, _, err := NewRedisLimiter(c).Allow(context.Background(), uuid.New(), 10); err == nil {
		t.Error("expected an error when Redis is unreachable")
	}
}
