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

// String is a safe, log-friendly representation of a Result: Ref/DeliveryURL/
// ChangeID only. Result never carries a credential or token, so this is safe to
// log, but Patch is omitted (may be large/binary and isn't needed for identity).
func (r Result) String() string {
	return fmt.Sprintf("Result{Ref:%s DeliveryURL:%s ChangeID:%s}", r.Ref, r.DeliveryURL, r.ChangeID)
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

// String is a redacted representation of an Env: it deliberately omits RunEnv
// (which carries the base64 auth extraheader) and Cred (the token), so a stray
// %v/%+v on an Env can never leak the credential. Never widen it to include them.
func (e Env) String() string {
	return fmt.Sprintf("Env{Dir:%s Remote:%s RemoteURL:%s APIBase:%s Actor:%s}",
		e.Dir, e.Remote, e.RemoteURL, e.APIBase, e.Actor)
}

// actor is the audit actor, defaulting to "system" when unset (also the git
// identity name for the Gerrit squash commit).
func (e Env) actor() string {
	if e.Actor != "" {
		return e.Actor
	}
	return "system"
}

// remote is the configured upstream remote name in the base, defaulting to "origin".
func (e Env) remote() string {
	if e.Remote != "" {
		return e.Remote
	}
	return "origin"
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
	if _, err := r.Run(ctx, e.Dir, e.RunEnv, [][]string{{"git", "push", e.remote(), source + ":" + dest}}); err != nil {
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
