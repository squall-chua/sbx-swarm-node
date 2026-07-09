package git

import (
	"context"
	"os"
	"path/filepath"
	"sync"
)

// Spec is the resolved config for one git-backed workspace (built in node.go from
// config; git stays config-package-agnostic).
type Spec struct {
	Name          string
	Base          string // host_path: the bare/mirror base repo
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

// Workspace orchestrates the clone-mode git lifecycle for one git-backed
// workspace, serializing operations on its bare base with a per-workspace lock.
type Workspace struct {
	spec   Spec
	runner *Runner
	mu     sync.Mutex
}

// New builds a Workspace orchestrator.
func New(s Spec) *Workspace { return &Workspace{spec: s, runner: NewRunner(s.Allowlist)} }

func (w *Workspace) Name() string          { return w.spec.Name }
func (w *Workspace) AllowPush() bool       { return w.spec.AllowPush }
func (w *Workspace) RemoteURL() string     { return w.spec.RemoteURL }
func (w *Workspace) Provider() string      { return w.spec.Provider }
func (w *Workspace) DefaultBranch() string { return w.spec.DefaultBranch }
func (w *Workspace) Cred() Credential      { return w.spec.Cred }
func (w *Workspace) Base() string          { return w.spec.Base }

// RemoteName returns the configured upstream remote name in the base, defaulting
// to "origin".
func (w *Workspace) RemoteName() string {
	if w.spec.Remote != "" {
		return w.spec.Remote
	}
	return "origin"
}

// env disables interactive credential prompts so a missing/expired host-side
// credential fails fast instead of hanging (ADR-0014: creds are host-side).
func gitEnv() []string { return []string{"GIT_TERMINAL_PROMPT=0"} }

// credEnv returns the credential env for the workspace remote, plus the base
// GIT_TERMINAL_PROMPT guard. Errors only on a malformed credential.
func (w *Workspace) credEnv() ([]string, error) { return w.spec.Cred.Env(w.spec.RemoteURL) }

// EnsureBase creates the mirror base from RemoteURL on first use (ADR-0020). No-op
// if the base already has a git dir, or if RemoteURL is empty (legacy operator-
// prepared base). Runs under the workspace lock with the credential env. The
// clone's remote.origin.mirror flag is cleared afterward (keeping the all-refs
// mirror fetch refspec) so downstream refspec pushes (Publish, gitprovider.Branch)
// aren't rejected by a mirror-mode origin.
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
		[][]string{
			{"git", "clone", "--mirror", w.spec.RemoteURL, w.spec.Base},
			{"git", "-C", w.spec.Base, "config", "remote.origin.mirror", "false"},
		})
	return err
}

// PreLock locks the workspace and runs the PRE pipeline (freshen the bare base
// from upstream). On success it returns an unlock func — the caller MUST hold the
// lock across the clone-sourcing Create so a concurrent PRE (which may prune)
// cannot race the clone-read — then call unlock. On PRE error it unlocks and
// returns the error.
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

// Publish locks the workspace and runs the PUBLISH pipeline (fetch the branch
// from the sandbox remote, push it upstream). Self-contained (no spanning).
func (w *Workspace) Publish(ctx context.Context, branch, sandboxRemote string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	env := gitEnv()
	if ce, err := w.credEnv(); err == nil {
		env = append(ce, env...)
	}
	vars := Vars{Branch: branch, Remote: w.spec.Remote, BaseRef: w.spec.DefaultBranch, SandboxRemote: sandboxRemote}
	argv, err := Build(w.spec.PublishSteps, vars)
	if err != nil {
		return err
	}
	_, err = w.runner.Run(ctx, w.spec.Base, env, argv)
	return err
}

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
