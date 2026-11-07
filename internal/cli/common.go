package cli

import (
	"os"

	"github.com/delaanthonio/lease/internal/config"
	"github.com/delaanthonio/lease/internal/identity"
	"github.com/delaanthonio/lease/internal/paths"
)

// loadConfig loads the config file, reporting a bad file as a plain error.
func loadConfig(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	paths.Shared = cfg.Shared
	return cfg, nil
}

// identityFromEnv derives the caller identity from the environment.
func identityFromEnv() identity.Identity {
	return identity.FromLookup(os.Getenv, identity.CurrentUser)
}

func stateDir(cfg *config.Config) string {
	if v := os.Getenv("LEASE_DIR"); v != "" {
		return v
	}
	if v := os.Getenv(paths.StateDirEnv); v != "" {
		return v
	}
	if cfg.Dir != "" {
		return cfg.Dir
	}
	return paths.StateDir(os.LookupEnv)
}
