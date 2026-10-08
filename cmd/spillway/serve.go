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

	"github.com/abdullah-9211/spillway/internal/api"
	"github.com/abdullah-9211/spillway/internal/auth"
	"github.com/abdullah-9211/spillway/internal/db"
	"github.com/abdullah-9211/spillway/internal/db/sqlcgen"
	"github.com/abdullah-9211/spillway/internal/gateway"
	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/telemetry"
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
	errc := make(chan error, 1)
	if cfg.Role == "api" || cfg.Role == "all" {
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
			Spend:    gateway.NewPGSpend(sqlcgen.New(pg.Pool)),
			Semantic: gateway.NewPGSemantic(pg.Pool),
			Observer: metrics,
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
			admin = api.NewAdmin(api.AdminDeps{Users: auth.NewUsers(pg.Pool, auth.DefaultParams), Keys: keys.NewStore(pg.Pool), Signer: signer, Deps: deps, Log: log})
		}

		ln, err := net.Listen("tcp", cfg.Addr)
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		handler := api.NewHandler(api.Options{Deps: deps, Gateway: gw, Auth: keys.NewStore(pg.Pool), Log: log, Metrics: metrics.Handler(), Admin: admin})
		srv = &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
		go func() { errc <- srv.Serve(ln) }()
		log.Info("listening", "addr", ln.Addr().String(), "role", cfg.Role)
	} else {
		log.Info("worker role: no HTTP listener in this phase", "role", cfg.Role)
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
	if gw != nil {
		gw.WaitForFills() // cache writes started by the last requests
	}
	if writer != nil { // after the server: in-flight requests may still record usage
		errs = append(errs, writer.Close(shutdownCtx))
		log.Info("usage flushed", "written", writer.Written(), "dropped", writer.Dropped())
	}
	return errors.Join(errs...)
}
