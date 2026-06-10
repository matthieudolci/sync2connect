// Package config loads the YAML configuration file. String values may
// reference environment variables with ${VAR_NAME}, which is the recommended
// way to keep secrets out of the file.
package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so YAML values like "1h30m" parse naturally.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", raw, err)
	}
	*d = Duration(parsed)
	return nil
}

// Route connects one source provider to one destination provider.
type Route struct {
	Source      string `yaml:"source"`
	Destination string `yaml:"destination"`
}

// Sync controls the sync engine.
type Sync struct {
	// Interval between sync runs. Zero or omitted means run once and exit.
	Interval Duration `yaml:"interval"`
	// InitialLookback bounds how far back the first sync reaches when no
	// previous sync state exists. Defaults to 30 days.
	InitialLookback Duration `yaml:"initial_lookback"`
	Routes          []Route  `yaml:"routes"`
}

// Config is the root of the configuration file.
type Config struct {
	// StateDir stores tokens and sync state. Defaults to the
	// SYNC2CONNECT_STATE_DIR environment variable, then "./data".
	StateDir  string                       `yaml:"state_dir"`
	Sync      Sync                         `yaml:"sync"`
	Providers map[string]map[string]string `yaml:"providers"`
}

var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces ${VAR} references with the environment variable's value.
// Unset variables expand to the empty string.
func expandEnv(raw []byte) []byte {
	return envPattern.ReplaceAllFunc(raw, func(match []byte) []byte {
		name := envPattern.FindSubmatch(match)[1]
		return []byte(os.Getenv(string(name)))
	})
}

// Load reads, expands and validates the configuration file at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(expandEnv(raw), &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.StateDir == "" {
		c.StateDir = os.Getenv("SYNC2CONNECT_STATE_DIR")
	}
	if c.StateDir == "" {
		c.StateDir = "data"
	}
	if c.Sync.InitialLookback == 0 {
		c.Sync.InitialLookback = Duration(30 * 24 * time.Hour)
	}
	if c.Providers == nil {
		c.Providers = map[string]map[string]string{}
	}
}

func (c *Config) validate() error {
	if len(c.Sync.Routes) == 0 {
		return fmt.Errorf("sync.routes must define at least one route")
	}
	for i, r := range c.Sync.Routes {
		if r.Source == "" || r.Destination == "" {
			return fmt.Errorf("sync.routes[%d]: source and destination are required", i)
		}
		if r.Source == r.Destination {
			return fmt.Errorf("sync.routes[%d]: source and destination must differ", i)
		}
	}
	if c.Sync.Interval < 0 {
		return fmt.Errorf("sync.interval must not be negative")
	}
	return nil
}

// ProviderSettings returns the settings section for the named provider,
// which may be empty but never nil.
func (c *Config) ProviderSettings(name string) map[string]string {
	if s := c.Providers[name]; s != nil {
		return s
	}
	return map[string]string{}
}
