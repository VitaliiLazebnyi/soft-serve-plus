package main

import (
	"errors"
	"os"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/hooks"
)

// errHookInternal is what a hook tells the client when it cannot load the
// config; the details go to the hook log.
var errHookInternal = errors.New("internal server error")

// loadConfig loads cfg from the config file, if there is one, and then the
// environment, validating each.
func loadConfig(cfg *config.Config) error {
	if err := cfg.CheckConfigLocation(); err != nil {
		return err
	}
	// Parse reads the file and then the environment.
	parse := cfg.ParseEnv
	if cfg.Exist() {
		parse = cfg.Parse
	}
	return parse()
}

// loadHookConfig returns the config a `soft hook` command runs with: the
// server's effective config, from the file the server names in
// SOFT_SERVE_HOOK_CONFIG (see hooks.WriteConfig), so neither an edit to
// config.yaml after the server started nor a typo in it reaches the hook.
// A server that sets no such file, one from before it, gets the old load
// from config.yaml and the environment.
//
// On error it returns the error with a config that logs to the hook log,
// for the caller to report it there rather than to the client.
func loadHookConfig() (*config.Config, error) {
	cfg := config.DefaultConfig()
	if path := os.Getenv(hooks.ConfigEnv); path != "" && os.Getenv("SOFT_SERVE_DATA_PATH") != "" {
		hc, err := hooks.ReadConfig(path, cfg.DataPath)
		if err == nil {
			return hc, nil
		}
		return hookLogConfig(), err
	}
	if err := loadConfig(cfg); err != nil {
		return hookLogConfig(), err
	}
	return cfg, nil
}

// hookLogConfig returns the default config with the hook log the server
// names in SOFT_SERVE_LOG_PATH, if it does, as its log file.
func hookLogConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Log.Path = os.Getenv("SOFT_SERVE_LOG_PATH")
	return cfg
}
