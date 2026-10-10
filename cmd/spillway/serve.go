package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/api"
	"github.com/abdullah-9211/spillway/internal/auth"
	"github.com/abdullah-9211/spillway/internal/db"
	"github.com/abdullah-9211/spillway/internal/db/sqlcgen"
	"github.com/abdullah-9211/spillway/internal/gateway"
	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/playground"
	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/runs"
	"github.com/abdullah-9211/spillway/internal/secret"
	"github.com/abdullah-9211/spillway/internal/telemetry"
	"github.com/abdullah-9211/spillway/internal/tools"
	"github.com/abdullah-9211/spillway/internal/usage"

	// Provider adapters register themselves on import.
	_ "github.com/abdullah-9211/spillway/internal/provider/anthropic"
	_ "github.com/abdullah-9211/spillway/internal/provider/google"
	_ "github.com/abdullah-9211/spillway/internal/provider/ollama"
	_ "github.com/abdullah-9211/spillway/internal/provider/openai"
)

func serve(ctx context.Context, args []string, getenv func(string) string, errOut io.Writer) error {
	cfg, err := parseConfig("serve", args, getenv, errOut)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(errOut, &slog.HandlerOptions{Level: cfg.LogLevel}))
	if cfg.DatabaseURL == "" {
		return errors.New("DATABASE_URL or --database-url is required")
	}

	pg, err := db.OpenPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pg.Close()
	deps := []api.Dependency{{Checker: pg, Required: true}}

	// Redis is optional: without it rate limits and the exact cache are off, and the service still starts.
	var rdb *db.Redis
	if cfg.RedisURL == "" {
		log.Warn("redis disabled: REDIS_URL not set, so rate limits and the exact cache are off")
	} else if rd, err := db.OpenRedis(ctx, cfg.RedisURL); err != nil {
		log.Warn("redis unavailable, so rate limits and the exact cache are off", "error", err)
	} else {
		rdb = rd
		defer rd.Close()
		deps = append(deps, api.Dependency{Checker: rd, Required: false})
	}

	shutdownTracing, err := telemetry.SetupTracing(ctx, getenv("OTEL_EXPORTER_OTLP_ENDPOINT"), "spillway")
	if err != nil {
		return err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(c)
	}()

	var srv *http.Server
	var writer *usage.Writer
	var gw *gateway.Gateway
	var poolDone, brokerDone chan struct{}
	errc := make(chan error, 1)
	// Both roles need the gateway: the api serves it over HTTP, a worker calls it in-process for a run's model calls.
	if cfg.Role == "api" || cfg.Role == "worker" || cfg.Role == "all" {
		cat, err := gateway.LoadCatalog(cfg.ConfigPath)
		if err != nil {
			return err
		}
		provs, missing, err := gateway.BuildProviders(cat, getenv, log)
		if err != nil {
			return err
		}
		writer = usage.NewWriter(pg.Pool, log, usage.WriterOptions{})
		gw = gateway.New(cat, provs, missing, writer, log)

		metrics := telemetry.NewMetrics()
		metrics.SetBreakerSource(func() map[string]int {
			out := map[string]int{}
			for p, st := range gw.BreakerStates() {
				out[p] = int(st)
			}
			return out
		})
		opts := gateway.Options{
			FaultInjection: cat.Playground.FaultInjection,
			Spend:          gateway.NewPGSpend(sqlcgen.New(pg.Pool)),
			Semantic:       gateway.NewPGSemantic(pg.Pool),
			Observer:       metrics,
		}
		if rdb != nil {
			opts.Limiter = gateway.NewRedisLimiter(rdb.Client)
			opts.Exact = gateway.NewRedisCache(rdb.Client)
		}
		if pc, ok := cat.Providers["ollama"]; ok {
			if emb, ok := provs["ollama"].(provider.Embedder); ok {
				opts.Embedder, opts.EmbedModel = emb, pc.EmbedModel
				if opts.EmbedModel == "" {
					opts.EmbedModel = "nomic-embed-text"
				}
			}
		}
		if opts.Embedder == nil {
			log.Info("semantic cache off: no ollama provider configured to embed with")
		}
		gw.Use(opts)
		keyStore := keys.NewStore(pg.Pool)
		runStore := runs.NewStore(pg.Pool)
		var box *secret.Box
		if k := getenv("SPILLWAY_SECRET_KEY"); k != "" {
			raw, err := secret.ParseKey(k)
			if err != nil {
				return fmt.Errorf("SPILLWAY_SECRET_KEY: %w", err)
			}
			if box, err = secret.New(raw); err != nil {
				return err
			}
		}
		toolStore := tools.NewStore(pg.Pool, box)

		if cfg.Role == "worker" || cfg.Role == "all" {
			workers := cfg.Workers
			if workers <= 0 {
				workers = cat.Runs.Workers
			}
			caller := &runs.GatewayCaller{GW: gw, Keys: keyStore}
			eng := &runs.Engine{Model: caller, Tools: &tools.Executor{Source: toolStore, WebhookSecret: getenv("SPILLWAY_WEBHOOK_SECRET")},
				Compactor: &runs.ModelCompactor{Model: caller, Policy: cat.Runs.CompactionPolicy}, Log: log, ToolBudget: cat.Runs.ToolErrorBudget}
			pool := runs.NewPool(runStore, eng, runs.PoolOptions{Workers: workers, LeaseTTL: cat.Runs.LeaseTTL, Heartbeat: cat.Runs.Heartbeat, Log: log})
			poolDone = make(chan struct{})
			go func() {
				defer close(poolDone)
				pool.Run(ctx)
			}()
		}
		if cfg.Role == "worker" {
			log.Info("worker role: no HTTP listener", "role", cfg.Role)
		}

		if cfg.Role != "worker" {
			broker := runs.NewBroker(pg.Pool, log)
			brokerDone = make(chan struct{})
			go func() {
				defer close(brokerDone)
				broker.Run(ctx)
			}()
			var admin *api.Admin
			if secret := getenv("ADMIN_SESSION_SECRET"); secret == "" {
				log.Warn("admin API off: ADMIN_SESSION_SECRET is not set, so the dashboard cannot sign in")
			} else {
				signer, err := auth.NewSigner([]byte(secret))
				if err != nil {
					return err
				}
				if err := seedUsers(ctx, pg.Pool, getenv, false, log); err != nil {
					return fmt.Errorf("seed users: %w", err)
				}
				budget, rpm := cat.Playground.Budget, cat.Playground.RateLimitRPM
				pkey, err := keyStore.EnsureBuiltin(ctx, &rpm, &budget)
				if err != nil {
					return err
				}
				pg_ := playground.NewService(gw, func(context.Context) (keys.Key, error) { return pkey, nil }, playground.NewStore(pg.Pool), cat.Playground.FaultInjection, log)
				admin = api.NewAdmin(api.AdminDeps{Users: auth.NewUsers(pg.Pool, auth.DefaultParams), Keys: keyStore, Usage: usage.NewReader(pg.Pool), Health: gw.ProviderHealth, Latency: metrics,
					Runs:   runs.NewReader(pg.Pool),
					Tools:  &toolsAdmin{store: toolStore, exec: &tools.Executor{Source: toolStore}, now: time.Now},
					Events: broker, RunStarter: starter{c: &runs.Creator{Store: runStore, Key: func(context.Context) (keys.Key, error) { return pkey, nil },
						Caps: runs.Caps{MaxSteps: cat.Runs.MaxSteps, MaxCost: cat.Runs.MaxCost, Deadline: cat.Runs.Deadline}, Known: cat.Has, ToolsReady: true, MissingTools: toolStore.Missing}, s: runStore},
					Playground: &api.PlaygroundDeps{Service: pg_, Catalog: gw.Catalog, Key: func(ctx context.Context, now time.Time) (keys.Stats, error) { return keyStore.Stats(ctx, pkey.ID, now) }},
					Signer:     signer, Deps: deps, Log: log})
			}

			ln, err := net.Listen("tcp", cfg.Addr)
			if err != nil {
				return fmt.Errorf("listen: %w", err)
			}
			handler := api.NewHandler(api.Options{Deps: deps, Gateway: gw, Auth: keyStore, Log: log, Metrics: metrics.Handler(), Admin: admin,
				Runs: &api.RunsOptions{Store: runStore, Caps: runs.Caps{MaxSteps: cat.Runs.MaxSteps, MaxCost: cat.Runs.MaxCost, Deadline: cat.Runs.Deadline}, Known: cat.Has, ToolsReady: true, MissingTools: toolStore.Missing, Events: broker}})
			// Event streams last as long as the client stays; shutting down ends them, or Shutdown would wait for them.
			streams, endStreams := context.WithCancel(context.Background())
			srv = &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return streams }}
			srv.RegisterOnShutdown(endStreams)
			go func() { errc <- srv.Serve(ln) }()
			log.Info("listening", "addr", ln.Addr().String(), "role", cfg.Role)
		}
	}

	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var errs []error
	if srv != nil {
		errs = append(errs, srv.Shutdown(shutdownCtx))
	}
	if brokerDone != nil {
		<-brokerDone // it holds a database connection until it stops
	}
	if poolDone != nil {
		<-poolDone // the pool stops on the same signal; wait for its grace period before the usage buffer closes
	}
	if gw != nil {
		gw.WaitForFills() // cache writes started by the last requests
	}
	if writer != nil { // after the server: in-flight requests may still record usage
		errs = append(errs, writer.Close(shutdownCtx))
		log.Info("usage flushed", "written", writer.Written(), "dropped", writer.Dropped())
	}
	return errors.Join(errs...)
}

// starter lets the admin API start runs (under the playground key) and cancel any run.
type starter struct {
	c *runs.Creator
	s *runs.Store
}

func (s starter) Create(ctx context.Context, req runs.Request, raw []byte) (runs.Run, error) {
	return s.c.Create(ctx, req, raw)
}

func (s starter) Decide(ctx context.Context, id uuid.UUID, d runs.Decision, now time.Time) (runs.Run, error) {
	return s.s.Decide(ctx, id, nil, d, now)
}

func (s starter) CancelAny(ctx context.Context, id uuid.UUID, now time.Time) (runs.Run, error) {
	return s.s.CancelAny(ctx, id, now)
}

// toolsAdmin is the registry for the admin API: the store, plus discovery, which needs the MCP client.
type toolsAdmin struct {
	store *tools.Store
	exec  *tools.Executor
	now   func() time.Time
}

func (t *toolsAdmin) List(ctx context.Context) ([]tools.Tool, error) { return t.store.List(ctx) }
func (t *toolsAdmin) Create(ctx context.Context, p tools.CreateParams) (tools.Tool, error) {
	return t.store.Create(ctx, p)
}
func (t *toolsAdmin) Update(ctx context.Context, id uuid.UUID, p tools.UpdateParams) (tools.Tool, error) {
	return t.store.Update(ctx, id, p)
}
func (t *toolsAdmin) Delete(ctx context.Context, id uuid.UUID) error { return t.store.Delete(ctx, id) }

func (t *toolsAdmin) Discover(ctx context.Context, id uuid.UUID) (tools.Tool, error) {
	return tools.DiscoverAndStore(ctx, t.store, t.exec, id, t.now())
}
