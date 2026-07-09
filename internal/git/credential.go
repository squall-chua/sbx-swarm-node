package git

import (
	"encoding/base64"
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

	// SSH key + host-key policy.
	if c.SSHKeyPath != "" {
		ssh := "ssh -i " + c.SSHKeyPath + " -o IdentitiesOnly=yes"
		if c.SSHKnownHostsPath != "" {
			ssh += " -o StrictHostKeyChecking=yes -o UserKnownHostsFile=" + c.SSHKnownHostsPath
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
