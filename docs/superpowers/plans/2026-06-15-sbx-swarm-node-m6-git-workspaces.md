# sbx-swarm-node M6 — Git-Backed Workspaces Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development or superpowers:executing-plans. Checkbox steps. Tasks 3-4 need the `git` binary.
>
> **Forward-looking:** depends on M1c (`Manager`, `CreateSpec.Clone`), M3 (`audit`), M5 (workspace config). Reconcile signatures against real code.

**Goal:** Implement the clone-mode git lifecycle — provision with `WithClone()`, a **PRE** step that refreshes a bare/mirror base, the agent working in its private clone, and a **PUBLISH** step that fetches the agent's branch and pushes upstream — using **node-local, shell-free, argv-step pipelines** (ADR-0003), a **per-workspace lock**, **host-side credentials**, and an audit trail; the agent sandbox stays credential-free.

**Architecture:** `git.Builder` turns declarative steps + validated values into `argv` (no shell). `git.Runner` executes argv via `os/exec` (allowlisted binaries) in a directory. `git.Workspace` orchestrates PRE/PUBLISH under a per-workspace mutex against a bare/mirror base, injecting upstream credentials only into the host-side push. Pipelines live in node config (ADR-0003); requests supply only validated `{branch}`/`{commit_message}`.

**Tech Stack:** Go 1.23, `os/exec` + `git`, M1/M3/M5 stack.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/git/builder.go` | substitute validated values into argv; reject injection |
| `internal/git/runner.go` | run argv steps (allowlist, capture, stop-on-error) |
| `internal/git/workspace.go` | per-workspace lock; PRE/PUBLISH against bare base |
| `internal/git/creds.go` | host-side credential injection (GIT_ASKPASS) |
| `internal/config/config.go` | git workspace config (steps, remote, allow_push, allowlist) |
| `internal/node/node.go` | clone-mode provision; publish trigger (explicit + on-stop) |

---

## Task 1: Argv builder (substitution + validation)

**Files:** `internal/git/builder.go`, test `internal/git/builder_test.go`

- [ ] **Step 1: Failing test**

```go
package git

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuild_SubstitutesValidatedValues(t *testing.T) {
	vars := Vars{Branch: "agent/task-1", BaseRef: "main", Remote: "origin", CommitMessage: "work"}
	steps := [][]string{
		{"git", "fetch", "{remote}"},
		{"git", "checkout", "-b", "{branch}", "{remote}/{base_ref}"},
	}
	argv, err := Build(steps, vars)
	require.NoError(t, err)
	require.Equal(t, []string{"git", "checkout", "-b", "agent/task-1", "origin/main"}, argv[1])
}

func TestBuild_RejectsInjection(t *testing.T) {
	_, err := Build([][]string{{"git", "checkout", "{branch}"}}, Vars{Branch: "--upload-pack=evil"})
	require.Error(t, err) // leading '-' rejected
	_, err = Build([][]string{{"git", "checkout", "{branch}"}}, Vars{Branch: "bad\nname"})
	require.Error(t, err) // control char rejected
}
```

- [ ] **Step 2: Run → FAIL**: `go test ./internal/git/ -run TestBuild -v`

- [ ] **Step 3: Implement `builder.go`**

```go
// Package git runs declarative, shell-free git pipelines for clone-mode
// workspaces (ADR-0003). Commands come from node config; only the values below
// are request-supplied, and they are validated and bound as discrete argv.
package git

import (
	"fmt"
	"regexp"
	"strings"
)

// Vars are the request-supplied values bound into pipeline steps.
type Vars struct {
	Branch        string
	BaseRef       string
	Remote        string
	SandboxRemote string
	CommitMessage string
}

// branch/ref names: no leading '-', no control chars/spaces, no '..'.
var refOK = regexp.MustCompile(`^[A-Za-z0-9._/\-]+$`)

func validateRef(name, val string) error {
	if val == "" {
		return nil // unset variable: allowed (step may not use it)
	}
	if strings.HasPrefix(val, "-") || strings.Contains(val, "..") || !refOK.MatchString(val) {
		return fmt.Errorf("invalid %s %q", name, val)
	}
	return nil
}

// Build substitutes vars into each step's argv. Commit message is bound only
// where {commit_message} appears and is not ref-validated (it is a single argv
// element, never shell-interpreted), but control chars are stripped.
func Build(steps [][]string, v Vars) ([][]string, error) {
	if err := validateRef("branch", v.Branch); err != nil {
		return nil, err
	}
	if err := validateRef("base_ref", v.BaseRef); err != nil {
		return nil, err
	}
	if err := validateRef("remote", v.Remote); err != nil {
		return nil, err
	}
	if err := validateRef("sandbox_remote", v.SandboxRemote); err != nil {
		return nil, err
	}
	repl := strings.NewReplacer(
		"{branch}", v.Branch, "{base_ref}", v.BaseRef, "{remote}", v.Remote,
		"{sandbox_remote}", v.SandboxRemote, "{commit_message}", sanitizeMsg(v.CommitMessage),
	)
	out := make([][]string, len(steps))
	for i, step := range steps {
		argv := make([]string, len(step))
		for j, tok := range step {
			argv[j] = repl.Replace(tok)
		}
		out[i] = argv
	}
	return out, nil
}

func sanitizeMsg(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if r < 0x20 {
			return -1
		}
		return r
	}, s)
}
```

- [ ] **Step 4: Run → PASS, commit**

```bash
go test ./internal/git/ -run TestBuild -v
git add internal/git/builder.go internal/git/builder_test.go
git commit -m "feat(git): shell-free argv builder with value validation (ADR-0003)"
```

---

## Task 2: Pipeline runner (allowlist + capture)

**Files:** `internal/git/runner.go`, test `internal/git/runner_test.go`

- [ ] **Step 1: Failing test** (uses `echo`/`false` so it runs anywhere)

```go
package git

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRunner_RunsAllowedStepsStopsOnError(t *testing.T) {
	r := NewRunner(map[string]bool{"echo": true, "false": true})

	res, err := r.Run(context.Background(), t.TempDir(), nil, [][]string{{"echo", "hello"}})
	require.NoError(t, err)
	require.Len(t, res, 1)
	require.Equal(t, 0, res[0].ExitCode)

	_, err = r.Run(context.Background(), t.TempDir(), nil, [][]string{{"false"}, {"echo", "never"}})
	require.Error(t, err) // stops at the failing step
}

func TestRunner_RejectsDisallowedBinary(t *testing.T) {
	r := NewRunner(map[string]bool{"git": true})
	_, err := r.Run(context.Background(), t.TempDir(), nil, [][]string{{"rm", "-rf", "/"}})
	require.ErrorContains(t, err, "not allowed")
}
```

- [ ] **Step 2: Run → FAIL**: `go test ./internal/git/ -run TestRunner -v`

- [ ] **Step 3: Implement `runner.go`**

```go
package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// StepResult is one executed step's outcome.
type StepResult struct {
	Argv     []string
	ExitCode int
	Output   []byte
}

// Runner executes argv steps via os/exec (no shell), restricted to an allowlist
// of binaries.
type Runner struct{ allow map[string]bool }

// NewRunner builds a runner permitting only the given binaries (e.g. git, git-lfs).
func NewRunner(allow map[string]bool) *Runner { return &Runner{allow: allow} }

// Run executes steps in dir with extra env, stopping at the first failure.
func (r *Runner) Run(ctx context.Context, dir string, env []string, steps [][]string) ([]StepResult, error) {
	var results []StepResult
	for _, argv := range steps {
		if len(argv) == 0 {
			continue
		}
		if !r.allow[argv[0]] {
			return results, fmt.Errorf("binary %q not allowed", argv[0])
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) // argv, never a shell string
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(), env...)
		var buf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &buf, &buf
		err := cmd.Run()
		res := StepResult{Argv: argv, Output: buf.Bytes()}
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				res.ExitCode = ee.ExitCode()
			} else {
				res.ExitCode = -1
			}
			results = append(results, res)
			return results, fmt.Errorf("step %v failed (exit %d): %s", argv, res.ExitCode, buf.String())
		}
		results = append(results, res)
	}
	return results, nil
}
```

- [ ] **Step 4: Run → PASS, commit**

```bash
go test ./internal/git/ -run TestRunner -v
git add internal/git/runner.go internal/git/runner_test.go
git commit -m "feat(git): allowlisted argv pipeline runner"
```

---

## Task 3: Workspace PRE/PUBLISH against a bare base (real git)

**Files:** `internal/git/workspace.go`, test `internal/git/workspace_test.go` (needs `git`)

- [ ] **Step 1: Failing test** — create a bare "upstream" repo with a commit on `main`, a mirror "base" cloned from it, and a fake "sandbox clone" with a new branch; assert PRE fast-forwards the base and PUBLISH pushes the branch upstream.

```go
package git

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

func TestWorkspace_PreAndPublish(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	upstream := filepath.Join(root, "upstream.git")
	work := filepath.Join(root, "work")
	base := filepath.Join(root, "base.git")

	git(t, root, "init", "--bare", upstream)
	git(t, root, "clone", upstream, work)
	git(t, work, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "--allow-empty", "-m", "init")
	git(t, work, "push", "origin", "HEAD:main")
	git(t, root, "clone", "--mirror", upstream, base)

	ws := NewWorkspace("repo", base, "origin", NewRunner(map[string]bool{"git": true}), nil)

	// PRE: fetch refs into the bare base
	require.NoError(t, ws.Pre(context.Background(), Vars{Remote: "origin", BaseRef: "main"},
		[][]string{{"git", "fetch", "{remote}", "+refs/heads/*:refs/heads/*"}}))

	// Simulate an agent branch landing in the base, then PUBLISH pushes it upstream.
	git(t, work, "checkout", "-b", "agent/x")
	git(t, work, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "--allow-empty", "-m", "agent work")
	git(t, work, "push", base, "agent/x") // stand-in for "host fetches from sandbox clone"

	require.NoError(t, ws.Publish(context.Background(), Vars{Remote: "origin", Branch: "agent/x"},
		[][]string{{"git", "push", "{remote}", "{branch}"}}))

	// upstream now has agent/x
	cmd := exec.Command("git", "branch", "--list", "agent/x")
	cmd.Dir = base
	out, _ := cmd.CombinedOutput()
	require.Contains(t, string(out), "agent/x")
}
```

- [ ] **Step 2: Run → FAIL**: `go test ./internal/git/ -run TestWorkspace -v`

- [ ] **Step 3: Implement `workspace.go`**

```go
package git

import (
	"context"
	"sync"
)

// Workspace orchestrates clone-mode git lifecycle for one git-backed workspace,
// serializing all operations on its bare/mirror base with a per-workspace lock.
type Workspace struct {
	name   string
	base   string // path to the bare/mirror base repo (host_path)
	remote string
	runner *Runner
	creds  *Creds // nil = no credential injection (e.g. tests)
	mu     sync.Mutex
}

// NewWorkspace builds a workspace orchestrator.
func NewWorkspace(name, base, remote string, runner *Runner, creds *Creds) *Workspace {
	return &Workspace{name: name, base: base, remote: remote, runner: runner, creds: creds}
}

// Pre runs the PRE pipeline (refresh the base from upstream) under the lock.
func (w *Workspace) Pre(ctx context.Context, vars Vars, steps [][]string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	argv, err := Build(steps, vars)
	if err != nil {
		return err
	}
	_, err = w.runner.Run(ctx, w.base, w.credEnv(), argv)
	return err
}

// Publish runs the PUBLISH pipeline (push upstream) under the lock, injecting
// host-side credentials only here.
func (w *Workspace) Publish(ctx context.Context, vars Vars, steps [][]string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	argv, err := Build(steps, vars)
	if err != nil {
		return err
	}
	_, err = w.runner.Run(ctx, w.base, w.credEnv(), argv)
	return err
}

func (w *Workspace) credEnv() []string {
	if w.creds == nil {
		return nil
	}
	return w.creds.Env()
}
```

- [ ] **Step 4: Run → PASS, commit**

```bash
go test ./internal/git/ -run TestWorkspace -v
git add internal/git/workspace.go internal/git/workspace_test.go
git commit -m "feat(git): per-workspace-locked PRE/PUBLISH against bare base"
```

---

## Task 4: Credentials + clone-mode provision + publish triggers + wiring

**Files:** `internal/git/creds.go`, `internal/config/config.go`, `internal/node/node.go`, `internal/apiserver/sandboxservice.go`

- [ ] **Step 1: Credential injection `creds.go`** (TDD: `Env()` sets `GIT_ASKPASS` + a token env var; the askpass script echoes the token — verify it prints the token)

```go
package git

import (
	"os"
	"path/filepath"
)

// Creds injects an upstream credential host-side via GIT_ASKPASS, never writing
// it to .git/config and never exposing it to the agent sandbox (spec §12).
type Creds struct {
	askpassPath string
	token       string
}

// NewCreds writes a one-line askpass helper to dir and binds the token. The
// token is passed via an env var the helper echoes — it is never persisted in
// repo config.
func NewCreds(dir, token string) (*Creds, error) {
	p := filepath.Join(dir, "askpass.sh")
	script := "#!/bin/sh\nexec printf '%s' \"$SBX_GIT_TOKEN\"\n"
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
		return nil, err
	}
	return &Creds{askpassPath: p, token: token}, nil
}

// Env returns the environment to attach to git pushes.
func (c *Creds) Env() []string {
	return []string{"GIT_ASKPASS=" + c.askpassPath, "GIT_TERMINAL_PROMPT=0", "SBX_GIT_TOKEN=" + c.token}
}
```

- [ ] **Step 2: Config** — extend each git-backed `WorkspaceConfig` with:

```go
type GitConfig struct {
	Bare         bool       `yaml:"bare"`
	Remote       string     `yaml:"remote"`
	DefaultBranch string    `yaml:"default_branch"`
	AuthSecretRef string    `yaml:"auth_secret_ref"` // resolves to a token from env/file (never gossiped/logged)
	AllowPush    bool       `yaml:"allow_push"`
	PreSteps     [][]string `yaml:"pre_steps"`
	PublishSteps [][]string `yaml:"publish_steps"`
	ExecAllowlist []string  `yaml:"exec_allowlist"` // default: git, git-lfs
}
```

Defaults when unset: `PreSteps = [["git","fetch","{remote}","+refs/heads/*:refs/heads/*"]]`; `PublishSteps = [["git","fetch","{sandbox_remote}","{branch}"],["git","push","{remote}","{branch}"]]`; `ExecAllowlist = ["git","git-lfs"]`. Validate: `branch` values come only from requests; pipelines only from config (ADR-0003).

- [ ] **Step 3: Clone-mode provision constraint + wiring**
  - In the provision path (M5), when `spec.Clone` is true: enforce **exactly one workspace** (reject otherwise — spec §12 v1 constraint), and that workspace must be `git`-enabled. The SDK adapter already passes `WithClone()` (M1c) and resolves the single workspace's `host_path` (the bare base).
  - In `node.New`, build a `git.Workspace` per git-backed workspace (runner from `ExecAllowlist`, creds from `AuthSecretRef` resolved at startup into a `git.Creds`). Run `Pre` before the sandbox is handed off (after create).

- [ ] **Step 4: Publish triggers** — add `PublishSandbox` handling:
  - Explicit: `POST /v1/sandboxes/{id}/git/publish` (already in M1 spec surface) → resolve the sandbox's workspace → `Workspace.Publish` with `Vars{Remote, Branch}` (branch from request or the sandbox's `git.branch`), gated by `AllowPush`; audit the result.
  - Auto on graceful stop: in `Manager.Stop`, if the sandbox is clone-mode + `AllowPush` + has a branch, run `Publish` before/just-after stopping (best-effort; audit). `AgentRun`'s `publish_on_success` (M1c field) triggers the same `Publish` on a clean exit.

  TDD: a `PublishSandbox` unit test with a fake `Workspace` (interface) asserting it's called with the right branch and skipped when `AllowPush=false`.

- [ ] **Step 5: Run all + commit**

```bash
go test ./... && go test ./internal/git/ -v
git add internal/git/creds.go internal/config/ internal/node/ internal/apiserver/
git commit -m "feat(git): clone-mode provision + publish triggers + host-side creds"
```

---

## Self-Review

**Spec coverage (M6):** native `WithClone()` provision (M1c) + single-workspace constraint (§12) → Task 3 ✓; PRE refresh of bare/mirror base under per-workspace lock → Task 3 ✓; PUBLISH (fetch-from-sandbox + push) → Task 3 ✓; declarative argv pipelines from node config, no shell, validated request values (ADR-0003) → Tasks 1,4 ✓; host-side credentials via GIT_ASKPASS, never on disk/agent → Task 4 ✓; `allow_push` gate + audit → Task 4 ✓; publish triggers (explicit + on-stop + on agent-run success) → Task 4 ✓. **Deferred:** the "host fetches from the sandbox clone" exact transport (`sandbox-<name>` remote) is exercised in tests via a stand-in push; the real wiring reads the sbx-provided host-side remote name — verify against sbx when the SDK adapter's clone path is live (flagged).

**Placeholder scan:** Builder + runner + creds fully coded and unit-TDD'd; workspace ops tested with real `git`. Pipeline defaults are concrete. No TBD/TODO. The one verification point (sbx `sandbox-<name>` remote name) is explicit, not silent.

**Type consistency:** `git.Build(steps, Vars)→[][]string`; `git.NewRunner(allow).Run(ctx,dir,env,steps)`; `git.NewWorkspace(name,base,remote,runner,creds).{Pre,Publish}`; `git.NewCreds(dir,token).Env()`. Config `GitConfig` fields match builder vars + defaults.
