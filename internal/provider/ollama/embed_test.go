package ollama_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/provider/ollama"
)

func TestEmbed(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"model":"nomic-embed-text","embeddings":[[0.1,0.2],[0.3,0.4]]}`))
	}))
	defer srv.Close()

	c := ollama.New(provider.Config{BaseURL: srv.URL + "/v1"})
	got, err := c.Embed(context.Background(), "nomic-embed-text", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/embed" || gotBody["model"] != "nomic-embed-text" {
		t.Errorf("path=%s body=%v; embeddings use the native endpoint at the server root", gotPath, gotBody)
	}
	if len(got) != 2 || got[1][0] != 0.3 {
		t.Errorf("embeddings = %v", got)
	}
}

func TestEmbedErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := ollama.New(provider.Config{BaseURL: srv.URL + "/v1"}).Embed(context.Background(), "nope", []string{"a"})
	if provider.KindOf(err) != provider.KindBadRequest {
		t.Errorf("err = %v", err)
	}

	short := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"embeddings":[[1]]}`))
	}))
	defer short.Close()
	if _, err := ollama.New(provider.Config{BaseURL: short.URL}).Embed(context.Background(), "m", []string{"a", "b"}); err == nil {
		t.Error("a short answer must be an error")
	}
}
