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

// Config is the node configuration. Extended in M4 with cluster fields.
type Config struct {
	NodeName    string   `yaml:"node_name"`
	DataDir     string   `yaml:"data_dir"`
	ListenAddr  string   `yaml:"listen_addr"`
	LogLevel    string   `yaml:"log_level"`
	TLSCertFile string   `yaml:"tls_cert_file"`
	TLSKeyFile  string   `yaml:"tls_key_file"`
	APIKeys     []APIKey `yaml:"api_keys"`

	// M4 cluster fields.
	ClusterSecret string            `yaml:"cluster_secret"`
	Join          []string          `yaml:"join"`
	SwarmName     string            `yaml:"swarm_name"`
	Labels        map[string]string `yaml:"labels"`
	ProvisionLimits ProvisionLimits `yaml:"provision_limits"`
	GossipAddr    string            `yaml:"gossip_addr"`
}

// ProvisionLimits caps how much CPU/memory this node offers to the swarm.
type ProvisionLimits struct {
	CPUCores    float64 `yaml:"cpu_cores"`
	MemoryBytes int64   `yaml:"memory_bytes"`
}

// APIKey is a bearer credential mapped to a role ("admin"|"read-only").
type APIKey struct {
	Key  string `yaml:"key"`
	Role string `yaml:"role"`
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
		GossipAddr: ":7946",
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

// RoleForKey returns the role for a bearer key, if configured.
func (c *Config) RoleForKey(key string) (string, bool) {
	for _, k := range c.APIKeys {
		if k.Key == key {
			return k.Role, true
		}
	}
	return "", false
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
	for _, k := range c.APIKeys {
		if k.Key == "" {
			return fmt.Errorf("api_keys: empty key")
		}
		if k.Role != "admin" && k.Role != "read-only" {
			return fmt.Errorf("api_keys: role must be admin|read-only, got %q", k.Role)
		}
	}
	if len(c.Join) > 0 && c.ClusterSecret == "" {
		return fmt.Errorf("cluster_secret must be set when join seeds are configured")
	}
	if c.GossipAddr == "" {
		return fmt.Errorf("gossip_addr must not be empty")
	}
	return nil
}
