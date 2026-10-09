package main

import (
	"io"
	"log/slog"
	"testing"
)

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestParseConfig(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		want    Config
		wantErr bool
	}{
		{"defaults", nil, nil,
			Config{Role: "all", Addr: ":8080", ConfigPath: "config/models.yaml", LogLevel: slog.LevelInfo}, false},
		{"env", nil, map[string]string{"SPILLWAY_ROLE": "worker", "DATABASE_URL": "postgres://x", "REDIS_URL": "redis://y", "LOG_LEVEL": "debug"},
			Config{Role: "worker", Addr: ":8080", ConfigPath: "config/models.yaml", DatabaseURL: "postgres://x", RedisURL: "redis://y", LogLevel: slog.LevelDebug}, false},
		{"flag beats env", []string{"--role=api", "--addr=:9000"}, map[string]string{"SPILLWAY_ROLE": "worker", "SPILLWAY_ADDR": ":1"},
			Config{Role: "api", Addr: ":9000", ConfigPath: "config/models.yaml", LogLevel: slog.LevelInfo}, false},
		{"worker count", []string{"--role=worker", "--workers=3"}, nil, Config{Role: "worker", Addr: ":8080", ConfigPath: "config/models.yaml", LogLevel: slog.LevelInfo, Workers: 3}, false},
		{"negative worker count", []string{"--workers=-1"}, nil, Config{}, true},
		{"bad role", []string{"--role=boss"}, nil, Config{}, true},
		{"bad level", []string{"--log-level=loud"}, nil, Config{}, true},
		{"unknown flag", []string{"--nope"}, nil, Config{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseConfig("test", tc.args, envMap(tc.env), io.Discard)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
