package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Config is parsed once here and passed down; nothing under internal/ reads the environment.
type Config struct {
	Role        string // api | worker | all
	Addr        string
	ConfigPath  string
	DatabaseURL string
	RedisURL    string // empty disables Redis-backed features
	LogLevel    slog.Level
}

var validRoles = map[string]bool{"api": true, "worker": true, "all": true}

// parseConfig resolves each setting as flag, then environment, then default.
func parseConfig(name string, args []string, getenv func(string) string, errOut io.Writer) (Config, error) {
	env := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}

	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(errOut)
	role := fs.String("role", env("SPILLWAY_ROLE", "all"), "api, worker or all")
	addr := fs.String("addr", env("SPILLWAY_ADDR", ":8080"), "listen address")
	cfgPath := fs.String("config", env("SPILLWAY_CONFIG", "config/models.yaml"), "model catalog path")
	dbURL := fs.String("database-url", env("DATABASE_URL", ""), "Postgres URL")
	redisURL := fs.String("redis-url", env("REDIS_URL", ""), "Redis URL (optional)")
	level := fs.String("log-level", env("LOG_LEVEL", "info"), "debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	if !validRoles[*role] {
		return Config{}, fmt.Errorf("invalid role %q: want api, worker or all", *role)
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(strings.ToLower(*level))); err != nil {
		return Config{}, fmt.Errorf("invalid log level %q", *level)
	}
	return Config{
		Role: *role, Addr: *addr, ConfigPath: *cfgPath,
		DatabaseURL: *dbURL, RedisURL: *redisURL, LogLevel: lvl,
	}, nil
}
