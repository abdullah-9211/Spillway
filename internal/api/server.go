// Package api holds the HTTP handlers.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/abdullah-9211/spillway/internal/gateway"
	"github.com/abdullah-9211/spillway/internal/keys"
)

// Checker is a dependency whose health is reported by /healthz.
type Checker interface {
	Name() string
	Ping(ctx context.Context) error
}

// Dependency is a checker plus whether its failure makes the service unhealthy.
type Dependency struct {
	Checker  Checker
	Required bool
}

// Authenticator resolves a bearer token to an API key.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (keys.Key, error)
}

// Options are what the handler needs. Gateway and Auth may be nil, which leaves only /healthz.
type Options struct {
	Deps    []Dependency
	Gateway *gateway.Gateway
	Auth    Authenticator
	Log     *slog.Logger
	// Metrics, when set, is served at GET /metrics.
	Metrics http.Handler
	// Admin, when set, serves the dashboard's API under /admin/.
	Admin *Admin
	// Runs, when set (with Auth), serves /v1/runs.
	Runs *RunsOptions
}

// NewHandler builds the router. Required dependencies that fail their ping turn /healthz into a 503;
// optional ones (Redis) are reported as "down" without failing it.
func NewHandler(o Options) http.Handler {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz(o.Deps))
	if o.Admin != nil {
		mux.Handle("/admin/", o.Admin.Handler())
	}
	if o.Metrics != nil {
		mux.Handle("GET /metrics", o.Metrics)
	}
	if o.Gateway != nil && o.Auth != nil {
		v1 := &v1{gw: o.Gateway, auth: o.Auth, log: o.Log}
		mux.Handle("POST /v1/chat/completions", v1.withRequest(v1.chatCompletions))
		mux.Handle("GET /v1/models", v1.withRequest(v1.models))
	}
	if o.Runs != nil && o.Auth != nil {
		(&runsAPI{RunsOptions: *o.Runs, v1: &v1{auth: o.Auth, log: o.Log}, log: o.Log}).register(mux)
	}
	return mux
}

type health struct {
	Status       string            `json:"status"`
	Dependencies map[string]string `json:"dependencies,omitempty"`
}

func healthz(deps []Dependency) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		out := health{Status: "ok", Dependencies: map[string]string{}}
		code := http.StatusOK
		for _, d := range deps {
			if err := d.Checker.Ping(ctx); err != nil {
				out.Dependencies[d.Checker.Name()] = "down"
				if d.Required {
					out.Status = "unavailable"
					code = http.StatusServiceUnavailable
				}
				continue
			}
			out.Dependencies[d.Checker.Name()] = "up"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(out)
	}
}
