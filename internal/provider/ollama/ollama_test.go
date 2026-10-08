package ollama_test

import (
	"net/http/httptest"
	"testing"

	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/provider/ollama"
	"github.com/abdullah-9211/spillway/internal/provider/providertest"
)

func TestConformance(t *testing.T) {
	providertest.Run(t, providertest.Harness{New: func(t *testing.T, s providertest.Scenario) (provider.Provider, <-chan struct{}) {
		canceled := make(chan struct{})
		srv := httptest.NewServer(providertest.OpenAIWire(s, canceled))
		t.Cleanup(srv.Close)
		return ollama.New(provider.Config{Name: "ollama", BaseURL: srv.URL + "/v1"}), canceled
	}})
}

func TestNameAndDefaults(t *testing.T) {
	if got := ollama.New(provider.Config{}).Name(); got != "ollama" {
		t.Errorf("name = %q", got)
	}
}
