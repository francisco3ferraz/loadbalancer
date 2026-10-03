// Package config loads the load balancer's YAML config file.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/francisco3ferraz/loadbalancer/internal/balancer"
)

const (
	defaultListen              = ":8000"
	defaultHealthCheckInterval = 5 * time.Second
)

// Config is the format of the config file. It's kept separate from
// balancer.Config: the file is a contract with whoever writes it, while
// balancer.Config is internal and free to change.
type Config struct {
	Listen              string        `yaml:"listen"`
	Algorithm           string        `yaml:"algorithm"`
	Backends            []Backend     `yaml:"backends"`
	HealthCheckInterval time.Duration `yaml:"health_check_interval"`
	AttemptTimeout      time.Duration `yaml:"attempt_timeout"`
	RequestTimeout      time.Duration `yaml:"request_timeout"`
	MaxFailures         int           `yaml:"max_failures"`

	// AdminListen is the address of the admin server, which serves /stats.
	// Unlike the other settings, empty doesn't mean a default: it means
	// the admin server is off.
	AdminListen string `yaml:"admin_listen"`
}

// Backend is one entry in the backends list. It can be written as a plain
// URL, or as an object with a url and a weight:
//
//	backends:
//	  - http://127.0.0.1:8080
//	  - url: http://127.0.0.1:8081
//	    weight: 3
//
// The plain form keeps config files from before weights existed working.
type Backend struct {
	URL    string `yaml:"url"`
	Weight int    `yaml:"weight"`
}

// UnmarshalYAML accepts both forms of a backend entry.
func (b *Backend) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		return value.Decode(&b.URL)
	}
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: backend must be a URL or an object with url and weight", value.Line)
	}

	// The decoder's KnownFields setting doesn't reach inside a custom
	// UnmarshalYAML, so check the keys here. Content alternates key, value.
	for i := 0; i < len(value.Content); i += 2 {
		if key := value.Content[i]; key.Value != "url" && key.Value != "weight" {
			return fmt.Errorf("line %d: unknown backend field %q (want url or weight)", key.Line, key.Value)
		}
	}

	// plain has Backend's fields but not its methods, so this Decode uses
	// the default decoding instead of calling UnmarshalYAML forever.
	type plain Backend
	return value.Decode((*plain)(b))
}

// Load reads a YAML config file and fills in the defaults for the settings
// that belong to the program rather than the balancer. The balancer settings
// are checked and defaulted by balancer.New.
func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("load config: %w", err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	// Reject unknown keys, so a typo like "algoritm" fails at startup
	// instead of being silently ignored.
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return Config{}, fmt.Errorf("load config %s: file is empty", path)
		}
		return Config{}, fmt.Errorf("load config %s: %w", path, err)
	}

	if cfg.Listen == "" {
		cfg.Listen = defaultListen
	}
	switch {
	case cfg.HealthCheckInterval == 0:
		cfg.HealthCheckInterval = defaultHealthCheckInterval
	case cfg.HealthCheckInterval < 0:
		return Config{}, fmt.Errorf("load config %s: health_check_interval must be positive", path)
	}
	return cfg, nil
}

// BalancerConfig returns the settings that configure the balancer itself.
func (c Config) BalancerConfig() balancer.Config {
	backends := make([]balancer.Backend, len(c.Backends))
	for i, b := range c.Backends {
		backends[i] = balancer.Backend{URL: b.URL, Weight: b.Weight}
	}
	return balancer.Config{
		Backends:       backends,
		Algorithm:      balancer.Algorithm(c.Algorithm),
		AttemptTimeout: c.AttemptTimeout,
		RequestTimeout: c.RequestTimeout,
		MaxFailures:    c.MaxFailures,
	}
}

// EffectiveRequestTimeout is the balancer's request timeout as it will
// actually be used, default included, so a server's own timeouts can be set
// above it.
func (c Config) EffectiveRequestTimeout() time.Duration {
	if c.RequestTimeout == 0 {
		return balancer.DefaultRequestTimeout
	}
	return c.RequestTimeout
}
