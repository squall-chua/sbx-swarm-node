// Package config loads node configuration from defaults, an optional YAML
// file, environment variables, and command-line flags, in that increasing
// order of precedence (flags > env > file > defaults).
package config

import (
	"flag"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the node configuration. Extended in M4 with cluster fields.
type Config struct {
	NodeName    string `yaml:"node_name"`
	DataDir     string `yaml:"data_dir"`
	ListenAddr  string `yaml:"listen_addr"`
	LogLevel    string `yaml:"log_level"`
	TLSCertFile string `yaml:"tls_cert_file"`
	TLSKeyFile  string `yaml:"tls_key_file"`
	// ConsoleAddr, when set, starts a SECOND browser-facing listener that serves
	// the embedded console SPA + REST over a browser-compatible cert. The main
	// ListenAddr keeps its pinned Ed25519 identity cert (ADR-0004), which
	// browsers reject. Empty disables the console listener.
	ConsoleAddr        string   `yaml:"console_addr"`
	ConsoleTLSCertFile string   `yaml:"console_tls_cert_file"` // optional; empty → self-signed ECDSA
	ConsoleTLSKeyFile  string   `yaml:"console_tls_key_file"`
	APIKeys            []APIKey `yaml:"api_keys"`

	// M4 cluster fields.
	ClusterSecret           string            `yaml:"cluster_secret"`
	Join                    []string          `yaml:"join"`
	SwarmName               string            `yaml:"swarm_name"`
	Labels                  map[string]string `yaml:"labels"`
	ProvisionLimits         ProvisionLimits   `yaml:"provision_limits"`
	GossipAddr              string            `yaml:"gossip_addr"`
	Workspaces              []WorkspaceConfig `yaml:"workspaces"`
	DefaultStrategy         string            `yaml:"default_strategy"`
	DefaultSandboxResources SandboxResources  `yaml:"default_sandbox_resources"`
	Backend                 string            `yaml:"backend"`          // "fake" (default) | "sdk"
	IdleTimeout             string            `yaml:"idle_timeout"`     // Go duration, e.g. "30m"; "" or <=0 disables idle-stop
	MaxUploadBytes          int64             `yaml:"max_upload_bytes"` // 0 → 100 MiB; per-request file-upload ceiling
}

// ProvisionLimits caps how much CPU/memory/disk this node offers to the swarm.
type ProvisionLimits struct {
	CPUCores    float64 `yaml:"cpu_cores"`
	MemoryBytes int64   `yaml:"memory_bytes"`
	DiskGB      float64 `yaml:"disk_gb"`
}

// WorkspaceConfig is a named host directory advertised for mounting/cloning.
type WorkspaceConfig struct {
	Name     string     `yaml:"name"`
	HostPath string     `yaml:"host_path"`
	ReadOnly bool       `yaml:"read_only"`
	Git      *GitConfig `yaml:"git,omitempty"` // non-nil => git-backed (clone-only, ADR-0015)
}

// GitConfig configures a git-backed workspace's pre/publish pipelines (ADR-0003).
// Credentials are operator host-side git config (ADR-0014) — there are no auth
// fields here.
type GitConfig struct {
	Remote        string     `yaml:"remote"`
	DefaultBranch string     `yaml:"default_branch"`
	AllowPush     bool       `yaml:"allow_push"`
	PreSteps      [][]string `yaml:"pre_steps"`
	PublishSteps  [][]string `yaml:"publish_steps"`
	ExecAllowlist []string   `yaml:"exec_allowlist"`
}

// WithDefaults returns a copy with unset fields filled with built-in defaults.
func (g GitConfig) WithDefaults() GitConfig {
	if g.Remote == "" {
		g.Remote = "origin"
	}
	if len(g.ExecAllowlist) == 0 {
		g.ExecAllowlist = []string{"git", "git-lfs"}
	}
	if len(g.PreSteps) == 0 {
		g.PreSteps = [][]string{{"git", "fetch", "{remote}", "+refs/heads/*:refs/heads/*"}}
	}
	if len(g.PublishSteps) == 0 {
		g.PublishSteps = [][]string{
			{"git", "fetch", "{sandbox_remote}", "+refs/heads/{branch}:refs/heads/{branch}"},
			{"git", "push", "{remote}", "{branch}"},
		}
	}
	return g
}

// SandboxResources is the per-sandbox default applied when a request omits a resource.
type SandboxResources struct {
	CPUCores    float64 `yaml:"cpu_cores"`
	MemoryBytes int64   `yaml:"memory_bytes"`
	DiskGB      float64 `yaml:"disk_gb"`
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
		Backend:    "fake",
	}
}

// Load builds a Config from defaults, an optional --config YAML file, env vars
// (SBX_ prefix), and flags. lookupEnv is injected for testability (use
// os.LookupEnv in production).
func Load(args []string, lookupEnv func(string) (string, bool)) (*Config, error) {
	cfg := Default()

	fs := flag.NewFlagSet("sbx-swarm-node", flag.ContinueOnError)
	var (
		configPath  = fs.String("config", "", "path to YAML config file")
		nodeName    = fs.String("node-name", "", "human-readable node name")
		dataDir     = fs.String("data-dir", "", "directory for node key and database")
		listenAddr  = fs.String("listen-addr", "", "address for the HTTP server")
		consoleAddr = fs.String("console-addr", "", "address for the browser console listener (empty disables)")
		logLevel    = fs.String("log-level", "", "debug|info|warn|error")
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
	if v, ok := lookupEnv("SBX_CONSOLE_ADDR"); ok {
		cfg.ConsoleAddr = v
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
		case "console-addr":
			cfg.ConsoleAddr = *consoleAddr
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
	if c.ClusterSecret != "" && (c.TLSCertFile != "" || c.TLSKeyFile != "") {
		return fmt.Errorf("tls_cert_file/tls_key_file is incompatible with clustering: clustered nodes use the node-key-derived TLS cert for peer pinning (ADR-0004)")
	}
	if c.GossipAddr == "" {
		return fmt.Errorf("gossip_addr must not be empty")
	}
	if c.ProvisionLimits.CPUCores < 0 || c.ProvisionLimits.MemoryBytes < 0 || c.ProvisionLimits.DiskGB < 0 {
		return fmt.Errorf("provision_limits must not be negative")
	}
	switch c.DefaultStrategy {
	case "", "least-loaded", "bin-pack", "spread":
	default:
		return fmt.Errorf("default_strategy must be one of least-loaded|bin-pack|spread, got %q", c.DefaultStrategy)
	}
	for _, w := range c.Workspaces {
		if w.Git != nil && w.HostPath == "" {
			return fmt.Errorf("workspace %q is git-backed but has no host_path", w.Name)
		}
	}
	switch c.Backend {
	case "", "fake", "sdk":
	default:
		return fmt.Errorf("backend must be one of fake|sdk, got %q", c.Backend)
	}
	if c.IdleTimeout != "" {
		d, err := time.ParseDuration(c.IdleTimeout)
		if err != nil {
			return fmt.Errorf("idle_timeout: %w", err)
		}
		if d < 0 {
			return fmt.Errorf("idle_timeout must not be negative, got %q", c.IdleTimeout)
		}
	}
	return nil
}

// IdleTimeoutDuration parses IdleTimeout (already validated; "" yields 0).
func (c *Config) IdleTimeoutDuration() time.Duration {
	d, _ := time.ParseDuration(c.IdleTimeout)
	return d
}
