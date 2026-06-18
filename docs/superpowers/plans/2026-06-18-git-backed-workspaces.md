# Git-Backed Workspaces (clone mode) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Tasks 3 and 9 need the `git` binary.

**Goal:** Provision a sandbox on a private in-container clone of a git-backed workspace (`sbx --clone`), freshen the bare base from upstream before cloning (PRE), and publish the agent's branch upstream — via node-local, shell-free, argv-step pipelines, serialized by a per-workspace lock, with credentials as a host-side operator concern.

**Architecture:** A new `internal/git` package turns declarative argv steps + validated values into commands run without a shell (`builder` → `runner` → `Workspace`). The owner node runs PRE under a per-workspace lock spanning the clone-sourcing `Create`; publish (explicit RPC + agent-run success + on-stop) runs the same lock briefly. Enforcement of the clone⟺git-backed bijection and PRE happen at the **target/owner** node in a shared `ProvisionLocal` helper. Credentials are operator host-side git config (ADR-0014); the package never holds a token.

**Tech Stack:** Go 1.25, `os/exec` + `git`, existing M1–M5 stack (grpc-gateway, bbolt store, memberlist gossip, ops/audit/events).

Design: `docs/superpowers/specs/2026-06-18-git-backed-workspaces-design.md`. Decisions: ADR-0003 (commands are node config), ADR-0014 (host-side credentials), ADR-0015 (clone-only bijection).

## Global Constraints

- **Go 1.25**; module `github.com/squall-chua/sbx-swarm-node`.
- **No shell.** Git steps run via `exec.CommandContext(argv[0], argv[1:]...)`, never a shell string.
- **Commands come only from node config** (ADR-0003). The wire/API carries a workspace *name* + the single validated value `{branch}`, never argv.
- **`{branch}` is the only request-supplied value** — validate it as a git ref (reject leading `-`, control chars, `..`, anything outside `[A-Za-z0-9._/\-]`).
- **No credential code** (ADR-0014). The runner sets `GIT_TERMINAL_PROMPT=0` so a missing credential fails fast.
- **Every new gRPC method must be classified** in `internal/apiserver/authz.go` or `TestAuthz_AllMethodsClassified` fails.
- **Codegen needs network:** edit `.proto` → run `buf generate` at repo root → commit regenerated `internal/gen/sbxswarm/v1/*`. gopls may show false undefined/redeclared after codegen — trust only the `go` toolchain.
- **Standalone must keep working:** a node with no cluster must boot; git workspaces have no cluster dependency.
- **Verification bar:** `go build ./... && go vet ./... && go test ./...`; the `-tags integration` suite when cross-node behavior changes; commit after each green task.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/git/builder.go` (create) | Substitute validated values into argv; reject injection. |
| `internal/git/runner.go` (create) | Run argv steps via `os/exec` (allowlist, stop-on-error, capture). |
| `internal/git/workspace.go` (create) | Per-workspace lock; `PreLock` (lock+PRE, returns unlock) and `Publish` (lock+publish). |
| `internal/config/config.go` (modify) | `GitConfig` on `WorkspaceConfig`; defaults + validation. |
| `proto/sbxswarm/v1/sandbox.proto` (modify) | `CreateSandboxRequest.branch`; `PublishSandbox` RPC; `Sandbox.branch`/`last_publish`. |
| `internal/sandbox/backend.go` (modify) | `CreateSpec.Branch`. |
| `internal/sandbox/record.go` (modify) | `Record.LastPublish`. |
| `internal/apiserver/provision_git.go` (create) | `ProvisionLocal` shared helper: bijection + PRE + AdmitAndCreate. |
| `internal/apiserver/sandboxservice.go` (modify) | `PublishSandbox` + `doPublish`; wire `publish_on_success` + on-stop; git fields in `toSpec`/`toProto`. |
| `internal/apiserver/provision.go` (modify) | `InternalService` uses `ProvisionLocal` + holds `gitWS`. |
| `internal/apiserver/authz.go` (modify) | Classify `PublishSandbox` as mutating. |
| `internal/apiserver/forward.go` (modify) | Register `PublishSandbox` reply type. |
| `internal/node/node.go` (modify) | Build `gitWS` map; inject into service/internal/attemptFor. |

---

## Task 1: git argv builder (substitution + ref validation)

**Files:**
- Create: `internal/git/builder.go`
- Test: `internal/git/builder_test.go`

**Interfaces:**
- Produces: `type Vars struct { Branch, BaseRef, Remote, SandboxRemote string }`; `func Build(steps [][]string, v Vars) ([][]string, error)`.

- [ ] **Step 1: Write the failing test**

```go
package git

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuild_SubstitutesValidatedValues(t *testing.T) {
	vars := Vars{Branch: "agent/task-1", BaseRef: "main", Remote: "origin", SandboxRemote: "sandbox-n1.abc"}
	steps := [][]string{
		{"git", "fetch", "{sandbox_remote}", "+refs/heads/{branch}:refs/heads/{branch}"},
		{"git", "push", "{remote}", "{branch}"},
	}
	argv, err := Build(steps, vars)
	require.NoError(t, err)
	require.Equal(t, []string{"git", "fetch", "sandbox-n1.abc", "+refs/heads/agent/task-1:refs/heads/agent/task-1"}, argv[0])
	require.Equal(t, []string{"git", "push", "origin", "agent/task-1"}, argv[1])
}

func TestBuild_RejectsInjection(t *testing.T) {
	_, err := Build([][]string{{"git", "checkout", "{branch}"}}, Vars{Branch: "--upload-pack=evil"})
	require.Error(t, err) // leading '-' rejected
	_, err = Build([][]string{{"git", "checkout", "{branch}"}}, Vars{Branch: "bad\nname"})
	require.Error(t, err) // control char rejected
	_, err = Build([][]string{{"git", "checkout", "{branch}"}}, Vars{Branch: "a..b"})
	require.Error(t, err) // ".." rejected
}

func TestBuild_EmptyValueAllowed(t *testing.T) {
	// An unset value is fine; a step simply may not reference it.
	_, err := Build([][]string{{"git", "fetch", "{remote}"}}, Vars{Remote: "origin"})
	require.NoError(t, err)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/git/ -run TestBuild -v`
Expected: FAIL (`undefined: Build`, `undefined: Vars`).

- [ ] **Step 3: Write the implementation**

```go
// Package git runs declarative, shell-free git pipelines for clone-mode
// workspaces (ADR-0003). Commands come from node config; only Vars below are
// request-supplied, and they are validated and bound as discrete argv.
package git

import (
	"fmt"
	"regexp"
	"strings"
)

// Vars are the values bound into pipeline steps. Only Branch is request-supplied;
// the rest are config-/runtime-derived. All are validated as git refs.
type Vars struct {
	Branch        string
	BaseRef       string
	Remote        string
	SandboxRemote string
}

// refOK: no leading '-', no control chars/spaces, no '..' (checked separately).
var refOK = regexp.MustCompile(`^[A-Za-z0-9._/\-]+$`)

func validateRef(name, val string) error {
	if val == "" {
		return nil // unset: a step may simply not reference it
	}
	if strings.HasPrefix(val, "-") || strings.Contains(val, "..") || !refOK.MatchString(val) {
		return fmt.Errorf("invalid %s %q", name, val)
	}
	return nil
}

// Build substitutes vars into each step's argv tokens, after validating every
// value as a git ref. Values are bound as discrete argv elements (never
// shell-interpreted).
func Build(steps [][]string, v Vars) ([][]string, error) {
	for _, f := range []struct{ name, val string }{
		{"branch", v.Branch}, {"base_ref", v.BaseRef},
		{"remote", v.Remote}, {"sandbox_remote", v.SandboxRemote},
	} {
		if err := validateRef(f.name, f.val); err != nil {
			return nil, err
		}
	}
	repl := strings.NewReplacer(
		"{branch}", v.Branch, "{base_ref}", v.BaseRef,
		"{remote}", v.Remote, "{sandbox_remote}", v.SandboxRemote,
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/git/ -run TestBuild -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/git/builder.go internal/git/builder_test.go
git commit -m "feat(git): shell-free argv builder with ref validation (ADR-0003)"
```

---

## Task 2: git pipeline runner (allowlist + capture + stop-on-error)

**Files:**
- Create: `internal/git/runner.go`
- Test: `internal/git/runner_test.go`

**Interfaces:**
- Produces: `type StepResult struct { Argv []string; ExitCode int; Output []byte }`; `type Runner struct{...}`; `func NewRunner(allow []string) *Runner`; `func (r *Runner) Run(ctx context.Context, dir string, env []string, steps [][]string) ([]StepResult, error)`.

- [ ] **Step 1: Write the failing test**

```go
package git

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRunner_RunsAllowedStepsStopsOnError(t *testing.T) {
	r := NewRunner([]string{"echo", "false"})

	res, err := r.Run(context.Background(), t.TempDir(), nil, [][]string{{"echo", "hello"}})
	require.NoError(t, err)
	require.Len(t, res, 1)
	require.Equal(t, 0, res[0].ExitCode)

	res, err = r.Run(context.Background(), t.TempDir(), nil, [][]string{{"false"}, {"echo", "never"}})
	require.Error(t, err)  // stops at the failing step
	require.Len(t, res, 1) // second step never ran
}

func TestRunner_RejectsDisallowedBinary(t *testing.T) {
	r := NewRunner([]string{"git"})
	_, err := r.Run(context.Background(), t.TempDir(), nil, [][]string{{"rm", "-rf", "/"}})
	require.ErrorContains(t, err, "not allowed")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/git/ -run TestRunner -v`
Expected: FAIL (`undefined: NewRunner`).

- [ ] **Step 3: Write the implementation**

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
// of binaries (defense in depth on top of config-only commands, ADR-0003).
type Runner struct{ allow map[string]bool }

// NewRunner permits only the given binaries (e.g. "git", "git-lfs").
func NewRunner(allow []string) *Runner {
	m := make(map[string]bool, len(allow))
	for _, a := range allow {
		m[a] = true
	}
	return &Runner{allow: m}
}

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
			res.ExitCode = -1
			if ee, ok := err.(*exec.ExitError); ok {
				res.ExitCode = ee.ExitCode()
			}
			results = append(results, res)
			return results, fmt.Errorf("step %v failed (exit %d): %s", argv, res.ExitCode, buf.String())
		}
		results = append(results, res)
	}
	return results, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/git/ -run TestRunner -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/git/runner.go internal/git/runner_test.go
git commit -m "feat(git): allowlisted argv pipeline runner (no shell)"
```

---

## Task 3: git.Workspace — PreLock + Publish against real git

**Files:**
- Create: `internal/git/workspace.go`
- Test: `internal/git/workspace_test.go` (needs the `git` binary; skips if absent)

**Interfaces:**
- Consumes: `Build`, `NewRunner`, `Runner.Run` (Tasks 1–2).
- Produces:
  - `type Spec struct { Name, Base, Remote, DefaultBranch string; AllowPush bool; PreSteps, PublishSteps [][]string; Allowlist []string }`
  - `func New(s Spec) *Workspace`
  - `func (w *Workspace) Name() string`, `func (w *Workspace) AllowPush() bool`
  - `func (w *Workspace) PreLock(ctx context.Context, branch string) (unlock func(), err error)` — locks, runs PRE; on success returns the unlock func (caller holds the lock across the clone-sourcing Create); on PRE error unlocks and returns the error.
  - `func (w *Workspace) Publish(ctx context.Context, branch, sandboxRemote string) error` — locks, runs PUBLISH, unlocks.

- [ ] **Step 1: Write the failing test**

```go
package git

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

// build: upstream (bare) <- work (clone, has main) ; base = mirror of upstream ;
// "sandbox" stood in by a second clone with branch agent/x registered as a
// remote on base. PreLock fetches refs into base; Publish fetches agent/x from
// the sandbox remote and pushes it upstream.
func TestWorkspace_PreLockAndPublish(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	upstream := filepath.Join(root, "upstream.git")
	work := filepath.Join(root, "work")
	base := filepath.Join(root, "base.git")
	sbx := filepath.Join(root, "sbx")

	gitCmd(t, root, "init", "--bare", upstream)
	gitCmd(t, root, "clone", upstream, work)
	gitCmd(t, work, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "--allow-empty", "-m", "init")
	gitCmd(t, work, "push", "origin", "HEAD:main")
	gitCmd(t, root, "clone", "--mirror", upstream, base)

	// Stand in for the in-container clone: a repo with branch agent/x, exposed to
	// the base as a remote named "sandbox-fake" (sbx wires this as sandbox-<name>).
	gitCmd(t, root, "clone", upstream, sbx)
	gitCmd(t, sbx, "checkout", "-b", "agent/x")
	gitCmd(t, sbx, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "--allow-empty", "-m", "agent work")
	gitCmd(t, base, "remote", "add", "sandbox-fake", sbx)

	ws := New(Spec{
		Name: "repo", Base: base, Remote: "origin", DefaultBranch: "main", AllowPush: true,
		PreSteps:     [][]string{{"git", "fetch", "{remote}", "+refs/heads/*:refs/heads/*"}},
		PublishSteps: [][]string{{"git", "fetch", "{sandbox_remote}", "+refs/heads/{branch}:refs/heads/{branch}"}, {"git", "push", "{remote}", "{branch}"}},
		Allowlist:    []string{"git"},
	})

	unlock, err := ws.PreLock(context.Background(), "agent/x")
	require.NoError(t, err)
	unlock()

	require.NoError(t, ws.Publish(context.Background(), "agent/x", "sandbox-fake"))

	// upstream now has agent/x
	cmd := exec.Command("git", "branch", "--list", "agent/x")
	cmd.Dir = upstream
	out, _ := cmd.CombinedOutput()
	require.Contains(t, string(out), "agent/x")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/git/ -run TestWorkspace -v`
Expected: FAIL (`undefined: New`, `undefined: Spec`).

- [ ] **Step 3: Write the implementation**

```go
package git

import (
	"context"
	"sync"
)

// Spec is the resolved config for one git-backed workspace (built in node.go from
// config; git stays config-package-agnostic).
type Spec struct {
	Name          string
	Base          string // host_path: the bare/mirror base repo
	Remote        string
	DefaultBranch string
	AllowPush     bool
	PreSteps      [][]string
	PublishSteps  [][]string
	Allowlist     []string
}

// Workspace orchestrates the clone-mode git lifecycle for one git-backed
// workspace, serializing operations on its bare base with a per-workspace lock.
type Workspace struct {
	spec   Spec
	runner *Runner
	mu     sync.Mutex
}

// New builds a Workspace orchestrator.
func New(s Spec) *Workspace { return &Workspace{spec: s, runner: NewRunner(s.Allowlist)} }

func (w *Workspace) Name() string    { return w.spec.Name }
func (w *Workspace) AllowPush() bool { return w.spec.AllowPush }

// env disables interactive credential prompts so a missing/expired host-side
// credential fails fast instead of hanging (ADR-0014: creds are host-side).
func gitEnv() []string { return []string{"GIT_TERMINAL_PROMPT=0"} }

// PreLock locks the workspace and runs the PRE pipeline (freshen the bare base
// from upstream). On success it returns an unlock func — the caller MUST hold the
// lock across the clone-sourcing Create so a concurrent PRE (which may prune)
// cannot race the clone-read — then call unlock. On PRE error it unlocks and
// returns the error.
func (w *Workspace) PreLock(ctx context.Context, branch string) (func(), error) {
	w.mu.Lock()
	vars := Vars{Branch: branch, Remote: w.spec.Remote, BaseRef: w.spec.DefaultBranch}
	argv, err := Build(w.spec.PreSteps, vars)
	if err != nil {
		w.mu.Unlock()
		return nil, err
	}
	if _, err := w.runner.Run(ctx, w.spec.Base, gitEnv(), argv); err != nil {
		w.mu.Unlock()
		return nil, err
	}
	return w.mu.Unlock, nil
}

// Publish locks the workspace and runs the PUBLISH pipeline (fetch the branch
// from the sandbox remote, push it upstream). Self-contained (no spanning).
func (w *Workspace) Publish(ctx context.Context, branch, sandboxRemote string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	vars := Vars{Branch: branch, Remote: w.spec.Remote, BaseRef: w.spec.DefaultBranch, SandboxRemote: sandboxRemote}
	argv, err := Build(w.spec.PublishSteps, vars)
	if err != nil {
		return err
	}
	_, err = w.runner.Run(ctx, w.spec.Base, gitEnv(), argv)
	return err
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/git/ -run TestWorkspace -v`
Expected: PASS (or SKIP if `git` is absent).

- [ ] **Step 5: Commit**

```bash
git add internal/git/workspace.go internal/git/workspace_test.go
git commit -m "feat(git): per-workspace-locked PreLock/Publish against bare base"
```

---

## Task 4: config GitConfig + defaults + validation

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go` (append)

**Interfaces:**
- Produces: `GitConfig` struct on `WorkspaceConfig`; `func (g GitConfig) WithDefaults() GitConfig`.

- [ ] **Step 1: Write the failing test**

```go
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

func TestValidate_GitWorkspaceNeedsHostPath(t *testing.T) {
	cfg := Default()
	cfg.Workspaces = []WorkspaceConfig{{Name: "repo", Git: &GitConfig{}}}
	require.ErrorContains(t, cfg.Validate(), "host_path")
}
```

Add `"github.com/stretchr/testify/require"` to the test file imports if not present.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run "TestGitConfig_WithDefaults|TestValidate_GitWorkspaceNeedsHostPath" -v`
Expected: FAIL (`unknown field Git`, `undefined: GitConfig`).

- [ ] **Step 3: Write the implementation**

In `internal/config/config.go`, extend `WorkspaceConfig` and add `GitConfig`:

```go
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
```

In `Validate()`, before the final `return nil`, add the git-workspace check:

```go
	for _, w := range c.Workspaces {
		if w.Git != nil && w.HostPath == "" {
			return fmt.Errorf("workspace %q is git-backed but has no host_path", w.Name)
		}
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): git-backed workspace config (GitConfig) + defaults"
```

---

## Task 5: proto + codegen + Go field plumbing

**Files:**
- Modify: `proto/sbxswarm/v1/sandbox.proto`
- Regenerate: `internal/gen/sbxswarm/v1/*` (via `buf generate`)
- Modify: `internal/sandbox/backend.go`, `internal/sandbox/record.go`, `internal/apiserver/sandboxservice.go`
- Test: `internal/apiserver/sandboxservice_test.go` (append a mapping test)

**Interfaces:**
- Produces: proto `CreateSandboxRequest.branch` (field 13); `rpc PublishSandbox`; `PublishSandboxRequest{id,branch}`; `Sandbox.branch`/`last_publish`. Go: `CreateSpec.Branch`, `Record.LastPublish`, `toSpec` copies branch, `toProto` emits branch/last_publish.

- [ ] **Step 1: Edit the proto**

In `proto/sbxswarm/v1/sandbox.proto`, add the RPC to `SandboxService`:

```proto
  rpc PublishSandbox(PublishSandboxRequest) returns (Operation) {
    option (google.api.http) = {post: "/v1/sandboxes/{id}/git/publish" body: "*"};
  }
```

Add `branch` to `CreateSandboxRequest` (after field 12):

```proto
  string branch = 13; // clone-mode: branch the agent works on / auto-publish target
```

Add git fields to `Sandbox` (after field 5):

```proto
  string branch = 6;       // clone-mode recorded branch
  string last_publish = 7; // RFC3339; empty if never published
```

Add the request message (near `IdRequest`):

```proto
message PublishSandboxRequest {
  string id = 1;
  string branch = 2; // optional: overrides the recorded branch
}
```

- [ ] **Step 2: Regenerate code**

Run: `buf generate` (at repo root)
Expected: `internal/gen/sbxswarm/v1/sandbox.pb.go` and `sandbox_grpc.pb.go` updated with `GetBranch()`, `PublishSandbox`, `PublishSandboxRequest`, `Sandbox.Branch`/`LastPublish`. (gopls may show stale errors — ignore; the `go` toolchain is authoritative.)

- [ ] **Step 3: Add the Go fields + mapping**

`internal/sandbox/backend.go` — add `Branch` to `CreateSpec`:

```go
	Clone       bool
	Branch      string // clone-mode recorded branch (auto-publish target)
	Workspaces  []WorkspaceMount
```

`internal/sandbox/record.go` — add `LastPublish`:

```go
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	LastPublish time.Time         `json:"last_publish,omitempty"`
```

`internal/apiserver/sandboxservice.go` — copy branch in `toSpec`, emit git fields in `toProto`:

```go
	return sandbox.CreateSpec{
		Agent: r.Agent, Template: r.Template, CPUs: int(r.Cpus),
		MemoryBytes: r.MemoryBytes, DiskGB: r.DiskGb, Clone: r.Clone, Branch: r.Branch, Workspaces: ws, Env: r.Env,
	}
```

```go
func toProto(rec *sandbox.Record) *sbxv1.Sandbox {
	ports := make([]*sbxv1.Port, 0, len(rec.Ports))
	for _, p := range rec.Ports {
		ports = append(ports, &sbxv1.Port{ContainerPort: int32(p.ContainerPort), HostPort: int32(p.HostPort)})
	}
	var lastPub string
	if !rec.LastPublish.IsZero() {
		lastPub = rec.LastPublish.UTC().Format(time.RFC3339)
	}
	return &sbxv1.Sandbox{
		Id: rec.ID, OwnerNode: rec.OwnerNode, Status: rec.Status, Ports: ports, Labels: rec.Labels,
		Branch: rec.Spec.Branch, LastPublish: lastPub,
	}
}
```

- [ ] **Step 4: Write the failing test** (append to `internal/apiserver/sandboxservice_test.go`)

```go
func TestToProto_GitFields(t *testing.T) {
	rec := &sandbox.Record{ID: "n1.x", Status: "running", Spec: sandbox.CreateSpec{Branch: "agent/x"}}
	p := toProto(rec)
	require.Equal(t, "agent/x", p.Branch)
	require.Empty(t, p.LastPublish)

	rec.LastPublish = time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)
	require.Equal(t, "2026-06-18T09:00:00Z", toProto(rec).LastPublish)
}
```

- [ ] **Step 5: Build + test**

Run: `go build ./... && go test ./internal/apiserver/ -run TestToProto_GitFields -v`
Expected: build OK; test PASS.

- [ ] **Step 6: Commit**

```bash
git add proto/ internal/gen/ internal/sandbox/backend.go internal/sandbox/record.go internal/apiserver/sandboxservice.go internal/apiserver/sandboxservice_test.go
git commit -m "feat(proto): branch + PublishSandbox + sandbox git fields"
```

---

## Task 6: ProvisionLocal — bijection + PRE on the owner node

**Files:**
- Create: `internal/apiserver/provision_git.go`
- Test: `internal/apiserver/provision_git_test.go`

**Interfaces:**
- Consumes: `git.Workspace.PreLock` (Task 3); `sandbox.Manager.AdmitAndCreate`, `sandbox.CreateSpec` (Task 5).
- Produces: `func ProvisionLocal(ctx context.Context, mgr *sandbox.Manager, gitWS map[string]*git.Workspace, spec sandbox.CreateSpec) (*sandbox.Record, error)`.

This is the single owner-node create chokepoint shared by `attemptFor` (local target) and `InternalService.Provision` (remote target). It enforces the clone⟺git-backed bijection (ADR-0015) — git-backed-ness is node-local, so only the owner can check it — runs PRE under the workspace lock spanning Create, then admits+creates.

- [ ] **Step 1: Write the failing test**

```go
package apiserver

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/git"
	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/ops"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newTestManager builds a fake-backend Manager with ample capacity (shared by the
// git tests in this package). Mirrors the inline construction in the existing
// newSandboxSvc / provision_test.go.
func newTestManager(t *testing.T) *sandbox.Manager {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	mgr := sandbox.NewManager("n1", sandbox.NewFake(), st, ids.NewGen("n1"))
	mgr.SetCapacity(sandbox.NewCapacity(4, 1e9, 1e9))
	return mgr
}

// newTestOps builds an ops.Manager (its own store; the tested publish paths don't
// exercise op persistence, so a separate store is fine).
func newTestOps(t *testing.T) *ops.Manager {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "ops.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return ops.NewManager(st, ids.NewGen("n1"))
}

func newGitWS(t *testing.T) map[string]*git.Workspace {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	base := filepath.Join(root, "base.git")
	cmd := exec.Command("git", "init", "--bare", base)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	ws := git.New(git.Spec{
		Name: "repo", Base: base, Remote: "origin", DefaultBranch: "main", AllowPush: true,
		PreSteps:  [][]string{{"git", "fetch", "--all"}}, // no remote configured => succeeds as a no-op-ish fetch
		Allowlist: []string{"git"},
	})
	return map[string]*git.Workspace{"repo": ws}
}

func TestProvisionLocal_RejectsNonCloneGitBacked(t *testing.T) {
	gitWS := newGitWS(t)
	mgr := newTestManager(t) // helper used elsewhere in this package's tests
	_, err := ProvisionLocal(context.Background(), mgr, gitWS, sandbox.CreateSpec{
		Agent: "shell", Clone: false, Workspaces: []sandbox.WorkspaceMount{{Name: "repo"}},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestProvisionLocal_RejectsCloneWithNonGitWorkspace(t *testing.T) {
	gitWS := newGitWS(t)
	mgr := newTestManager(t)
	_, err := ProvisionLocal(context.Background(), mgr, gitWS, sandbox.CreateSpec{
		Agent: "shell", Clone: true, Workspaces: []sandbox.WorkspaceMount{{Name: "not-git"}},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestProvisionLocal_CloneRunsPreAndCreates(t *testing.T) {
	gitWS := newGitWS(t)
	mgr := newTestManager(t)
	rec, err := ProvisionLocal(context.Background(), mgr, gitWS, sandbox.CreateSpec{
		Agent: "shell", Clone: true, Branch: "agent/x", Workspaces: []sandbox.WorkspaceMount{{Name: "repo"}},
	})
	require.NoError(t, err)
	require.Equal(t, "agent/x", rec.Spec.Branch)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/apiserver/ -run TestProvisionLocal -v`
Expected: FAIL (`undefined: ProvisionLocal`).

- [ ] **Step 3: Write the implementation**

```go
package apiserver

import (
	"context"

	"github.com/squall-chua/sbx-swarm-node/internal/git"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ProvisionLocal enforces the clone <=> git-backed bijection (ADR-0015), runs the
// PRE pipeline under the workspace lock spanning Create, then admits+creates. It
// runs on the OWNER node (the only place that knows which workspaces are
// git-backed — that is node-local config, not gossiped). Shared by the local
// attempt (attemptFor) and the forwarded InternalService.Provision.
func ProvisionLocal(ctx context.Context, mgr *sandbox.Manager, gitWS map[string]*git.Workspace, spec sandbox.CreateSpec) (*sandbox.Record, error) {
	var gw *git.Workspace
	gitRefs := 0
	for _, w := range spec.Workspaces {
		if g, ok := gitWS[w.Name]; ok {
			gitRefs++
			gw = g
		}
	}
	if spec.Clone {
		if len(spec.Workspaces) != 1 || gw == nil {
			return nil, status.Error(codes.InvalidArgument, "clone mode requires exactly one git-backed workspace")
		}
		unlock, err := gw.PreLock(ctx, spec.Branch)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "git pre: %v", err)
		}
		defer unlock() // hold the lock across Create (clone-sourcing)
	} else if gitRefs > 0 {
		return nil, status.Error(codes.InvalidArgument, "git-backed workspace requires clone mode")
	}
	return mgr.AdmitAndCreate(ctx, spec)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/apiserver/ -run TestProvisionLocal -v`
Expected: PASS (or SKIP without git).

- [ ] **Step 5: Commit**

```bash
git add internal/apiserver/provision_git.go internal/apiserver/provision_git_test.go
git commit -m "feat(apiserver): ProvisionLocal — clone<->git-backed bijection + PRE (ADR-0015)"
```

---

## Task 7: PublishSandbox RPC (doPublish) + authz + forward

**Files:**
- Modify: `internal/apiserver/sandboxservice.go`
- Modify: `internal/apiserver/authz.go`
- Modify: `internal/apiserver/forward.go`
- Test: `internal/apiserver/sandboxservice_test.go` (append), `internal/apiserver/authz_test.go` (drift guard already covers it)

**Interfaces:**
- Consumes: `git.Workspace.Publish`/`AllowPush`/`Name` (Task 3); `audit.Log.Record` (existing); `sandbox.Manager.Get`/`Resolve` (existing).
- Produces: `SandboxService.gitWS`, `SandboxService.audit`, `SandboxService.events` fields + setters `SetGit`, `SetAudit`, `SetEvents`; `func (s *SandboxService) doPublish(ctx, sandboxID, reqBranch string) error`; `PublishSandbox` handler.

- [ ] **Step 1: Add fields + setters to SandboxService**

In `internal/apiserver/sandboxservice.go`, extend the struct and add setters:

```go
type SandboxService struct {
	sbxv1.UnimplementedSandboxServiceServer
	mgr              *sandbox.Manager
	ops              *ops.Manager
	obs              ObserveDeps
	place            PlaceFunc
	defaultStrategy  string
	defaultResources sandbox.Resources
	gitWS            map[string]*git.Workspace
	audit            *audit.Log
	events           events.Publisher
}

// SetGit wires git-backed workspaces (by name) for the publish path.
func (s *SandboxService) SetGit(ws map[string]*git.Workspace) { s.gitWS = ws }

// SetAudit wires the audit log for git operations.
func (s *SandboxService) SetAudit(a *audit.Log) { s.audit = a }

// SetEvents wires the event publisher for publish success/failure signals.
func (s *SandboxService) SetEvents(p events.Publisher) { s.events = p }
```

Add imports: `"github.com/squall-chua/sbx-swarm-node/internal/audit"`, `"github.com/squall-chua/sbx-swarm-node/internal/events"`, `"github.com/squall-chua/sbx-swarm-node/internal/git"`.

- [ ] **Step 2: Implement `doPublish`**

```go
// doPublish runs the publish pipeline for a sandbox's git-backed workspace and
// audits/emits the outcome. Used by the explicit RPC and the auto-triggers. The
// sandbox must be running (the sandbox-<name> fetch needs the live git-daemon).
func (s *SandboxService) doPublish(ctx context.Context, sandboxID, reqBranch string) error {
	rec, err := s.mgr.Get(ctx, sandboxID)
	if err == sandbox.ErrNotFound {
		return status.Error(codes.NotFound, "sandbox not found")
	}
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	if len(rec.Spec.Workspaces) != 1 {
		return status.Error(codes.FailedPrecondition, "sandbox is not clone-mode")
	}
	ws := s.gitWS[rec.Spec.Workspaces[0].Name]
	if ws == nil {
		return status.Error(codes.FailedPrecondition, "workspace is not git-backed")
	}
	if !ws.AllowPush() {
		return status.Error(codes.FailedPrecondition, "workspace does not allow push")
	}
	branch := reqBranch
	if branch == "" {
		branch = rec.Spec.Branch
	}
	if branch == "" {
		return status.Error(codes.FailedPrecondition, "no branch to publish")
	}
	if rec.Status != "running" {
		return status.Error(codes.FailedPrecondition, "sandbox not running; cannot reach sandbox-"+rec.BackendName)
	}

	perr := ws.Publish(ctx, branch, "sandbox-"+rec.BackendName)
	s.auditPublish(ws.Name(), branch, perr)
	if perr != nil {
		s.emit("sandbox.publish_failed", sandboxID, map[string]string{"branch": branch})
		return status.Errorf(codes.Internal, "publish: %v", perr)
	}
	s.emit("sandbox.published", sandboxID, map[string]string{"branch": branch})
	_ = s.mgr.SetLastPublish(ctx, sandboxID, time.Now())
	return nil
}

func (s *SandboxService) auditPublish(workspace, branch string, err error) {
	if s.audit == nil {
		return
	}
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	_ = s.audit.Record(audit.Entry{Action: "git.publish", Target: workspace + "@" + branch, Outcome: outcome})
}

func (s *SandboxService) emit(eventType, sandboxID string, payload map[string]string) {
	if s.events != nil {
		s.events.Publish(eventType, sandboxID, payload)
	}
}
```

> `audit.Entry` has `Actor/Action/Target/Outcome` (no value field). `events.Publisher.Publish(eventType, sandboxID string, payload map[string]string)` matches the bus signature used by `ops`/`manager`.

- [ ] **Step 3: Add `SetLastPublish` to the manager**

In `internal/sandbox/manager.go`, add (mirrors the existing save pattern):

```go
// SetLastPublish records a successful publish time on the sandbox record.
func (m *Manager) SetLastPublish(ctx context.Context, id string, t time.Time) error {
	rec, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	rec.LastPublish = t
	return m.save(rec)
}
```

(Add `"time"` to imports if not already present.)

- [ ] **Step 4: Implement the `PublishSandbox` handler** (in `sandboxservice.go`)

```go
// PublishSandbox starts an async git-publish operation (owner-local; forwarded
// by id). Mutating (admin-only, authz.go).
func (s *SandboxService) PublishSandbox(ctx context.Context, r *sbxv1.PublishSandboxRequest) (*sbxv1.Operation, error) {
	op, _, err := s.ops.Start(ctx, "git-publish", idempotencyKey(ctx))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	id, branch := r.Id, r.Branch
	s.ops.Run(op.ID, func() (string, error) { return id, s.doPublish(context.Background(), id, branch) })
	return opProto(op), nil
}
```

- [ ] **Step 5: Classify in authz + register forwarding**

`internal/apiserver/authz.go` — add to `mutatingMethods`:

```go
	"/sbxswarm.v1.SandboxService/PublishSandbox": true,
```

`internal/apiserver/forward.go` — add to `newReplyFor`'s switch:

```go
	case "/sbxswarm.v1.SandboxService/PublishSandbox":
		return new(sbxv1.Operation)
```

- [ ] **Step 6: Write the failing test** (append to `sandboxservice_test.go`)

```go
func TestPublishSandbox_AllowPushGate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	// Build a service over a git-backed workspace with AllowPush=false.
	root := t.TempDir()
	base := filepath.Join(root, "base.git")
	out, err := exec.Command("git", "init", "--bare", base).CombinedOutput()
	require.NoError(t, err, string(out))
	ws := git.New(git.Spec{Name: "repo", Base: base, Remote: "origin", AllowPush: false, Allowlist: []string{"git"}})

	mgr := newTestManager(t)
	rec, err := mgr.AdmitAndCreate(context.Background(), sandbox.CreateSpec{
		Agent: "shell", Clone: true, Branch: "agent/x", Workspaces: []sandbox.WorkspaceMount{{Name: "repo"}},
	})
	require.NoError(t, err)

	svc := NewSandboxService(mgr, newTestOps(t))
	svc.SetGit(map[string]*git.Workspace{"repo": ws})

	err = svc.doPublish(context.Background(), rec.ID, "")
	require.Equal(t, codes.FailedPrecondition, status.Code(err)) // allow_push=false
}
```

> Use the existing test ops-manager helper (`newTestOps`/equivalent) already present in the package; if absent, build one with `ops.NewManager(st, gen)` as the other tests do.

- [ ] **Step 7: Run + build + commit**

Run: `go build ./... && go test ./internal/apiserver/ -run "TestPublishSandbox|TestAuthz" -v`
Expected: PASS (the authz drift-guard test stays green because `PublishSandbox` is now classified).

```bash
git add internal/apiserver/sandboxservice.go internal/apiserver/authz.go internal/apiserver/forward.go internal/sandbox/manager.go internal/apiserver/sandboxservice_test.go
git commit -m "feat(apiserver): PublishSandbox RPC (git-publish op) + authz + forward + audit"
```

---

## Task 8: auto-triggers — agent-run success + on graceful stop

**Files:**
- Modify: `internal/apiserver/sandboxservice.go`
- Test: `internal/apiserver/sandboxservice_test.go` (append)

**Interfaces:**
- Consumes: `doPublish` (Task 7).
- Behavior: `AgentRun` publishes the recorded branch on exit 0 when `publish_on_success`; `StopSandbox` publishes the recorded branch before stopping. Both best-effort (don't fail the run / don't block the stop), audited + logged + evented via `doPublish`.

- [ ] **Step 1: Write the failing test** (append to `sandboxservice_test.go`)

```go
func TestStopSandbox_AutoPublishesThenStops(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	upstream := filepath.Join(root, "up.git")
	base := filepath.Join(root, "base.git")
	sbx := filepath.Join(root, "sbx")
	for _, c := range [][]string{
		{"git", "init", "--bare", upstream},
		{"git", "clone", upstream, sbx},
	} {
		out, err := exec.Command(c[0], c[1:]...).CombinedOutput()
		require.NoError(t, err, string(out))
	}
	run := func(dir string, a ...string) {
		out, err := func() ([]byte, error) { c := exec.Command("git", a...); c.Dir = dir; return c.CombinedOutput() }()
		require.NoError(t, err, string(out))
	}
	run(sbx, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "--allow-empty", "-m", "init")
	run(sbx, "push", "origin", "HEAD:main")
	out, err := exec.Command("git", "clone", "--mirror", upstream, base).CombinedOutput()
	require.NoError(t, err, string(out))
	run(sbx, "checkout", "-b", "agent/x")
	run(sbx, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "--allow-empty", "-m", "work")
	run(base, "remote", "add", "sandbox-fake", sbx)

	ws := git.New(git.Spec{
		Name: "repo", Base: base, Remote: "origin", DefaultBranch: "main", AllowPush: true,
		PreSteps:     [][]string{{"git", "fetch", "{remote}", "+refs/heads/*:refs/heads/*"}},
		PublishSteps: [][]string{{"git", "fetch", "{sandbox_remote}", "+refs/heads/{branch}:refs/heads/{branch}"}, {"git", "push", "{remote}", "{branch}"}},
		Allowlist:    []string{"git"},
	})

	mgr := newTestManager(t)
	// The fake backend names the sandbox by spec.Name; force BackendName == "fake" so
	// {sandbox_remote} == "sandbox-fake". (newTestManager uses the fake backend; the
	// record's BackendName equals its swarm id. Override the remote name in the test
	// by registering the base remote under "sandbox-<backendName>".)
	rec, err := mgr.AdmitAndCreate(context.Background(), sandbox.CreateSpec{
		Agent: "shell", Clone: true, Branch: "agent/x", Workspaces: []sandbox.WorkspaceMount{{Name: "repo"}},
	})
	require.NoError(t, err)
	out, err = func() ([]byte, error) { c := exec.Command("git", "remote", "add", "sandbox-"+rec.BackendName, sbx); c.Dir = base; return c.CombinedOutput() }()
	require.NoError(t, err, string(out))

	svc := NewSandboxService(mgr, newTestOps(t))
	svc.SetGit(map[string]*git.Workspace{"repo": ws})

	_, err = svc.StopSandbox(context.Background(), &sbxv1.IdRequest{Id: rec.ID})
	require.NoError(t, err)

	// upstream received agent/x (publish ran before stop), and the sandbox is stopped.
	bo, _ := func() ([]byte, error) { c := exec.Command("git", "branch", "--list", "agent/x"); c.Dir = upstream; return c.CombinedOutput() }()
	require.Contains(t, string(bo), "agent/x")
	got, err := mgr.Get(context.Background(), rec.ID)
	require.NoError(t, err)
	require.Equal(t, "stopped", got.Status)
}
```

> If the fake backend's `Stop` does not flip status to `stopped`, assert on the publish side-effect (upstream has `agent/x`) only and note the status assertion depends on the fake. Keep the test deterministic against the real fake backend's behavior.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/apiserver/ -run TestStopSandbox_AutoPublishes -v`
Expected: FAIL (upstream lacks `agent/x` — StopSandbox doesn't publish yet).

- [ ] **Step 3: Wire on-stop publish** — modify `StopSandbox`:

```go
func (s *SandboxService) StopSandbox(ctx context.Context, r *sbxv1.IdRequest) (*sbxv1.Sandbox, error) {
	s.maybeAutoPublish(ctx, r.Id) // publish-then-stop: the sandbox-<name> fetch needs the live daemon
	if err := s.mgr.Stop(ctx, r.Id); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return s.GetSandbox(ctx, &sbxv1.GetSandboxRequest{Id: r.Id})
}

// maybeAutoPublish best-effort publishes the recorded branch of a clone-mode,
// push-allowed sandbox. Failures are audited + logged inside doPublish and do NOT
// block the caller (ADR: auto-publish is best-effort).
func (s *SandboxService) maybeAutoPublish(ctx context.Context, id string) {
	if s.gitWS == nil {
		return
	}
	rec, err := s.mgr.Get(ctx, id)
	if err != nil || len(rec.Spec.Workspaces) != 1 || !rec.Spec.Clone || rec.Spec.Branch == "" {
		return
	}
	ws := s.gitWS[rec.Spec.Workspaces[0].Name]
	if ws == nil || !ws.AllowPush() {
		return // not git-backed or pull-only: silent skip
	}
	if perr := s.doPublish(ctx, id, ""); perr != nil {
		slog.Warn("auto-publish failed", "sandbox", id, "err", perr)
	}
}
```

Add `"log/slog"` to imports.

- [ ] **Step 4: Wire agent-run publish_on_success** — in `AgentRun`, capture the flag and publish on success. Replace the success return inside the poll loop:

```go
	cmd, opts := r.Cmd, sandbox.ExecOpts{Workdir: r.Workdir, Env: r.Env}
	sbID := r.Id
	publishOnSuccess := r.PublishOnSuccess
	s.ops.Run(op.ID, func() (string, error) {
		did, derr := s.mgr.Backend().ExecDetached(context.Background(), name, cmd, opts)
		if derr != nil {
			return "", derr
		}
		for {
			st, perr := s.mgr.Backend().PollDetached(context.Background(), name, did)
			if perr != nil {
				return "", perr
			}
			if st.Done {
				if st.ExitCode != 0 {
					return sbID, status.Errorf(codes.Internal, "agent run exited %d", st.ExitCode)
				}
				if publishOnSuccess {
					s.maybeAutoPublish(context.Background(), sbID) // best-effort
				}
				return sbID, nil
			}
			time.Sleep(200 * time.Millisecond)
		}
	})
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/apiserver/ -run "TestStopSandbox_AutoPublishes|TestPublish" -v`
Expected: PASS (or SKIP without git).

- [ ] **Step 6: Commit**

```bash
git add internal/apiserver/sandboxservice.go internal/apiserver/sandboxservice_test.go
git commit -m "feat(apiserver): auto-publish on agent-run success + graceful stop (best-effort)"
```

---

## Task 9: node wiring + cross-node integration

**Files:**
- Modify: `internal/node/node.go`
- Test: add to `internal/membership/cluster_integration_test.go` (the multi-node `//go:build integration` harness), mirroring `TestCluster_ForwardSandboxRequest`

**Interfaces:**
- Consumes: everything above. Builds `gitWS map[string]*git.Workspace` from `cfg.Workspaces`; injects into `SandboxService` (`SetGit`/`SetAudit`/`SetEvents`), `InternalService`, and `attemptFor`.

- [ ] **Step 1: Build the gitWS map + inject into the service** — in `node.go`, after `sandboxes := apiserver.NewSandboxService(mgr, opsM)` and `auditLog := audit.New(...)`:

```go
	gitWS := buildGitWorkspaces(cfg.Workspaces)
	sandboxes.SetGit(gitWS)
	sandboxes.SetAudit(auditLog)
	sandboxes.SetEvents(bus)
```

Add the builder (near `workspaceNames`):

```go
func buildGitWorkspaces(ws []config.WorkspaceConfig) map[string]*git.Workspace {
	out := map[string]*git.Workspace{}
	for _, w := range ws {
		if w.Git == nil {
			continue
		}
		g := w.Git.WithDefaults()
		out[w.Name] = git.New(git.Spec{
			Name: w.Name, Base: w.HostPath, Remote: g.Remote, DefaultBranch: g.DefaultBranch,
			AllowPush: g.AllowPush, PreSteps: g.PreSteps, PublishSteps: g.PublishSteps, Allowlist: g.ExecAllowlist,
		})
	}
	return out
}
```

Add `"github.com/squall-chua/sbx-swarm-node/internal/git"` to imports.

- [ ] **Step 2: Inject gitWS into the local-attempt + internal-provision paths**

Change `attemptFor` to take `gitWS` and call `ProvisionLocal` instead of `mgr.AdmitAndCreate`:

```go
func attemptFor(self string, spec *sbxv1.CreateSandboxRequest, requestID string, mgr *sandbox.Manager, gitWS map[string]*git.Workspace, tbl *routing.Table, pool *peer.Pool, log *slog.Logger) coordinator.AttemptFunc {
	return func(ctx context.Context, nodeID string) (string, error) {
		if nodeID == self {
			rec, err := apiserver.ProvisionLocal(ctx, mgr, gitWS, apiserver.ToSpecForProvision(spec))
			if err == sandbox.ErrNoCapacity {
				return "", coordinator.ErrNack
			}
			if err != nil {
				return "", err
			}
			return rec.ID, nil
		}
		// ... unchanged remote path ...
	}
}
```

Update the `WithPlacement` call site to pass `gitWS`:

```go
		return coord.Provision(ctx, req, attemptFor(id.NodeID, spec, req.RequestID, mgr, gitWS, tbl, pool, log))
```

`ProvisionLocal` is already exported (Task 6), so node.go can call it directly.

Wire `gitWS` into `InternalService`: change its constructor and field, and the `apiserver.Build` call site:

```go
// provision.go
type InternalService struct {
	sbxv1.UnimplementedInternalServiceServer
	mgr      *sandbox.Manager
	gitWS    map[string]*git.Workspace
	cordoned func() bool
	dedup    *dedup
}

func NewInternalService(mgr *sandbox.Manager, gitWS map[string]*git.Workspace, cordoned func() bool) *InternalService {
	return &InternalService{mgr: mgr, gitWS: gitWS, cordoned: cordoned, dedup: newDedup(5*time.Minute, 1024)}
}
```

In `Provision`, replace `rec, err := s.mgr.AdmitAndCreate(ctx, spec)` with:

```go
	rec, err := ProvisionLocal(ctx, s.mgr, s.gitWS, spec)
```

(`ProvisionLocal`/`ProvisionLocal` already returns `ErrNoCapacity` unchanged from `AdmitAndCreate`, so the existing `== sandbox.ErrNoCapacity` branch still works; a bijection violation returns an `InvalidArgument` status error, surfaced to the coordinator as a hard error — correct, it is not a capacity NACK.)

Update the `apiserver.Build` call site:

```go
		Internal:  apiserver.NewInternalService(mgr, gitWS, func() bool { return tbl.IsCordoned(id.NodeID) }),
```

- [ ] **Step 3: Build + vet + full unit tests**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all PASS. (Fix the now-2-arg→3-arg `NewInternalService` call in any existing `provision_test.go` — pass `nil` for `gitWS`.)

- [ ] **Step 4: Integration test — publish forwards to the owner**

Add `TestCluster_PublishForwardsToOwner` to `internal/membership/cluster_integration_test.go` by **copying the structure of the existing `TestCluster_ForwardSandboxRequest`** in that file (same two-node harness, dial helpers, and `SandboxServiceClient` usage). The only behavioral change: after a sandbox is provisioned on owner A, call `PublishSandbox` against node B's client with A's sandbox id and assert it returns an `*Operation` (the forwarder routed it to A, which created the `git-publish` op).

With the fake backend there is no real git-backed workspace, so the op's terminal state will be `error` — that is expected. The assertion is on **routing + op creation across nodes**, not transport success:

```go
op, err := bClient.PublishSandbox(ctx, &sbxv1.PublishSandboxRequest{Id: ownerSandboxID})
require.NoError(t, err)          // the call forwarded and returned an Operation
require.NotEmpty(t, op.Id)
require.Equal(t, "git-publish", op.Type)
```

> This validates forwarding + op creation, not the git transport. The real clone→`sandbox-<name>`→fetch path is verified manually (env lacks docker/sbx), matching the m5-latents posture. If reproducing the exact harness setup is non-trivial, read `TestCluster_ForwardSandboxRequest` first and reuse its node-bring-up verbatim.

- [ ] **Step 5: Run integration + commit**

Run: `go test -tags integration ./...`
Expected: PASS.

```bash
git add internal/node/node.go internal/apiserver/provision.go internal/apiserver/provision_test.go internal/membership/cluster_integration_test.go
git commit -m "feat(node): wire git workspaces into provision + publish paths"
```

---

## Definition of done

- `go build ./... && go vet ./... && go test ./...` green; `go test -tags integration ./...` green.
- A git-backed workspace (`git:` block in config) can be provisioned with `clone:true` (PRE freshens the base; bijection enforced at the owner), the agent's branch publishes via the explicit RPC, on agent-run success, and on graceful stop (best-effort).
- `PublishSandbox` is admin-only, forwards to the owner, and is audited.
- Credentials are nowhere in the code (ADR-0014); no shell anywhere (ADR-0003).
- Standalone (no cluster) still boots and can provision/publish locally.

## Self-review — spec coverage

- internal/git builder/runner/workspace → Tasks 1–3 ✓
- GitConfig + defaults + validation → Task 4 ✓
- proto branch/PublishSandbox/Sandbox git fields + Go plumbing → Task 5 ✓
- clone⟺git-backed bijection (ADR-0015), at the owner → Task 6 ✓
- PRE under per-workspace lock spanning Create → Tasks 3 (`PreLock`) + 6 ✓
- explicit publish (git-publish op) + authz + forward + audit → Task 7 ✓
- branch resolution (request → recorded) + allow_push gate + running precheck + `FailedPrecondition` paths → Task 7 ✓
- best-effort + loud (audit + event) auto-publish on agent-run success + graceful stop → Task 8 ✓
- host-side credentials (no code) + `GIT_TERMINAL_PROMPT=0` → Task 3 (`gitEnv`) ✓
- node wiring + cross-node forwarding integration → Task 9 ✓
- **Deferred (documented):** real `sbx --clone`→`sandbox-<name>` fetch is verified manually (no docker/sbx in CI); the integration test covers forwarding + op creation only. Pattern/multi-branch publish, `{commit_message}`, reaper idle-stop auto-publish are out of scope per the spec.

## Open verification points (flag during execution, don't silently assume)

1. **`{sandbox_remote}` name:** the default publish step assumes `sbx --clone` registers a remote named exactly `sandbox-<BackendName>` on the mounted `host_path` repo, where `BackendName` is the swarm id (`<node>.<ULID>`, contains a dot). Confirm against the real `sbx` binary; if sbx sanitizes the name or uses a URL form, adjust the `{sandbox_remote}` derivation in `doPublish`.
2. **Fake-backend test status:** Task 8's `stopped`-status assertion depends on the fake backend's `Stop` flipping status; if it does not, assert only the publish side-effect.
