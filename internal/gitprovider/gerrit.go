package gitprovider

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/squall-chua/sbx-swarm-node/internal/git"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const gerritIdentityEmail = "noreply@sbx-swarm.local"

var gerritURLRe = regexp.MustCompile(`https?://\S+`)

// GerritChange squashes source into one commit parented on target and pushes it
// to refs/for/<target> with a deterministic Change-Id, so a re-publish lands a
// new patchset on the same change (idempotent per workspace/source/target).
func GerritChange(ctx context.Context, r *git.Runner, e Env, source, target string) (Result, error) {
	if target == "" {
		return Result{}, status.Error(codes.InvalidArgument, "gerrit_change requires a target branch")
	}
	// Reuse an existing Change-Id from the source history (a review-head sandbox
	// carries the original Patchset's trailer) so the re-push lands a new Patchset
	// on the same Change instead of a duplicate; otherwise derive a stable one.
	changeID := existingChangeID(ctx, r, e.Dir, target, source)
	if changeID == "" {
		changeID = gerritChangeID(e.RemoteURL, source, target)
	}

	subject := e.Title
	if subject == "" {
		subject = tipSubject(ctx, r, e.Dir, source)
	}
	msg := subject
	if e.Body != "" {
		msg += "\n\n" + e.Body
	}
	msg += "\n\nChange-Id: " + changeID

	actor := e.actor()
	// The base is a bare repo with no user.email; commit-tree needs an identity.
	ident := []string{
		"GIT_AUTHOR_NAME=" + actor, "GIT_AUTHOR_EMAIL=" + gerritIdentityEmail,
		"GIT_COMMITTER_NAME=" + actor, "GIT_COMMITTER_EMAIL=" + gerritIdentityEmail,
	}
	env := append(append([]string{}, e.RunEnv...), ident...)

	made, err := r.Run(ctx, e.Dir, env, [][]string{
		{"git", "commit-tree", source + "^{tree}", "-p", target, "-m", msg},
	})
	if err != nil {
		return Result{}, err
	}
	commit := strings.TrimSpace(string(made[len(made)-1].Output))

	pushed, err := r.Run(ctx, e.Dir, env, [][]string{
		{"git", "push", e.remote(), commit + ":refs/for/" + target},
	})
	if err != nil {
		return Result{}, err
	}
	out := pushed[len(pushed)-1].Output
	return Result{
		Ref: "refs/for/" + target, ChangeID: changeID,
		DeliveryURL: parseGerritURL(out),
		// Gerrit refuses an empty/duplicate push with "no new changes" — the
		// no_change seam (#23-d). ponytail: text match; Gerrit has no porcelain here.
		NoChange: strings.Contains(string(out), "no new changes"),
	}, nil
}

var changeIDRe = regexp.MustCompile(`(?m)^Change-Id: (I[0-9a-fA-F]+)\s*$`)

// existingChangeID returns the Change-Id trailer from source's history above
// target (the original Patchset commit carries it; fix commits on top do not).
// "" if none — e.g. a fresh non-review branch, where the caller derives one.
func existingChangeID(ctx context.Context, r *git.Runner, dir, target, source string) string {
	res, err := r.Run(ctx, dir, nil, [][]string{{"git", "log", "--format=%B", target + ".." + source}})
	if err != nil || len(res) == 0 {
		return ""
	}
	if m := changeIDRe.FindSubmatch(res[len(res)-1].Output); m != nil {
		return string(m[1])
	}
	return ""
}

// gerritChangeID derives Gerrit's Change-Id from the deliverable key
// (workspace remote, source, target) — stable across re-publishes, independent
// of the sandbox (ADR-0021).
func gerritChangeID(remoteURL, source, target string) string {
	h := sha1.Sum([]byte(remoteURL + "\x00" + source + "\x00" + target))
	return "I" + hex.EncodeToString(h[:])
}

// parseGerritURL extracts the change URL Gerrit prints on push stderr
// (a `remote:` line). Best-effort: "" if the output has no recognizable URL.
func parseGerritURL(pushOutput []byte) string {
	for line := range strings.SplitSeq(string(pushOutput), "\n") {
		if !strings.Contains(line, "remote:") {
			continue
		}
		if m := gerritURLRe.FindString(line); m != "" {
			return strings.TrimRight(m, " \t\r")
		}
	}
	return ""
}
