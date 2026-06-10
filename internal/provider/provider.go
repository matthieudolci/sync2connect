// Package provider defines the interfaces every data provider implements and
// a registry that maps provider names to factories. A new provider only needs
// to implement Source and/or Destination and call Register from an init
// function; the sync engine and CLI discover it by name from the config file.
package provider

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/matthieudolci/sync2connect/internal/model"
)

// Settings holds the provider-specific key/value options from the config
// file, with environment variables already expanded.
type Settings map[string]string

// Get returns the value for key, or def when the key is absent or empty.
func (s Settings) Get(key, def string) string {
	if v := s[key]; v != "" {
		return v
	}
	return def
}

// Config is everything a factory needs to construct a provider.
type Config struct {
	// Settings is the provider's section from the config file.
	Settings Settings
	// StateDir is a writable directory for tokens and other provider state.
	StateDir string
	// ManualAuth requests a copy/paste authentication flow instead of a
	// local callback server (for headless machines and containers).
	ManualAuth bool
}

// Provider is the base interface shared by all providers.
type Provider interface {
	Name() string
}

// Source can fetch body measurements that changed since a given time.
type Source interface {
	Provider
	FetchBody(ctx context.Context, since time.Time) ([]model.BodyMeasurement, error)
}

// Destination can receive body measurements.
type Destination interface {
	Provider
	PushBody(ctx context.Context, measurements []model.BodyMeasurement) error
}

// PromptFunc asks the user for a value (e.g. an OAuth code or MFA code).
// When secret is true the input should not be echoed.
type PromptFunc func(label string, secret bool) (string, error)

// Authenticator is implemented by providers that need an interactive,
// one-time authentication step (`sync2connect auth <provider>`).
type Authenticator interface {
	Provider
	Authenticate(ctx context.Context, prompt PromptFunc) error
}

// Factory constructs a provider instance from its configuration.
type Factory func(cfg Config) (Provider, error)

var registry = map[string]Factory{}

// Register makes a provider available under the given name. It is meant to
// be called from the provider package's init function.
func Register(name string, factory Factory) {
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("provider: Register called twice for %q", name))
	}
	registry[name] = factory
}

// New instantiates the named provider.
func New(name string, cfg Config) (Provider, error) {
	factory, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q (available: %v)", name, Names())
	}
	return factory(cfg)
}

// Names lists the registered providers in alphabetical order.
func Names() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
