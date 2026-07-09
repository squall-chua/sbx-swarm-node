# Git Provider host-side support — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the node half of the pluggable Git Provider so real clones and real publishes work against GitHub, GitLab, Gerrit, and plain remotes, driven by operator-registered named workspaces that hold a node-side credential + CA trust.

**Architecture:** Extend the existing config-declared git-backed workspace (`internal/git`) with a per-workspace vaulted credential (HTTPS token or SSH key) + CA trust, applied host-side to both git transport and REST. The node auto-manages the mirror base from `remote_url`. A new synchronous `PublishWork` RPC on `SandboxService` bundles the sandbox's source branch out of the live sandbox and dispatches to a new imperative `internal/gitprovider` package (one function per strategy). The credential never enters the sandbox and is never returned to the Agency, gossiped, or logged.

**Tech Stack:** Go, gRPC + grpc-gateway (buf codegen), `os/exec` git (no shell), `net/http` for provider REST, testify. Design spec: `docs/superpowers/specs/2026-07-09-git-provider-host-side-design.md`.

## Global Constraints

- Git runs shell-free via `os/exec` argv only (ADR-0003). Binaries restricted to the workspace exec allowlist (default `git`, `git-lfs`).
- The credential reaches git only via `cmd.Env` (`GIT_CONFIG_*`, `GIT_SSH_COMMAND`), **never** argv — so it never appears in `ps aux` or an argv-based error string.
- TLS verification is **never** disabled. SSH host key check is **never** `StrictHostKeyChecking=no`.
- No credential / token / SSH key / CA bytes may appear in `PublishResult`, emitted events, audit records, the Sandbox record, or logs.
- Source branch is never caller-supplied: live HEAD when it is a real branch, else the recorded branch (`rec.Spec.Branch`) on detached HEAD.
- Existing `PublishSandbox` (async, branch-only) and `doPublish` stay **unchanged**. `PublishWork` is additive.
- Proto codegen: edit `proto/sbxswarm/v1/sandbox.proto`, run `buf generate` (config in `buf.gen.yaml`); generated Go lands under `internal/gen/sbxswarm/v1/`.
- Every new RPC must be classified in `internal/apiserver/authz.go` or the drift-guard test `TestAuthz_AllMethodsClassified` fails.
- Match existing style; format only files you touch (repo is gofmt-dirty-but-unenforced). Verify with `go build ./... && go vet ./... && go test ./...`.

---

## Phase 1 (this plan) — registry, clone-by-name, branch + patch, mismatch, leak test

Zero external network; merges green on its own.

---

### Task 1: ADR-0019 + ADR-0020

**Files:**
- Create: `docs/adr/0019-provider-workspaces-hold-node-side-credential.md`
- Create: `docs/adr/0020-node-auto-manages-mirror-base.md`

**Interfaces:**
- Consumes: nothing.
- Produces: the recorded rationale later tasks reference in comments.

- [ ] **Step 1: Write ADR-0019** — follow the format of `docs/adr/0014-upstream-git-credentials-are-host-side.md`. Content: registered provider workspaces hold a per-workspace node-side credential (`token_env`/`ssh_key_path`) + optional `ca_path`, applied host-side to both git transport and REST. Supersedes ADR-0014 **for provider workspaces** (ambient host git config cannot feed a REST `Authorization` header). Never gossiped / in Sandbox state / returned to Agency / logged. Trade-off: per-node credential setup, same as 0014.

- [ ] **Step 2: Write ADR-0020** — node auto-manages the mirror base: `git clone --mirror <remote_url>` host-side with the vaulted credential + CA on first use into a node-managed dir, `fetch` thereafter; the sandbox mounts it read-only (ADR-0015 preserved). `host_path` optional for provider workspaces. Trade-off: the node now owns base creation (vs operator-prepared `host_path`).

- [ ] **Step 3: Add a header line to ADR-0014** noting it is superseded by ADR-0019 for provider workspaces (one line under its title; do not rewrite it).

- [ ] **Step 4: Commit**

```bash
git add docs/adr/0019-*.md docs/adr/0020-*.md docs/adr/0014-*.md
git commit -m "docs(adr): 0019 provider-workspace credentials, 0020 auto-managed mirror base"
```

---

### Task 2: Config surface — provider fields on GitConfig

**Files:**
- Modify: `internal/config/config.go:66-93` (the `GitConfig` struct + `WithDefaults`)
- Test: `internal/config/config_test.go` (add cases; create if absent)

**Interfaces:**
- Consumes: nothing.
- Produces: `config.GitConfig` gains string fields `RemoteURL`, `Provider`, `TokenEnv`, `SSHKeyPath`, `SSHKnownHostsPath`, `CAPath`. Existing fields (`Remote`, `DefaultBranch`, `AllowPush`, `PreSteps`, `PublishSteps`, `ExecAllowlist`) unchanged.

- [ ] **Step 1: Write the failing test** in `internal/config/config_test.go`:

```go
func TestGitConfig_ProviderFields(t *testing.T) {
	g := config.GitConfig{
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
```

- [ ] **Step 2: Run it, verify it fails** — `go test ./internal/config/ -run TestGitConfig_ProviderFields -v` → FAIL (unknown fields).

- [ ] **Step 3: Add the fields** to `GitConfig`:

```go
type GitConfig struct {
	Remote        string     `yaml:"remote"`
	DefaultBranch string     `yaml:"default_branch"`
	AllowPush     bool       `yaml:"allow_push"`
	PreSteps      [][]string `yaml:"pre_steps"`
	PublishSteps  [][]string `yaml:"publish_steps"`
	ExecAllowlist []string   `yaml:"exec_allowlist"`

	// Registered provider workspace (ADR-0019/0020). All optional; empty keeps
	// legacy ambient-credential behavior (ADR-0014).
	RemoteURL         string `yaml:"remote_url"`          // HTTPS or SSH upstream
	Provider          string `yaml:"provider"`            // github|gitlab|gerrit|plain; "" => derive
	TokenEnv          string `yaml:"token_env"`           // env var holding the HTTPS token
	SSHKeyPath        string `yaml:"ssh_key_path"`        // SSH private key path
	SSHKnownHostsPath string `yaml:"ssh_known_hosts_path"` // pins SSH host key; "" => accept-new
	CAPath            string `yaml:"ca_path"`             // internal-CA / self-signed PEM (HTTPS only)
}
```

`WithDefaults` needs no new defaulting (empty is meaningful). Leave its body unchanged.

- [ ] **Step 4: Run tests** — `go test ./internal/config/ -v` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): add provider fields to GitConfig (remote_url, provider, creds, ca)"
```

---

### Task 3: Credential + credential-env building (`internal/git`)

**Files:**
- Create: `internal/git/credential.go`
- Test: `internal/git/credential_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Credential struct { Token, SSHKeyPath, SSHKnownHostsPath, CAPath string }`
  - `func (c Credential) Env(remoteURL string) ([]string, error)` — returns env vars that inject the credential + trust for a git child process. Token via `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_n`/`GIT_CONFIG_VALUE_n` setting `http.<remoteURL>.extraheader`; SSH via `GIT_SSH_COMMAND`; CA via `GIT_SSL_CAINFO`. Always includes `GIT_TERMINAL_PROMPT=0`. Never puts the secret in argv.

- [ ] **Step 1: Write the failing test** in `internal/git/credential_test.go`:

```go
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
	require.Contains(t, joined, "-i /k/id")
	require.Contains(t, joined, "StrictHostKeyChecking=yes")
	require.Contains(t, joined, "UserKnownHostsFile=/k/known_hosts")
}

func TestCredential_Env_SSHKey_NoKnownHosts_AcceptNew(t *testing.T) {
	c := Credential{SSHKeyPath: "/k/id"}
	env, err := c.Env("ssh://git@host/repo")
	require.NoError(t, err)
	require.Contains(t, strings.Join(env, "\n"), "StrictHostKeyChecking=accept-new")
}
```

- [ ] **Step 2: Run it, verify it fails** — `go test ./internal/git/ -run TestCredential -v` → FAIL (undefined Credential).

- [ ] **Step 3: Implement** `internal/git/credential.go`:

```go
package git

import (
	"encoding/base64"
	"fmt"
	"strconv"
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

// avoid "imported and not used" if strconv drops out during edits
var _ = strconv.Itoa
```

(Remove the `strconv` stub if you don't reference it; it's a placeholder guard — delete before commit.)

- [ ] **Step 4: Run tests** — `go test ./internal/git/ -run TestCredential -v` → PASS. Then `go vet ./internal/git/`.

- [ ] **Step 5: Commit**

```bash
git add internal/git/credential.go internal/git/credential_test.go
git commit -m "feat(git): per-workspace Credential with env injection (token/ssh/ca, no argv leak)"
```

---

### Task 4: Provider derivation (`internal/gitprovider`)

**Files:**
- Create: `internal/gitprovider/provider.go`
- Test: `internal/gitprovider/provider_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Provider string` with consts `GitHub="github"`, `GitLab="gitlab"`, `Gerrit="gerrit"`, `Plain="plain"`.
  - `func Derive(remoteURL, explicit string) Provider` — explicit wins; else host `== github.com` → GitHub, host contains `gitlab` → GitLab, else Plain.
  - `func (p Provider) Supports(strategy string) bool` — plain: `branch`,`patch`; github: +`pull_request`; gitlab: +`merge_request`; gerrit: `branch`,`patch`,`gerrit_change`.

- [ ] **Step 1: Write the failing test**:

```go
package gitprovider

import "testing"

func TestDerive(t *testing.T) {
	cases := []struct{ url, explicit string; want Provider }{
		{"https://github.com/acme/app", "", GitHub},
		{"git@github.com:acme/app.git", "", GitHub},
		{"https://gitlab.com/acme/app", "", GitLab},
		{"https://gitlab.corp.internal/acme/app", "", GitLab},
		{"https://github.corp.internal/acme/app", "", Plain}, // enterprise not host-derivable
		{"ssh://git@gerrit.corp:29418/svc", "", Plain},        // gerrit never derived
		{"ssh://git@gerrit.corp:29418/svc", "gerrit", Gerrit}, // explicit wins
		{"https://github.com/acme/app", "gitlab", GitLab},     // explicit overrides host
	}
	for _, c := range cases {
		if got := Derive(c.url, c.explicit); got != c.want {
			t.Errorf("Derive(%q,%q)=%q want %q", c.url, c.explicit, got, c.want)
		}
	}
}

func TestSupports(t *testing.T) {
	if !Plain.Supports("branch") || !Plain.Supports("patch") {
		t.Fatal("plain must support branch+patch")
	}
	if Plain.Supports("pull_request") || Plain.Supports("gerrit_change") {
		t.Fatal("plain must reject pull_request/gerrit_change")
	}
	if !GitHub.Supports("pull_request") || GitHub.Supports("gerrit_change") {
		t.Fatal("github supports PR, not gerrit")
	}
	if !Gerrit.Supports("gerrit_change") || Gerrit.Supports("pull_request") {
		t.Fatal("gerrit supports gerrit_change, not PR")
	}
}
```

- [ ] **Step 2: Run it, verify it fails** — `go test ./internal/gitprovider/ -v` → FAIL (package/undefined).

- [ ] **Step 3: Implement** `internal/gitprovider/provider.go`:

```go
// Package gitprovider runs the host-side PublishWork strategies against a
// registered provider workspace's remote (ADR-0019). Derivation and strategy
// support are decided here; the git transport uses the workspace credential env.
package gitprovider

import (
	"net/url"
	"strings"
)

type Provider string

const (
	GitHub Provider = "github"
	GitLab Provider = "gitlab"
	Gerrit Provider = "gerrit"
	Plain  Provider = "plain"
)

// Derive resolves the provider. Explicit config always wins; otherwise only the
// two obvious public hosts are recognized, everything else is Plain (self-hosted
// GitLab/Gerrit require an explicit provider). See design Q5.
func Derive(remoteURL, explicit string) Provider {
	switch Provider(strings.ToLower(strings.TrimSpace(explicit))) {
	case GitHub, GitLab, Gerrit, Plain:
		return Provider(strings.ToLower(strings.TrimSpace(explicit)))
	}
	host := hostOf(remoteURL)
	switch {
	case host == "github.com":
		return GitHub
	case strings.Contains(host, "gitlab"):
		return GitLab
	default:
		return Plain
	}
}

// hostOf extracts the host from an HTTPS or scp-like SSH URL.
func hostOf(remote string) string {
	remote = strings.TrimSpace(remote)
	if i := strings.Index(remote, "://"); i >= 0 {
		if u, err := url.Parse(remote); err == nil {
			return strings.ToLower(u.Hostname())
		}
	}
	// scp-like: git@host:path
	if at := strings.Index(remote, "@"); at >= 0 {
		rest := remote[at+1:]
		if colon := strings.Index(rest, ":"); colon >= 0 {
			return strings.ToLower(rest[:colon])
		}
	}
	return ""
}

var strategySupport = map[Provider]map[string]bool{
	Plain:  {"branch": true, "patch": true},
	GitHub: {"branch": true, "patch": true, "pull_request": true},
	GitLab: {"branch": true, "patch": true, "merge_request": true},
	Gerrit: {"branch": true, "patch": true, "gerrit_change": true},
}

// Supports reports whether this provider can run the given publish strategy.
func (p Provider) Supports(strategy string) bool { return strategySupport[p][strategy] }
```

- [ ] **Step 4: Run tests** — `go test ./internal/gitprovider/ -v` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gitprovider/provider.go internal/gitprovider/provider_test.go
git commit -m "feat(gitprovider): provider derivation + per-provider strategy support"
```

---

### Task 5: Auto-managed mirror base + credential-backed clone-by-name

**Files:**
- Modify: `internal/git/workspace.go` (extend `Spec` + `Workspace`; add `EnsureBase`; thread `Credential` into the runner env)
- Modify: `internal/node/node.go:565-579` (`buildGitWorkspaces` — pass new fields + build `Credential` reading `token_env`)
- Test: `internal/git/workspace_test.go` (add `TestWorkspace_EnsureBase_ClonesMirror`)

**Interfaces:**
- Consumes: `git.Credential` (Task 3), `config.GitConfig` provider fields (Task 2).
- Produces:
  - `git.Spec` gains `RemoteURL string`, `Provider string`, `Cred Credential`.
  - `func (w *Workspace) EnsureBase(ctx context.Context) error` — if `spec.Base` is missing/empty and `RemoteURL != ""`, `git clone --mirror <RemoteURL> <Base>` with the credential env; else no-op. Idempotent, under the workspace lock.
  - `func (w *Workspace) RemoteURL() string`, `func (w *Workspace) Provider() string`, `func (w *Workspace) DefaultBranch() string`, `func (w *Workspace) Cred() Credential` — accessors PublishWork uses.
  - `gitEnv()` (existing) is replaced at call sites by `append(w.credEnv(remoteURL), gitEnv()...)` where `credEnv` = `spec.Cred.Env(spec.RemoteURL)`.

- [ ] **Step 1: Write the failing test** in `internal/git/workspace_test.go`:

```go
func TestWorkspace_EnsureBase_ClonesMirror(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	upstream := filepath.Join(root, "upstream.git")
	work := filepath.Join(root, "work")
	gitCmd(t, root, "init", "--bare", upstream)
	gitCmd(t, root, "clone", upstream, work)
	gitCmd(t, work, "-c", "user.email=a@b.c", "-c", "user.name=a", "commit", "--allow-empty", "-m", "init")
	gitCmd(t, work, "push", "origin", "HEAD:main")

	base := filepath.Join(root, "acme.git")
	w := New(Spec{
		Name: "acme", Base: base, RemoteURL: upstream, DefaultBranch: "main",
		Allowlist: []string{"git"},
	})
	require.NoError(t, w.EnsureBase(context.Background()))
	// base now exists as a mirror and carries refs/heads/main:
	out, err := exec.Command("git", "--git-dir", base, "rev-parse", "refs/heads/main").CombinedOutput()
	require.NoError(t, err, string(out))
}
```

- [ ] **Step 2: Run it, verify it fails** — `go test ./internal/git/ -run TestWorkspace_EnsureBase -v` → FAIL (undefined RemoteURL/EnsureBase).

- [ ] **Step 3: Extend `Spec` and add `EnsureBase` + accessors** in `internal/git/workspace.go`:

```go
type Spec struct {
	Name          string
	Base          string
	Remote        string
	RemoteURL     string     // upstream for auto-mirror + provider ops (ADR-0019/0020)
	Provider      string     // github|gitlab|gerrit|plain; "" => derive
	Cred          Credential // node-side credential + trust
	DefaultBranch string
	AllowPush     bool
	PreSteps      [][]string
	PublishSteps  [][]string
	Allowlist     []string
}

func (w *Workspace) RemoteURL() string     { return w.spec.RemoteURL }
func (w *Workspace) Provider() string      { return w.spec.Provider }
func (w *Workspace) DefaultBranch() string { return w.spec.DefaultBranch }
func (w *Workspace) Cred() Credential      { return w.spec.Cred }
func (w *Workspace) Base() string          { return w.spec.Base }

// credEnv returns the credential env for the workspace remote, plus the base
// GIT_TERMINAL_PROMPT guard. Errors only on a malformed credential.
func (w *Workspace) credEnv() ([]string, error) { return w.spec.Cred.Env(w.spec.RemoteURL) }

// EnsureBase creates the mirror base from RemoteURL on first use (ADR-0020). No-op
// if the base already has a git dir, or if RemoteURL is empty (legacy operator-
// prepared base). Runs under the workspace lock with the credential env.
func (w *Workspace) EnsureBase(ctx context.Context) error {
	if w.spec.RemoteURL == "" {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := os.Stat(filepath.Join(w.spec.Base, "HEAD")); err == nil {
		return nil // already a bare/mirror repo
	}
	env, err := w.credEnv()
	if err != nil {
		return err
	}
	_, err = w.runner.Run(ctx, filepath.Dir(w.spec.Base), env,
		[][]string{{"git", "clone", "--mirror", w.spec.RemoteURL, w.spec.Base}})
	return err
}
```

Add `"os"`, `"path/filepath"` imports. Also update `PreLock` and `Publish` to prepend `w.credEnv()` output ahead of `gitEnv()` so the PRE fetch / publish push authenticate with the vaulted credential when `RemoteURL != ""`. Keep `gitEnv()` as the fallback base env. Example for `PreLock`:

```go
func (w *Workspace) PreLock(ctx context.Context, branch string) (func(), error) {
	w.mu.Lock()
	env := gitEnv()
	if ce, err := w.credEnv(); err == nil {
		env = append(ce, env...)
	}
	vars := Vars{Branch: branch, Remote: w.spec.Remote, BaseRef: w.spec.DefaultBranch}
	argv, err := Build(w.spec.PreSteps, vars)
	if err != nil {
		w.mu.Unlock()
		return nil, err
	}
	if _, err := w.runner.Run(ctx, w.spec.Base, env, argv); err != nil {
		w.mu.Unlock()
		return nil, err
	}
	return w.mu.Unlock, nil
}
```

Apply the same `env` change to `Publish`.

- [ ] **Step 4: Wire `buildGitWorkspaces`** in `internal/node/node.go` to pass the new fields and build the credential (read `token_env` from the environment at boot):

```go
func buildGitWorkspaces(ws []config.WorkspaceConfig) map[string]*git.Workspace {
	out := map[string]*git.Workspace{}
	for _, w := range ws {
		if w.Git == nil {
			continue
		}
		g := w.Git.WithDefaults()
		var token string
		if g.TokenEnv != "" {
			token = os.Getenv(g.TokenEnv)
		}
		base := w.HostPath
		if base == "" && g.RemoteURL != "" {
			base = filepath.Join(gitWorkspaceDir(), w.Name+".git") // node-managed mirror base
		}
		out[w.Name] = git.New(git.Spec{
			Name: w.Name, Base: base, Remote: g.Remote, RemoteURL: g.RemoteURL, Provider: g.Provider,
			Cred: git.Credential{
				Token: token, SSHKeyPath: g.SSHKeyPath,
				SSHKnownHostsPath: g.SSHKnownHostsPath, CAPath: g.CAPath,
			},
			DefaultBranch: g.DefaultBranch, AllowPush: g.AllowPush,
			PreSteps: g.PreSteps, PublishSteps: g.PublishSteps, Allowlist: g.ExecAllowlist,
		})
	}
	return out
}

// gitWorkspaceDir is the node-managed root for auto-mirror bases (ADR-0020).
func gitWorkspaceDir() string {
	if d := os.Getenv("SBX_GIT_WORKSPACE_DIR"); d != "" {
		return d
	}
	return "/var/lib/sbx-swarm/git-workspaces"
}
```

Add `os`, `path/filepath` imports to `node.go` if not present. (If node.go already has a data-dir config, use that instead of the hardcoded path — check `cfg` for an existing data dir and prefer it; the env override stays.)

- [ ] **Step 5: Run tests** — `go test ./internal/git/ ./internal/node/ -v` → PASS. `go build ./...`.

- [ ] **Step 6: Commit**

```bash
git add internal/git/workspace.go internal/git/workspace_test.go internal/node/node.go
git commit -m "feat(git): auto-manage mirror base from remote_url + credential-backed pre/publish"
```

---

### Task 6: PublishWork RPC — proto, codegen, registration, authz

**Files:**
- Modify: `proto/sbxswarm/v1/sandbox.proto` (add RPC + messages)
- Regenerate: `internal/gen/sbxswarm/v1/*` via `buf generate`
- Modify: `internal/apiserver/authz.go` (classify the method as mutating/admin)
- Test: `internal/apiserver/authz_test.go` runs `TestAuthz_AllMethodsClassified` (already exists — must stay green)

**Interfaces:**
- Consumes: nothing.
- Produces: `sbxv1.PublishWorkRequest{Id, Strategy, Target, Title, Body}`, `sbxv1.PublishResult{Ref, DeliveryUrl, ChangeId, Patch}`, and `SandboxServiceServer.PublishWork(context.Context, *PublishWorkRequest) (*PublishResult, error)` in the generated interface.

- [ ] **Step 1: Add the RPC + messages** to `proto/sbxswarm/v1/sandbox.proto`. In `service SandboxService`, after `PublishSandbox`:

```proto
  rpc PublishWork(PublishWorkRequest) returns (PublishResult) {
    option (google.api.http) = {post: "/v1/sandboxes/{id}/git/publish-work" body: "*"};
  }
```

Near `PublishSandboxRequest`:

```proto
message PublishWorkRequest {
  string id = 1;       // sandbox id; source branch is the sandbox's own HEAD/recorded branch
  string strategy = 2; // branch|patch|pull_request|merge_request|gerrit_change
  string target = 3;   // branch: push dest; PR/MR: base branch; gerrit: refs/for/<target>
  string title = 4;    // PR/MR title
  string body = 5;     // PR/MR body
}

message PublishResult {
  string ref = 1;          // pushed ref (refs/heads/... or refs/for/...)
  string delivery_url = 2; // PR/MR/Change URL; empty for a plain branch push
  string change_id = 3;    // gerrit only
  bytes  patch = 4;        // patch strategy only
}
```

- [ ] **Step 2: Regenerate** — `buf generate` (from repo root; config `buf.gen.yaml`). Confirm `internal/gen/sbxswarm/v1/sandbox.pb.go` and `sandbox_grpc.pb.go` and `sandbox.pb.gw.go` now reference `PublishWork`. Build: `go build ./internal/gen/...`.

- [ ] **Step 3: Classify the method** in `internal/apiserver/authz.go` — add to `mutatingMethods`:

```go
	"/sbxswarm.v1.SandboxService/PublishWork":       true,
```

- [ ] **Step 4: Run the drift-guard + build** — `go test ./internal/apiserver/ -run TestAuthz_AllMethodsClassified -v` → PASS; `go build ./...` (the `UnimplementedSandboxServiceServer` provides a default `PublishWork` returning Unimplemented, so it compiles before the handler exists).

- [ ] **Step 5: Commit**

```bash
git add proto/sbxswarm/v1/sandbox.proto internal/gen/sbxswarm/v1/ internal/apiserver/authz.go
git commit -m "feat(proto): add PublishWork RPC + PublishResult; classify as admin-mutating"
```

---

### Task 7: PublishWork handler — source-branch resolution + branch strategy

**Files:**
- Create: `internal/gitprovider/publish.go` (strategy dispatch + `branch`)
- Create: `internal/apiserver/publish_work.go` (the RPC handler)
- Test: `internal/gitprovider/publish_test.go`, `internal/apiserver/publish_work_test.go`

**Interfaces:**
- Consumes: `git.Workspace` accessors (Task 5), `gitprovider.Derive/Supports` (Task 4), existing `SandboxService.bundleBranches`, `s.mgr.Get`, `s.gitTarget`, `s.agentHeadBranch`, `s.emit`, `s.auditPublish`.
- Produces:
  - `type Result struct { Ref, DeliveryURL, ChangeID string; Patch []byte }`
  - `type Env struct { Dir string; RunEnv []string; RemoteURL, Remote string; Cred git.Credential }` — the resolved per-publish context handed to a strategy (`Dir` = the base repo git dir; `RunEnv` = credential env; `Remote` = the base's configured upstream remote name).
  - `func Branch(ctx context.Context, r *git.Runner, e Env, source, target string) (Result, error)` — `git push <remote> <source>:<target||source>` in `e.Dir` with `e.RunEnv`. Returns `Ref: "refs/heads/"+dest`.
  - `func (s *SandboxService) PublishWork(ctx context.Context, r *sbxv1.PublishWorkRequest) (*sbxv1.PublishResult, error)`.

- [ ] **Step 1: Write the failing strategy test** `internal/gitprovider/publish_test.go`:

```go
package gitprovider

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/git"
	"github.com/stretchr/testify/require"
)

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	require.NoError(t, err, string(out))
}

func TestBranch_PushesToRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	upstream := filepath.Join(root, "up.git")
	base := filepath.Join(root, "base.git")
	work := filepath.Join(root, "work")
	gitCmd(t, root, "init", "--bare", upstream)
	gitCmd(t, root, "clone", upstream, work)
	gitCmd(t, work, "-c", "user.email=a@b.c", "-c", "user.name=a", "commit", "--allow-empty", "-m", "x")
	gitCmd(t, work, "push", "origin", "HEAD:main")
	gitCmd(t, root, "clone", "--mirror", upstream, base) // base mirrors upstream; origin=upstream

	r := git.NewRunner([]string{"git"})
	res, err := Branch(context.Background(), r, Env{Dir: base, Remote: "origin"}, "main", "feature-x")
	require.NoError(t, err)
	require.Equal(t, "refs/heads/feature-x", res.Ref)
	require.Empty(t, res.DeliveryURL)
	// upstream now has feature-x:
	out, err := exec.Command("git", "--git-dir", upstream, "rev-parse", "refs/heads/feature-x").CombinedOutput()
	require.NoError(t, err, string(out))
}
```

- [ ] **Step 2: Run it, verify it fails** — `go test ./internal/gitprovider/ -run TestBranch -v` → FAIL (undefined Branch/Env/Result).

- [ ] **Step 3: Implement** `internal/gitprovider/publish.go`:

```go
package gitprovider

import (
	"context"
	"fmt"

	"github.com/squall-chua/sbx-swarm-node/internal/git"
)

// Result is a strategy outcome mapped 1:1 to the PublishResult proto.
type Result struct {
	Ref         string
	DeliveryURL string
	ChangeID    string
	Patch       []byte
}

// Env is the resolved per-publish context handed to a strategy: the base git dir,
// the credential env for the git child, the upstream remote name/URL, and the
// credential (for REST in P2). Never logged.
type Env struct {
	Dir       string
	RunEnv    []string
	Remote    string // configured upstream remote name in the base (e.g. "origin")
	RemoteURL string
	Cred      git.Credential
}

// Branch pushes source to <target||source> on the base's upstream remote.
func Branch(ctx context.Context, r *git.Runner, e Env, source, target string) (Result, error) {
	if source == "" {
		return Result{}, fmt.Errorf("empty source branch")
	}
	dest := target
	if dest == "" {
		dest = source
	}
	remote := e.Remote
	if remote == "" {
		remote = "origin"
	}
	if _, err := r.Run(ctx, e.Dir, e.RunEnv, [][]string{{"git", "push", remote, source + ":" + dest}}); err != nil {
		return Result{}, err
	}
	return Result{Ref: "refs/heads/" + dest}, nil
}
```

- [ ] **Step 4: Run the strategy test** — `go test ./internal/gitprovider/ -run TestBranch -v` → PASS.

- [ ] **Step 5: Write the handler test** `internal/apiserver/publish_work_test.go` — model it on `provision_git_test.go`/`sandboxservice` tests: a `Fake` backend whose `Exec` answers `git rev-parse --abbrev-ref HEAD` with a branch and `git bundle create` by writing a real bundle, a workspace whose base is a local mirror of a local bare "upstream", then call `PublishWork` with strategy `branch` and assert the upstream got the branch and `PublishResult.Ref` is set, `DeliveryUrl` empty. (Reuse the `Fake` from `internal/sandbox/fake.go`; see how existing publish tests stand up a `SandboxService` + `gitWS`.)

```go
func TestPublishWork_Branch(t *testing.T) {
	// ... stand up SandboxService s with a Fake backend + one git.Workspace "acme"
	// whose Base is a mirror of a local bare upstream, AllowPush=true.
	// Record a clone-mode sandbox rec with Spec.Branch="main".
	res, err := s.PublishWork(ctx, &sbxv1.PublishWorkRequest{Id: id, Strategy: "branch", Target: "feature-x"})
	require.NoError(t, err)
	require.Equal(t, "refs/heads/feature-x", res.Ref)
	require.Empty(t, res.DeliveryUrl)
}
```

- [ ] **Step 6: Run it, verify it fails** — `go test ./internal/apiserver/ -run TestPublishWork_Branch -v` → FAIL (PublishWork returns Unimplemented).

- [ ] **Step 7: Implement the handler** `internal/apiserver/publish_work.go`:

```go
package apiserver

import (
	"context"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/git"
	"github.com/squall-chua/sbx-swarm-node/internal/gitprovider"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// PublishWork synchronously publishes the sandbox's source branch via the chosen
// strategy against its registered provider workspace (ADR-0019). Source branch is
// the sandbox's own HEAD (recorded branch on detached HEAD), never caller-supplied.
func (s *SandboxService) PublishWork(ctx context.Context, r *sbxv1.PublishWorkRequest) (*sbxv1.PublishResult, error) {
	rec, ws, err := s.gitTarget(ctx, r.Id)
	if err != nil {
		return nil, err
	}
	prov := gitprovider.Derive(ws.RemoteURL(), ws.Provider())
	if !prov.Supports(r.Strategy) {
		return nil, status.Errorf(codes.InvalidArgument, "%s on %s: unsupported (set provider explicitly if self-hosted)", r.Strategy, prov)
	}
	if r.Strategy != "patch" && !ws.AllowPush() {
		return nil, status.Error(codes.FailedPrecondition, "workspace does not allow push")
	}

	to := s.publishTimeout
	if to <= 0 {
		to = defaultPublishTimeout
	}
	pubCtx, cancel := context.WithTimeout(ctx, to)
	defer cancel()

	source, err := s.sourceBranch(pubCtx, rec)
	if err != nil {
		return nil, err
	}

	// Bundle the source branch out of the LIVE sandbox into the base under lock,
	// then run the strategy from the base.
	bundlePath, cleanup, err := s.bundleBranches(pubCtx, rec.BackendName, []string{source})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "publish-work bundle: %v", err)
	}
	defer cleanup()

	runEnv, _ := ws.Cred().Env(ws.RemoteURL())
	if err := ws.FetchFromBundle(pubCtx, source, bundlePath); err != nil { // adds source to the base
		return nil, status.Errorf(codes.Internal, "publish-work fetch: %v", err)
	}
	env := gitprovider.Env{Dir: ws.Base(), RunEnv: runEnv, Remote: ws.RemoteName(), RemoteURL: ws.RemoteURL(), Cred: ws.Cred()}
	runner := git.NewRunner([]string{"git"})

	var res gitprovider.Result
	switch r.Strategy {
	case "branch":
		res, err = gitprovider.Branch(pubCtx, runner, env, source, r.Target)
	case "patch":
		res, err = gitprovider.Patch(pubCtx, runner, env, source, r.Target)
	default:
		return nil, status.Errorf(codes.Unimplemented, "strategy %q not yet implemented", r.Strategy)
	}
	actor := principalFromContext(ctx).userRole
	if actor == "" {
		actor = "system"
	}
	s.auditPublish(ws.Name(), source, actor, err)
	if err != nil {
		s.emit("sandbox.publish_failed", r.Id, map[string]string{"branch": source, "strategy": r.Strategy})
		return nil, status.Errorf(codes.Internal, "publish-work: %v", err)
	}
	s.emit("sandbox.published", r.Id, map[string]string{"branch": source, "strategy": r.Strategy})
	return &sbxv1.PublishResult{Ref: res.Ref, DeliveryUrl: res.DeliveryURL, ChangeId: res.ChangeID, Patch: res.Patch}, nil
}

// sourceBranch resolves the publish source: live HEAD when it is a real branch,
// else the recorded branch (detached-HEAD fallback). Never caller-supplied.
func (s *SandboxService) sourceBranch(ctx context.Context, rec *sandbox.Record) (string, error) {
	if b, err := s.agentHeadBranch(ctx, rec.BackendName); err == nil && b != "" {
		return b, nil
	}
	if rec.Spec.Branch != "" {
		return rec.Spec.Branch, nil
	}
	return "", status.Error(codes.FailedPrecondition, "no source branch: detached HEAD and no recorded branch")
}
```

Add the small helper `FetchFromBundle`, `RemoteName` accessor to `internal/git/workspace.go`:

```go
func (w *Workspace) RemoteName() string { if w.spec.Remote != "" { return w.spec.Remote }; return "origin" }

// FetchFromBundle fetches branch from a git bundle file into the base, under the
// workspace lock, so a strategy can push it. Reuses the credential env for parity.
func (w *Workspace) FetchFromBundle(ctx context.Context, branch, bundlePath string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	env := gitEnv()
	if ce, err := w.credEnv(); err == nil {
		env = append(ce, env...)
	}
	_, err := w.runner.Run(ctx, w.spec.Base, env,
		[][]string{{"git", "fetch", bundlePath, "+refs/heads/" + branch + ":refs/heads/" + branch}})
	return err
}
```

Note: `Patch` is defined in Task 8; for this task the `case "patch"` line references it, so implement Task 8's `Patch` signature stub first OR reorder to add `Patch` here returning `Unimplemented` and flesh it out in Task 8. Simplest: add a minimal `Patch` stub in `publish.go` now (returns `Result{}, fmt.Errorf("todo")`) so this compiles, and Task 8 replaces the body test-first.

- [ ] **Step 8: Run the handler test** — `go test ./internal/apiserver/ -run TestPublishWork_Branch -v` → PASS. `go build ./... && go vet ./internal/apiserver/ ./internal/gitprovider/`.

- [ ] **Step 9: Commit**

```bash
git add internal/gitprovider/publish.go internal/gitprovider/publish_test.go internal/apiserver/publish_work.go internal/apiserver/publish_work_test.go internal/git/workspace.go
git commit -m "feat(publish-work): PublishWork handler + source-branch resolution + branch strategy"
```

---

### Task 8: patch strategy

**Files:**
- Modify: `internal/gitprovider/publish.go` (replace the `Patch` stub)
- Test: `internal/gitprovider/publish_test.go` (add `TestPatch_ReturnsDiffBytes`)

**Interfaces:**
- Consumes: `Env`, `Result` (Task 7).
- Produces: `func Patch(ctx context.Context, r *git.Runner, e Env, source, target string) (Result, error)` — `git format-patch <base>..<source> --stdout` where base = `target` if set else the merge-base against the default upstream branch; returns bytes in `Result.Patch`, no remote write.

- [ ] **Step 1: Write the failing test**:

```go
func TestPatch_ReturnsDiffBytes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "r")
	gitCmd(t, root, "init", repo)
	gitCmd(t, repo, "-c", "user.email=a@b.c", "-c", "user.name=a", "commit", "--allow-empty", "-m", "base")
	gitCmd(t, repo, "branch", "-M", "main")
	gitCmd(t, repo, "checkout", "-b", "work")
	// a real change so format-patch emits a patch:
	require.NoError(t, os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hi"), 0o644))
	gitCmd(t, repo, "add", "f.txt")
	gitCmd(t, repo, "-c", "user.email=a@b.c", "-c", "user.name=a", "commit", "-m", "add f")

	r := git.NewRunner([]string{"git"})
	res, err := Patch(context.Background(), r, Env{Dir: repo}, "work", "main")
	require.NoError(t, err)
	require.Contains(t, string(res.Patch), "add f")
	require.Empty(t, res.Ref)
}
```

- [ ] **Step 2: Run it, verify it fails** — `go test ./internal/gitprovider/ -run TestPatch -v` → FAIL (stub returns error).

- [ ] **Step 3: Implement `Patch`** (replace the stub):

```go
// Patch returns format-patch bytes for target..source (no remote write). If
// target is empty it falls back to <source>~1..<source> (last commit).
func Patch(ctx context.Context, r *git.Runner, e Env, source, target string) (Result, error) {
	rng := source + "~1.." + source
	if target != "" {
		rng = target + ".." + source
	}
	results, err := r.Run(ctx, e.Dir, e.RunEnv, [][]string{{"git", "format-patch", rng, "--stdout"}})
	if err != nil {
		return Result{}, err
	}
	return Result{Patch: results[len(results)-1].Output}, nil
}
```

- [ ] **Step 4: Run tests** — `go test ./internal/gitprovider/ -v` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gitprovider/publish.go internal/gitprovider/publish_test.go
git commit -m "feat(publish-work): patch strategy (format-patch bytes, no remote write)"
```

---

### Task 9: Provider-mismatch rejection (handler-level test)

**Files:**
- Test: `internal/apiserver/publish_work_test.go` (add rejection cases)

**Interfaces:**
- Consumes: the handler + `gitprovider.Supports` (already built). No new production code — the `Supports` gate in Task 7 must reject correctly.

- [ ] **Step 1: Write the failing/guard test** — a github workspace rejecting `gerrit_change`, and a plain workspace rejecting `pull_request`:

```go
func TestPublishWork_ProviderMismatch(t *testing.T) {
	// github workspace (RemoteURL github.com or Provider "github"):
	_, err := s.PublishWork(ctx, &sbxv1.PublishWorkRequest{Id: ghID, Strategy: "gerrit_change", Target: "main"})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "unsupported")

	// plain workspace rejects pull_request:
	_, err = s.PublishWork(ctx, &sbxv1.PublishWorkRequest{Id: plainID, Strategy: "pull_request", Target: "main"})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}
```

- [ ] **Step 2: Run it** — `go test ./internal/apiserver/ -run TestPublishWork_ProviderMismatch -v`. It should PASS immediately (Task 7's gate). If it fails, fix the gate — do not weaken the test.

- [ ] **Step 3: Commit**

```bash
git add internal/apiserver/publish_work_test.go
git commit -m "test(publish-work): provider-mismatch rejection (gerrit on github, PR on plain)"
```

---

### Task 10: Credential/trust leak test (the security bar)

**Files:**
- Test: `internal/apiserver/publish_work_leak_test.go`

**Interfaces:**
- Consumes: the full PublishWork path + a capturing slog handler + captured events/audit sinks.

- [ ] **Step 1: Write the leak test** — register the workspace with sentinel credential values, run clone + `branch` + `patch` + one forced failure, and assert the sentinel appears in none of the five surfaces:

```go
func TestPublishWork_NoCredentialLeak(t *testing.T) {
	const tok = "SENTINEL-TOKEN-9f3a2b"
	// stand up SandboxService with:
	//  - a git.Workspace whose Cred.Token = tok, CAPath points at a sentinel PEM
	//  - an events sink capturing (type, id, payload)
	//  - an audit log capturing entries
	//  - slog routed to a bytes.Buffer
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))

	// success paths:
	rb, err := s.PublishWork(ctx, &sbxv1.PublishWorkRequest{Id: id, Strategy: "branch", Target: "x"})
	require.NoError(t, err)
	rp, err := s.PublishWork(ctx, &sbxv1.PublishWorkRequest{Id: id, Strategy: "patch", Target: "main"})
	require.NoError(t, err)
	// forced failure (bad target ref triggers a git error whose stderr we surface):
	_, ferr := s.PublishWork(ctx, &sbxv1.PublishWorkRequest{Id: id, Strategy: "branch", Target: "refs/heads/../evil"})
	require.Error(t, ferr)

	surfaces := []string{
		rb.String(), rp.String(), ferr.Error(), logs.String(),
		fmt.Sprint(capturedEvents), fmt.Sprint(capturedAudit),
		fmt.Sprint(mustGetRecord(t, s, id)), // persisted sandbox record
		string(rp.Patch), string(rb.Ref),
	}
	for _, sfc := range surfaces {
		require.NotContains(t, sfc, tok, "credential leaked into an outward surface")
	}
}
```

(Adapt the sink/record capture to the existing test helpers used by `observe_test.go` / `audit_test.go`. `rb.String()`/`rp.String()` use the proto `String()` method to cover all fields.)

- [ ] **Step 2: Run it, verify it passes** — `go test ./internal/apiserver/ -run TestPublishWork_NoCredentialLeak -v` → PASS. If the sentinel leaks (e.g. into a git error string), that is a real bug: ensure error wrapping never includes `e.RunEnv` and that git stderr does not echo the extraheader (it should not, since the token is in env, not argv).

- [ ] **Step 3: Full P1 verification**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/apiserver/publish_work_leak_test.go
git commit -m "test(publish-work): assert credential/trust never leak into any outward surface"
```

---

## Phase 2 (separate plan, after P1 merges) — PR / MR / Gerrit REST

Expand these into a full task-by-task plan (own `writing-plans` pass) once P1 is green and merged. Scope outline:

- **REST client (`internal/gitprovider/rest.go`)** — `func httpClient(cred git.Credential) (*http.Client, error)` building `tls.Config.RootCAs` from `cred.CAPath` (nil → system trust) + a helper attaching the `Authorization` header. REST base derivation from host (GitHub `api.github.com` vs `/api/v3`; GitLab `/api/v4`). Owner/repo parse from `remote_url` path.
- **pull_request (GitHub)** — push source first, then find an **open** PR by `head=owner:source & base=target`; PATCH title/body if found, else POST create; `target` required. Return `html_url` in `delivery_url`. Test: httptest GitHub fake asserting create-then-update (second call no duplicate).
- **merge_request (GitLab)** — same via `/api/v4/projects/:id/merge_requests` (list by `source_branch`+`target_branch`+`state=opened`). Test: httptest GitLab fake, update-in-place.
- **gerrit_change** — reuse existing trailer else amend deterministic `Change-Id: I<sha1hex(workspace \0 sandbox \0 source)>` in a temp worktree off the base; `git push HEAD:refs/for/<target>`; parse Change URL from push stderr; `change_id` = the trailer. Test: fake gerrit as a bare repo accepting `refs/for/*` + a stderr-emitting `receive-pack` hook; assert re-publish keeps one Change (same trailer).
- **TLS / SSH matrix tests** — clone-by-name over HTTPS + SSH incl. a generated self-signed cert exercised via `ca_path`.
- **Env-gated real smoke** — `//go:build integration` file hitting real GitHub/GitLab/Gerrit behind env tokens; skipped in CI.

---

## Self-Review

**Spec coverage (P1):** registered workspace config (T2) ✓; vaulted credential + env injection (T3) ✓; provider derivation + mismatch (T4, T9) ✓; auto-managed mirror base + clone-by-name with vaulted creds (T5) ✓; PublishWork proto/authz/handler (T6, T7) ✓; source-branch live-HEAD/recorded (T7) ✓; branch (T7) + patch (T8) ✓; leak test across 5 surfaces + forced failure (T10) ✓; ADR-0019/0020 (T1) ✓. PR/MR/gerrit/REST + TLS-SSH matrix + real smoke → Phase 2 (deferred by design).

**Placeholder scan:** the only intentional stub is `Patch` in T7 (compilation guard), explicitly replaced test-first in T8; the `strconv` guard line in T3 is flagged for deletion. No TBDs in production steps.

**Type consistency:** `git.Credential.Env(remoteURL)` (T3) is consumed identically in T5/T7; `gitprovider.Env{Dir,RunEnv,Remote,RemoteURL,Cred}` and `Result{Ref,DeliveryURL,ChangeID,Patch}` are defined in T7 and reused verbatim in T8/P2; workspace accessors `RemoteURL()/Provider()/Cred()/Base()/RemoteName()/DefaultBranch()/FetchFromBundle()` are defined in T5/T7 and used by the T7 handler; proto `PublishResult{Ref,DeliveryUrl,ChangeId,Patch}` maps 1:1 to `gitprovider.Result`.
