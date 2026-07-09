package git

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCredential_Env_HTTPSToken(t *testing.T) {
	c := Credential{Token: "SENTINEL-TOKEN-9f3a", CAPath: "/ca.pem"}
	env, err := c.Env("https://github.com/acme/app")
	require.NoError(t, err)
	joined := strings.Join(env, "\n")
	require.Contains(t, joined, "GIT_TERMINAL_PROMPT=0")
	require.Contains(t, joined, "GIT_SSL_CAINFO=/ca.pem")
	// token carried in a GIT_CONFIG_VALUE_* extraheader, base64'd:
	require.Contains(t, joined, "http.https://github.com/acme/app.extraheader")
	require.Contains(t, joined, "Authorization: Basic ")
	// the raw token must NOT appear anywhere in the env values verbatim
	// (it is base64-encoded inside the header):
	require.NotContains(t, joined, "SENTINEL-TOKEN-9f3a")
}

func TestCredential_Env_SSHKey(t *testing.T) {
	c := Credential{SSHKeyPath: "/k/id", SSHKnownHostsPath: "/k/known_hosts"}
	env, err := c.Env("ssh://git@host/repo")
	require.NoError(t, err)
	joined := strings.Join(env, "\n")
	require.Contains(t, joined, "GIT_SSH_COMMAND=")
	require.Contains(t, joined, "-i '/k/id'")
	require.Contains(t, joined, "StrictHostKeyChecking=yes")
	require.Contains(t, joined, "UserKnownHostsFile='/k/known_hosts'")
}

func TestCredential_Env_SSHKey_QuotesPathsWithSpaces(t *testing.T) {
	c := Credential{SSHKeyPath: "/home/op user/id", SSHKnownHostsPath: "/home/op user/known_hosts"}
	env, err := c.Env("ssh://git@host/repo")
	require.NoError(t, err)
	joined := strings.Join(env, "\n")
	// A path with a space must be shell-quoted as one argument, not split.
	require.Contains(t, joined, "-i '/home/op user/id'")
	require.Contains(t, joined, "UserKnownHostsFile='/home/op user/known_hosts'")
}

func TestCredential_Env_SSHKey_NoKnownHosts_AcceptNew(t *testing.T) {
	c := Credential{SSHKeyPath: "/k/id"}
	env, err := c.Env("ssh://git@host/repo")
	require.NoError(t, err)
	require.Contains(t, strings.Join(env, "\n"), "StrictHostKeyChecking=accept-new")
}
