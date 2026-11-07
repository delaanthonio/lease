// Package config resolves per-machine resource definitions from an
// optional TOML file. Resources absent from the file default to one
// exclusive slot.
package config

import (
	"fmt"
	"os"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// DefaultSlots is the slot count for a resource with no configured entry.
const DefaultSlots = 1

// Resource is one named machine resource.
type Resource struct {
	// Slots is how many callers may hold the resource at once.
	Slots int `toml:"slots"`
}

// Config is the resolved configuration.
type Config struct {
	// Path is the file the configuration came from, or "" when defaults
	// only were used.
	Path          string
	Resources     map[string]Resource
	RecordCommand bool
	ManualTTL     time.Duration
	Dir           string
	Shared        bool
}

type file struct {
	Dir           string              `toml:"dir"`
	Shared        bool                `toml:"shared"`
	ManualTTL     string              `toml:"manual_ttl"`
	RecordCommand bool                `toml:"record_command"`
	Resources     map[string]Resource `toml:"resources"`
}

// Load reads the config file at path. A missing file is not an error: the
// returned Config then holds defaults only and Path is empty.
func Load(path string) (*Config, error) {
	cfg := &Config{Resources: map[string]Resource{}, ManualTTL: 30 * time.Minute}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var f file
	if err := toml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	for name, r := range f.Resources {
		if r.Slots < 0 {
			return nil, fmt.Errorf("config %s: resource %q has negative slots", path, name)
		}
		cfg.Resources[name] = r
	}
	if f.ManualTTL != "" {
		d, err := time.ParseDuration(f.ManualTTL)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("invalid manual_ttl %q", f.ManualTTL)
		}
		cfg.ManualTTL = d
	}
	cfg.Dir, cfg.Shared = f.Dir, f.Shared
	cfg.RecordCommand = f.RecordCommand
	cfg.Path = path
	return cfg, nil
}

// Slots returns the slot count for a resource, defaulting to one exclusive
// slot for unknown names.
func (c *Config) Slots(resource string) int {
	if r, ok := c.Resources[resource]; ok && r.Slots > 0 {
		return r.Slots
	}
	return DefaultSlots
}
