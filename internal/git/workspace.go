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
