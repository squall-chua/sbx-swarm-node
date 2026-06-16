// Package config loads node configuration from defaults, an optional YAML
// file, environment variables, and command-line flags, in that increasing
// order of precedence (flags > env > file > defaults).
package config

import (
	"flag"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the M1a subset of node configuration. Later milestones extend it.
type Config struct {
	NodeName   string `yaml:"node_name"`
	DataDir    string `yaml:"data_dir"`
	ListenAddr string `yaml:"listen_addr"`
	LogLevel   string `yaml:"log_level"`
}

// Default returns the baseline configuration before any overrides.
func Default() *Config {
	host, _ := os.Hostname()
	if host == "" {
		host = "sbx-node"
	}
	return &Config{
		NodeName:   host,
		DataDir:    "./data",
		ListenAddr: ":8443",
		LogLevel:   "info",
	}
}

// Load builds a Config from defaults, an optional --config YAML file, env vars
// (SBX_ prefix), and flags. lookupEnv is injected for testability (use
// os.LookupEnv in production).
func Load(args []string, lookupEnv func(string) (string, bool)) (*Config, error) {
	cfg := Default()

	fs := flag.NewFlagSet("sbx-swarm-node", flag.ContinueOnError)
	var (
		configPath = fs.String("config", "", "path to YAML config file")
		nodeName   = fs.String("node-name", "", "human-readable node name")
		dataDir    = fs.String("data-dir", "", "directory for node key and database")
		listenAddr = fs.String("listen-addr", "", "address for the HTTP server")
		logLevel   = fs.String("log-level", "", "debug|info|warn|error")
	)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	// File layer.
	if *configPath != "" {
		raw, err := os.ReadFile(*configPath)
		if err != nil {
			return nil, fmt.Errorf("read config file: %w", err)
		}
		if err := yaml.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("parse config file: %w", err)
		}
	}

	// Env layer.
	if v, ok := lookupEnv("SBX_NODE_NAME"); ok {
		cfg.NodeName = v
	}
	if v, ok := lookupEnv("SBX_DATA_DIR"); ok {
		cfg.DataDir = v
	}
	if v, ok := lookupEnv("SBX_LISTEN_ADDR"); ok {
		cfg.ListenAddr = v
	}
	if v, ok := lookupEnv("SBX_LOG_LEVEL"); ok {
		cfg.LogLevel = v
	}

	// Flag layer (only flags the user actually set).
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "node-name":
			cfg.NodeName = *nodeName
		case "data-dir":
			cfg.DataDir = *dataDir
		case "listen-addr":
			cfg.ListenAddr = *listenAddr
		case "log-level":
			cfg.LogLevel = *logLevel
		}
	})

	return cfg, nil
}

// Validate checks the configuration for obvious mistakes.
func (c *Config) Validate() error {
	if c.NodeName == "" {
		return fmt.Errorf("node_name must not be empty")
	}
	if c.DataDir == "" {
		return fmt.Errorf("data_dir must not be empty")
	}
	if c.ListenAddr == "" {
		return fmt.Errorf("listen_addr must not be empty")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level must be one of debug|info|warn|error, got %q", c.LogLevel)
	}
	return nil
}
