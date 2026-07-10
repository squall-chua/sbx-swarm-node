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
	APIBaseURL    string // REST API base override (GitHub/GitLab); "" => derive
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
func (w *Workspace) APIBaseURL() string    { return w.spec.APIBaseURL }

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

// runEnv is the environment for a git child run in the base: the credential env
// (which already carries the GIT_TERMINAL_PROMPT guard), falling back to the bare
// guard if the credential is malformed.
func (w *Workspace) runEnv() []string {
	env, err := w.credEnv()
	if err != nil {
		return gitEnv()
	}
	return env
}

// EnsureBase creates the base from RemoteURL on first use (ADR-0020). No-op if the
// base already exists, or if RemoteURL is empty (legacy operator-prepared base).
// Runs under the workspace lock with the credential env.
//
// The base is a NON-BARE clone with a DETACHED HEAD. It must serve two masters:
// `sbx --clone` requires a working tree (it rejects a bare repo), while the
// server-side PRE fetch (+refs/heads/*:refs/heads/*) and Publish's fetch-into-base
// must update refs/heads/* without hitting "refusing to fetch into checked-out
// branch". Detaching HEAD leaves no branch checked out, satisfying both. A plain
// (non-mirror) clone also keeps a normal origin, so refspec pushes (Publish,
// gitprovider.Branch) are not rejected the way a mirror origin would reject them.
func (w *Workspace) EnsureBase(ctx context.Context) error {
	if w.spec.RemoteURL == "" {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// No-op if a repo is already there: our own non-bare clone (Base/.git) or an
	// operator-prepared bare base (Base/HEAD).
	if _, err := os.Stat(filepath.Join(w.spec.Base, ".git")); err == nil {
		return nil
	}
	if _, err := os.Stat(filepath.Join(w.spec.Base, "HEAD")); err == nil {
		return nil
	}
	env, err := w.credEnv()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(w.spec.Base), 0o755); err != nil { // node-managed root may not exist yet
		return err
	}
	if _, err := w.runner.Run(ctx, "", env,
		[][]string{{"git", "clone", w.spec.RemoteURL, w.spec.Base}}); err != nil {
		return err
	}
	// Detach HEAD so no branch is checked out. If the clone landed on an unborn
	// branch (upstream's default HEAD points at a branch with no commits), there's
	// nothing to detach and nothing checked out to conflict with a later fetch, so
	// leave it as-is.
	if _, err := w.runner.Run(ctx, "", env,
		[][]string{{"git", "-C", w.spec.Base, "rev-parse", "--verify", "-q", "HEAD"}}); err != nil {
		return nil // unborn HEAD (empty checkout) — safe to leave
	}
	_, err = w.runner.Run(ctx, "", env,
		[][]string{{"git", "-C", w.spec.Base, "checkout", "--detach"}})
	return err
}

// PreLock locks the workspace and runs the PRE pipeline (freshen the bare base
// from upstream). On success it returns an unlock func — the caller MUST hold the
// lock across the clone-sourcing Create so a concurrent PRE (which may prune)
// cannot race the clone-read — then call unlock. On PRE error it unlocks and
// returns the error.
func (w *Workspace) PreLock(ctx context.Context, branch string) (func(), error) {
	w.mu.Lock()
	env := w.runEnv()
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
	env := w.runEnv()
	vars := Vars{Branch: branch, Remote: w.spec.Remote, BaseRef: w.spec.DefaultBranch, SandboxRemote: sandboxRemote}
	argv, err := Build(w.spec.PublishSteps, vars)
	if err != nil {
		return err
	}
	_, err = w.runner.Run(ctx, w.spec.Base, env, argv)
	return err
}

// FetchFromBundle locks the workspace, fetches branch from a git bundle file into
// the base, and returns the unlock func (like PreLock) so the caller runs its push
// strategy against the base WHILE STILL HOLDING THE LOCK — fetch+push must be atomic
// on a shared base (a concurrent publish or PRE prune must not interleave). On fetch
// error it unlocks and returns the error.
func (w *Workspace) FetchFromBundle(ctx context.Context, branch, bundlePath string) (func(), error) {
	w.mu.Lock()
	env := w.runEnv()
	if _, err := w.runner.Run(ctx, w.spec.Base, env,
		[][]string{{"git", "fetch", bundlePath, "+refs/heads/" + branch + ":refs/heads/" + branch}}); err != nil {
		w.mu.Unlock()
		return nil, err
	}
	return w.mu.Unlock, nil
}
