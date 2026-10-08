package provider

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
)

// Config is what an adapter factory needs to build a Provider.
type Config struct {
	Name       string // the provider's name in the catalog
	APIKey     string
	BaseURL    string // empty means the adapter's default
	HTTPClient *http.Client
}

type Factory func(Config) (Provider, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register is called from each adapter's init. Adding a provider is one package plus a blank import.
func Register(adapter string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := factories[adapter]; dup {
		panic("provider: adapter registered twice: " + adapter)
	}
	factories[adapter] = f
}

func New(adapter string, cfg Config) (Provider, error) {
	mu.RLock()
	f, ok := factories[adapter]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no adapter named %q (have %v)", adapter, Adapters())
	}
	return f(cfg)
}

func Adapters() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(factories))
	for n := range factories {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
