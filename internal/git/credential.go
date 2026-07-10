package git

import (
	"encoding/base64"
	"strings"
)

// Credential is a workspace's node-side upstream credential + trust (ADR-0019).
// Zero value = no credential (legacy ambient-config behavior). Never logged,
// never returned to callers, never placed in argv.
type Credential struct {
	Token             string // HTTPS token
	SSHKeyPath        string // SSH private key path
	SSHKnownHostsPath string // pins the SSH host key; "" => accept-new
	CAPath            string // internal-CA / self-signed PEM (HTTPS)
}

// String redacts the credential so a stray %v/%+v (directly or via an enclosing
// struct like gitprovider.Env) can never print the token or key paths.
func (c Credential) String() string { return "Credential{<redacted>}" }

// Env returns environment variables that apply this credential + trust to a git
// child process for remoteURL. The token is injected as an http extraheader via
// GIT_CONFIG_* env (git >= 2.31), keeping it out of argv. SSH uses GIT_SSH_COMMAND.
func (c Credential) Env(remoteURL string) ([]string, error) {
	env := []string{"GIT_TERMINAL_PROMPT=0"}

	// HTTPS token -> http.<url>.extraheader Authorization: Basic base64("x-access-token:<token>")
	if c.Token != "" {
		hdr := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+c.Token))
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http."+remoteURL+".extraheader",
			"GIT_CONFIG_VALUE_0="+hdr,
		)
	}

	// SSH key + host-key policy. git runs GIT_SSH_COMMAND through the shell, so the
	// operator-supplied paths are shell-quoted to survive spaces or metacharacters.
	if c.SSHKeyPath != "" {
		ssh := "ssh -i " + shQuote(c.SSHKeyPath) + " -o IdentitiesOnly=yes"
		if c.SSHKnownHostsPath != "" {
			ssh += " -o StrictHostKeyChecking=yes -o UserKnownHostsFile=" + shQuote(c.SSHKnownHostsPath)
		} else {
			ssh += " -o StrictHostKeyChecking=accept-new"
		}
		env = append(env, "GIT_SSH_COMMAND="+ssh)
	}

	// CA trust for HTTPS.
	if c.CAPath != "" {
		env = append(env, "GIT_SSL_CAINFO="+c.CAPath)
	}
	return env, nil
}

// shQuote single-quotes s for safe embedding in GIT_SSH_COMMAND, which git runs via
// the shell. Single quotes disable every shell metacharacter; an embedded single
// quote is closed, escaped, and reopened ('\”).
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
