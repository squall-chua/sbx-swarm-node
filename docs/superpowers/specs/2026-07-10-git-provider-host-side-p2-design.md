# Git Provider host-side — Phase 2 (PR / MR / Gerrit)

Status: approved design, ready for planning.

Follows Phase 1 (`2026-07-09-git-provider-host-side-design.md`, merged shape on
`feat/git-provider-host-side`). P1 built the credential, mirror base, `PublishWork`
RPC, and the `branch`/`patch` strategies, and gated the three REST-driven
strategies as `Unimplemented` before touching the base. P2 fills those in.

## Goal

Implement the three remaining publish strategies:

- `pull_request` (GitHub) — push branch, then REST create-or-update a PR.
- `merge_request` (GitLab) — push branch, then REST create-or-update an MR.
- `gerrit_change` (Gerrit) — push `HEAD:refs/for/<target>` with a `Change-Id`
  trailer; no REST for the change itself.

All three re-publish **in place**: publishing the same sandbox's work twice to the
same target updates one PR/MR/change, never duplicates it.

## What already exists (P1, do not redesign)

- `internal/gitprovider`: `Derive`/`Supports` (github/gitlab/gerrit/plain),
  `Branch` + `Patch` free functions, `Env{Dir,RunEnv,Remote,RemoteURL,Cred}`,
  `Result{Ref,DeliveryURL,ChangeID,Patch}`.
- `git.Credential{Token,SSHKeyPath,SSHKnownHostsPath,CAPath}` and `.Env(remoteURL)`
  — the token feeds git transport as a base64 extraheader. **P2 reuses the same
  `Token` for the REST `Authorization` header and the same `CAPath` for TLS.**
- `apiserver.PublishWork`: resolves the provider, gates unsupported /
  unimplemented strategies **before** mutating the base, resolves the source
  branch (live HEAD else recorded), ensures the mirror base, bundles the source
  branch out of the live sandbox into the base, and holds the workspace lock
  across the strategy (fetch+push atomic). The strategy switch dispatches
  `branch`/`patch` today.
- `PublishResult` proto already carries `delivery_url` and `change_id`.
- Leak test: token never appears across the outward surfaces (error, event,
  audit, log, result), including its base64 form.

## Design

### Shape

No new abstraction. Three free functions added to `internal/gitprovider`,
dispatched from the existing `publish_work.go` switch:

- `PullRequest(ctx, r *git.Runner, e Env, source, target string) (Result, error)`
- `MergeRequest(ctx, r *git.Runner, e Env, source, target string) (Result, error)`
- `GerritChange(ctx, r *git.Runner, e Env, source, target string) (Result, error)`

`Env` gains three fields the new paths need:

- `APIBase string` — the REST base URL (GitHub/GitLab only; empty for Gerrit).
- `Title string`, `Body string` — from the existing `PublishWorkRequest.title/body`.

The idempotency key `(workspace, source, target)` is available from the workspace
`RemoteURL()` plus the `source`/`target` args — no sandbox id is threaded in.

PR and MR share one small internal `restClient{http *http.Client, base, token
string, provider Provider}`. Gerrit uses the `git.Runner` only. A `Strategy`
interface / registry is deliberately **not** introduced — five strategies in a
switch do not justify it. Add one only if a future provider needs runtime
registration.

### Idempotency key + preconditions (checked before the base is mutated)

- **Idempotency key** = `(workspace, source, target)`, sandbox-independent
  (ADR-0021). All three strategies deliver exactly one PR / MR / change per key;
  a re-publish updates it in place. GitHub already enforces this for PRs
  (`state=open` on `(head, base)`); Gerrit gets it from the derived Change-Id.
- **PR / MR require an HTTPS token.** If `cred.Token == ""` (an SSH-only
  workspace), reject with `FailedPrecondition: REST strategy requires an HTTPS
  token credential`. Gerrit is exempt — it delivers over `git push` and works with
  whichever transport the workspace uses.
- **PR / MR parse the repo identity from `remote_url` up front** (rules below); an
  unparseable remote is rejected with `InvalidArgument` before any base mutation.
- Existing P1 gates still run first: provider `Supports(strategy)`, `AllowPush`
  for every non-`patch` strategy, and the source-branch resolution.

### Per-strategy behavior

**PR / MR** — push then REST:

1. Push the source branch to origin, reusing P1's `Branch` push (`source:source`).
2. `restClient` (repo identity parsed from `remote_url` — see below):
   - GitHub: `GET /repos/{owner}/{repo}/pulls?head={owner}:{source}&base={target}&state=open`.
   - GitLab: `GET /projects/{url-encoded project path}/merge_requests?source_branch={source}&target_branch={target}&state=opened`.
   - **found** → update title/body **only for the fields the request set non-empty**
     (Q4), `PATCH` (GH) / `PUT` (GL); return its URL.
   - **none** → `POST` create; `title` = request title or the source tip subject if
     empty (Q4); `body` = request body (may be empty).
3. `Result{Ref: "refs/heads/"+source, DeliveryURL: html_url/web_url}`.

Repo identity is parsed per provider from `remote_url` (both `https://host/…` and
`git@host:…` forms, trailing `.git` stripped):
- **GitHub:** first two path segments → `{owner}/{repo}`.
- **GitLab:** the *whole* remaining path, URL-encoded, is the project (handles
  nested subgroups `group/subgroup/repo`).
- Fewer than 2 segments (GitHub) or empty path (GitLab) → early
  `InvalidArgument: cannot parse owner/repo from remote_url`.

Only **same-repo** is supported: the head branch is pushed to origin and the PR
head is `{owner}:{source}`. Cross-fork (head from a separate fork remote) is out
of scope.

**Gerrit** — push one squashed change with a stable Change-Id:

Gerrit creates one change *per commit* pushed to `refs/for/<target>`, and every
such commit must carry its own `Change-Id` trailer or the whole push is rejected.
To deliver "one change per `(workspace, source, target)`" (the Q1 unit) we squash
the branch to a single snapshot commit:

1. In a temp worktree off the base, build one commit holding `source`'s tree
   parented on `target`'s tip: `git commit-tree source^{tree} -p <target> -m <msg>`.
   - `<msg>` = request `title`/`body`, or the source tip subject if empty (Q4),
     with the `Change-Id` trailer appended.
   - `Change-Id = I<sha1(remoteURL \0 source \0 target)>` — deterministic in the
     idempotency key, **not** the sandbox, so a re-publish lands a new patchset on
     the same change.
   - Inject a git identity for the commit (`GIT_AUTHOR_*`/`GIT_COMMITTER_*`): name
     = the audit actor resolved in `PublishWork` (`"system"` fallback), email =
     a fixed placeholder (`noreply@sbx-swarm.local`). The base is a bare repo with
     no `user.email`, so without this `commit-tree` errors.
2. `git push origin <commit>:refs/for/<target>`.
3. `Result{ChangeID, DeliveryURL}`. `ChangeID` is always the injected value.
   `DeliveryURL` is best-effort: parse the first `remote: <url>` line from push
   stderr; if the format doesn't match (Gerrit versions vary), leave it empty.

The snapshot is a tree copy, not a merge, so it never conflicts. A pre-existing
`Change-Id` on the source tip is not consulted — the squash message is authoritative
and its Change-Id is derived from the key.

### Config + base-URL derivation

- One new **optional** workspace config field `api_base_url`. When set it wins.
- When unset, `gitprovider.APIBase(provider Provider, remoteURL string) string`
  derives it:
  - GitHub: `github.com` → `https://api.github.com`; any other host `H` →
    `https://H/api/v3` (GitHub Enterprise).
  - GitLab: host `H` → `https://H/api/v4` (public and self-hosted alike).
  - Gerrit / Plain: `""` — Gerrit delivers over `git push` (no REST), and plain has
    no REST strategy. `api_base` is a GitHub/GitLab-only concept.
- REST auth uses the **same token** with a per-provider header: GitHub
  `Authorization: Bearer <token>`; GitLab `PRIVATE-TOKEN: <token>`.
- TLS trust: `cred.CAPath` → `tls.Config.RootCAs`; empty → system roots.
  `httpClient(cred)` builds this once per publish.

### Leak bar + error mapping

- The P1 leak bar extends to every REST surface: the token (and its base64 form)
  must never appear in an error, `delivery_url`, event, audit entry, or log.
  `restClient` wraps failures as `HTTP <status>: <provider message>` — never the
  request, headers, or URL query.
- HTTP → gRPC status: 401/403 → `PermissionDenied`, 404 → `FailedPrecondition`,
  422 → `InvalidArgument`, 5xx → `Unavailable`, other non-2xx → `Internal`.
- Gate-before-mutation is preserved: unsupported strategy, missing token (PR/MR),
  or unparseable remote are all rejected before the base is fetched/pushed.
- No retries: one attempt inside the existing publish timeout; fail-closed. Safe
  under partial failure — if the branch push succeeds but the REST call fails, a
  re-publish re-pushes idempotently, finds no open PR/MR, and creates one.
- The result does **not** distinguish created vs updated (same URL either way); add
  a flag only if the Agency asks for it.

## Testing

- `httptest` fakes for GitHub and GitLab that assert **create-then-update**: the
  first publish `POST`s, the second `PATCH`/`PUT`s the same PR/MR — no duplicate.
- A generated self-signed cert served by the fake, trusted via `ca_path`, proving
  `CAPath` → `RootCAs` works and that a wrong/absent CA fails closed.
- A Gerrit fake = a bare repo accepting `refs/for/*`; assert one squashed change
  across two pushes (stable derived Change-Id), and that a multi-commit source
  still produces exactly one change.
- Extend the credential leak test to the REST paths (create, update, and a forced
  REST error), asserting the token and its base64 form never leak.
- `//go:build integration` real smoke behind env tokens (`GH_*`, `GL_*`,
  `GERRIT_*`), skipped in CI (no docker/sbx runner).

## Out of scope

- Cross-fork PR/MR (head from a fork remote).
- Retries / backoff on REST failures.
- Gerrit REST (change status, labels, submit) — only the review-ref push.
- Reopen-if-closed: an update targets the open PR/MR; a closed/merged one is not
  reopened (a fresh publish with no open PR/MR creates a new one).
