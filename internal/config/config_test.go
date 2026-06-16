package config

import (
	"os"
	"path/filepath"
	"testing"

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
