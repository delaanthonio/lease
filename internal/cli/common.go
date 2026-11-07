package cli

import (
	"os"

	"github.com/delaanthonio/lease/internal/config"
	"github.com/delaanthonio/lease/internal/identity"
)

// loadConfig loads the config file, reporting a bad file as a plain error.
func loadConfig(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// identityFromEnv derives the caller identity from the environment.
func identityFromEnv() identity.Identity {
	return identity.FromLookup(os.Getenv, identity.CurrentUser)
}
