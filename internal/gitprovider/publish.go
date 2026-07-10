package gitprovider

import (
	"context"
	"fmt"
	"strings"

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
	APIBase   string // REST base URL (GitHub/GitLab); "" for gerrit/plain
	Title     string // raw request title ("" => not supplied)
	Body      string // raw request body ("" => not supplied)
	Actor     string // audit actor, used as the git identity for the Gerrit squash
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

// tipSubject returns the first line of ref's tip commit message, falling back to
// the ref name if git fails. Used as the create-time title default (spec Q4).
func tipSubject(ctx context.Context, r *git.Runner, dir, ref string) string {
	res, err := r.Run(ctx, dir, nil, [][]string{{"git", "log", "-1", "--format=%s", ref}})
	if err != nil || len(res) == 0 {
		return ref
	}
	if s := strings.TrimSpace(string(res[len(res)-1].Output)); s != "" {
		return s
	}
	return ref
}
