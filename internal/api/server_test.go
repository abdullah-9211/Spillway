package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeDep struct {
	name string
	err  error
}

func (f fakeDep) Name() string               { return f.name }
func (f fakeDep) Ping(context.Context) error { return f.err }

func TestHealthz(t *testing.T) {
	boom := errors.New("down")
	tests := []struct {
		name     string
		deps     []Dependency
		wantCode int
		wantBody health
	}{
		{"no dependencies", nil, 200, health{Status: "ok", Dependencies: map[string]string{}}},
		{"all up", []Dependency{
			{fakeDep{"postgres", nil}, true}, {fakeDep{"redis", nil}, false},
		}, 200, health{Status: "ok", Dependencies: map[string]string{"postgres": "up", "redis": "up"}}},
		{"optional down stays ok", []Dependency{
			{fakeDep{"postgres", nil}, true}, {fakeDep{"redis", boom}, false},
		}, 200, health{Status: "ok", Dependencies: map[string]string{"postgres": "up", "redis": "down"}}},
		{"required down is 503", []Dependency{
			{fakeDep{"postgres", boom}, true},
		}, 503, health{Status: "unavailable", Dependencies: map[string]string{"postgres": "down"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			NewHandler(Options{Deps: tc.deps}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			if rec.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d", rec.Code, tc.wantCode)
			}
			var got health
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Status != tc.wantBody.Status {
				t.Errorf("status = %q, want %q", got.Status, tc.wantBody.Status)
			}
			for k, v := range tc.wantBody.Dependencies {
				if got.Dependencies[k] != v {
					t.Errorf("dependency %s = %q, want %q", k, got.Dependencies[k], v)
				}
			}
		})
	}
}

func TestHealthzRejectsPost(t *testing.T) {
	rec := httptest.NewRecorder()
	NewHandler(Options{}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code = %d, want 405", rec.Code)
	}
}
