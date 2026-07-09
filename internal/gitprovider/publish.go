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

// Patch is implemented in Task 8; this stub keeps PublishWork compiling.
func Patch(ctx context.Context, r *git.Runner, e Env, source, target string) (Result, error) {
	return Result{}, fmt.Errorf("todo")
}
