package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func noEnv(string) (string, bool) { return "", false }

func TestLoad_PrecedenceFlagsOverEnvOverFileOverDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("node_name: fromfile\nlog_level: warn\n"), 0o600))

	env := func(k string) (string, bool) {
		if k == "SBX_NODE_NAME" {
			return "fromenv", true
		}
		return "", false
	}

	// file sets node_name+log_level; env overrides node_name; flag overrides node_name again.
	cfg, err := Load([]string{"--config", cfgPath, "--node-name", "fromflag"}, env)
	require.NoError(t, err)
	require.Equal(t, "fromflag", cfg.NodeName) // flag wins
	require.Equal(t, "warn", cfg.LogLevel)     // from file (not overridden)
}

func TestLoad_Defaults(t *testing.T) {
	cfg, err := Load(nil, noEnv)
	require.NoError(t, err)
	require.NotEmpty(t, cfg.NodeName) // defaults to hostname
	require.Equal(t, "./data", cfg.DataDir)
	require.Equal(t, ":8443", cfg.ListenAddr)
	require.Equal(t, "info", cfg.LogLevel)
}

func TestBackend_DefaultAndValidate(t *testing.T) {
	require.Equal(t, "fake", Default().Backend) // daemonless default keeps tests/boot green

	c := Default()
	c.Backend = "sdk"
	require.NoError(t, c.Validate())

	c.Backend = "bogus"
	require.Error(t, c.Validate())
}

func TestValidate(t *testing.T) {
	ok := Default()
	require.NoError(t, ok.Validate())

	bad := Default()
	bad.LogLevel = "loud"
	require.Error(t, bad.Validate())

	empty := Default()
	empty.DataDir = ""
	require.Error(t, empty.Validate())
}

func TestRoleForKey(t *testing.T) {
	c := Default()
	c.APIKeys = []APIKey{{Key: "adm", Role: "admin"}, {Key: "ro", Role: "read-only"}}
	role, ok := c.RoleForKey("adm")
	require.True(t, ok)
	require.Equal(t, "admin", role)
	_, ok = c.RoleForKey("nope")
	require.False(t, ok)
}

func TestValidate_RejectsBadRole(t *testing.T) {
	c := Default()
	c.APIKeys = []APIKey{{Key: "x", Role: "wizard"}}
	require.Error(t, c.Validate())
}

func TestValidate_ClusterWithCustomTLS(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{
			name: "clustered with custom cert file is invalid",
			mutate: func(c *Config) {
				c.ClusterSecret = "s3cret"
				c.TLSCertFile = "/etc/tls/node.crt"
			},
			wantErr: true,
		},
		{
			name: "clustered with custom key file is invalid",
			mutate: func(c *Config) {
				c.ClusterSecret = "s3cret"
				c.TLSKeyFile = "/etc/tls/node.key"
			},
			wantErr: true,
		},
		{
			name: "clustered with both custom cert and key files is invalid",
			mutate: func(c *Config) {
				c.ClusterSecret = "s3cret"
				c.TLSCertFile = "/etc/tls/node.crt"
				c.TLSKeyFile = "/etc/tls/node.key"
			},
			wantErr: true,
		},
		{
			name: "standalone with custom cert and key files is valid",
			mutate: func(c *Config) {
				c.TLSCertFile = "/etc/tls/node.crt"
				c.TLSKeyFile = "/etc/tls/node.key"
			},
			wantErr: false,
		},
		{
			name: "clustered without custom cert files is valid",
			mutate: func(c *Config) {
				c.ClusterSecret = "s3cret"
			},
			wantErr: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.mutate(c)
			err := c.Validate()
			if tc.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), "tls")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidate_NegativeDiskLimitRejected(t *testing.T) {
	c := Default()
	c.ProvisionLimits.DiskGB = -1
	require.Error(t, c.Validate())
}

func TestValidate_ClusterFields(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{
			name:    "join without secret is invalid",
			mutate:  func(c *Config) { c.Join = []string{"10.0.0.1:7946"} },
			wantErr: true,
		},
		{
			name: "join with secret is valid",
			mutate: func(c *Config) {
				c.Join = []string{"10.0.0.1:7946"}
				c.ClusterSecret = "s3cret"
			},
			wantErr: false,
		},
		{
			name:    "empty gossip_addr is invalid",
			mutate:  func(c *Config) { c.GossipAddr = "" },
			wantErr: true,
		},
		{
			name:    "distinct gossip_addr is valid",
			mutate:  func(c *Config) { c.GossipAddr = ":7947" },
			wantErr: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.mutate(c)
			err := c.Validate()
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestConfig_MaxUploadBytes(t *testing.T) {
	var c Config
	require.Equal(t, int64(0), c.MaxUploadBytes) // unset → 0 (handler defaults to 100 MiB)
	c.MaxUploadBytes = 5 << 20
	require.Equal(t, int64(5<<20), c.MaxUploadBytes)
}

func TestGitConfig_WithDefaults(t *testing.T) {
	g := GitConfig{}.WithDefaults()
	require.Equal(t, "origin", g.Remote)
	require.Equal(t, []string{"git", "git-lfs"}, g.ExecAllowlist)
	require.Equal(t, [][]string{{"git", "fetch", "{remote}", "+refs/heads/*:refs/heads/*"}}, g.PreSteps)
	require.Equal(t, [][]string{
		{"git", "fetch", "{sandbox_remote}", "+refs/heads/{branch}:refs/heads/{branch}"},
		{"git", "push", "{remote}", "{branch}"},
	}, g.PublishSteps)

	// Explicit values are preserved.
	g2 := GitConfig{Remote: "up", ExecAllowlist: []string{"git"}}.WithDefaults()
	require.Equal(t, "up", g2.Remote)
	require.Equal(t, []string{"git"}, g2.ExecAllowlist)
}

func TestGitConfig_ProviderFields(t *testing.T) {
	g := GitConfig{
		RemoteURL: "https://github.com/acme/app",
		Provider:  "github",
		TokenEnv:  "ACME_GH_TOKEN",
		CAPath:    "/etc/sbx/acme-ca.pem",
	}.WithDefaults()
	require.Equal(t, "https://github.com/acme/app", g.RemoteURL)
	require.Equal(t, "github", g.Provider)
	require.Equal(t, "ACME_GH_TOKEN", g.TokenEnv)
	// back-compat defaults still applied:
	require.Equal(t, "origin", g.Remote)
	require.Equal(t, []string{"git", "git-lfs"}, g.ExecAllowlist)
}

func TestValidate_GitWorkspaceNeedsHostPath(t *testing.T) {
	cfg := Default()
	cfg.Workspaces = []WorkspaceConfig{{Name: "repo", Git: &GitConfig{}}}
	require.ErrorContains(t, cfg.Validate(), "host_path")
}

func TestValidate_GitWorkspaceProviderWithoutHostPathAllowed(t *testing.T) {
	// ADR-0020: a provider workspace (remote_url set) may omit host_path — the
	// node auto-manages the mirror base.
	cfg := Default()
	cfg.Workspaces = []WorkspaceConfig{{Name: "repo", Git: &GitConfig{RemoteURL: "https://github.com/acme/app"}}}
	require.NoError(t, cfg.Validate())
}

func TestIdleTimeout_ValidateAndDuration(t *testing.T) {
	c := Default()
	require.Equal(t, "", c.IdleTimeout) // disabled by default
	require.Equal(t, time.Duration(0), c.IdleTimeoutDuration())

	c.IdleTimeout = "30m"
	require.NoError(t, c.Validate())
	require.Equal(t, 30*time.Minute, c.IdleTimeoutDuration())

	c.IdleTimeout = "garbage"
	require.Error(t, c.Validate())

	c.IdleTimeout = "-5m"
	require.Error(t, c.Validate())
}
