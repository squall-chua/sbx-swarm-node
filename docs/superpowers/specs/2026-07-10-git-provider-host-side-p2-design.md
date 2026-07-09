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

- `APIBase string` — the REST base URL (derivation below); unused by Gerrit.
- `Title string`, `Body string` — from the existing `PublishWorkRequest.title/body`.

PR and MR share one small internal `restClient{http *http.Client, base, token
string, provider Provider}`. Gerrit uses the `git.Runner` only. A `Strategy`
interface / registry is deliberately **not** introduced — five strategies in a
switch do not justify it. Add one only if a future provider needs runtime
registration.

### Per-strategy behavior

**PR / MR** — push then REST:

1. Push the source branch to origin, reusing P1's `Branch` push (`source:source`).
2. `restClient`:
   - GitHub: `GET /repos/{owner}/{repo}/pulls?head={owner}:{source}&base={target}&state=open`.
   - GitLab: `GET /projects/{url-encoded owner/repo}/merge_requests?source_branch={source}&target_branch={target}&state=opened`.
   - found → `PATCH` (GH) / `PUT` (GL) title+body, return its URL.
   - none → `POST` create with title+body.
3. `Result{Ref: "refs/heads/"+source, DeliveryURL: html_url/web_url}`.

Only **same-repo** is supported: the head branch is pushed to origin and the PR
head is `{owner}:{source}`. Cross-fork (head from a separate fork remote) is out
of scope.

**Gerrit** — push a review ref with a stable Change-Id:

1. In a temp worktree checked out at the bundled source tip:
   - if the tip commit message already has a `Change-Id:` trailer, keep it;
   - else `git commit --amend --no-edit --trailer "Change-Id: I<sha1(workspace \0 sandbox \0 source)>"`.
2. `git push origin HEAD:refs/for/<target>`.
3. Parse the change URL from push stderr (Gerrit prints it). `Result{ChangeID,
   DeliveryURL}`.

The Change-Id is deterministic in the workspace name, sandbox id, and source
branch, so a second publish amends the same change (a new patchset), not a new
one. The temp worktree keeps the amend off the mirror base's refs.

### Config + base-URL derivation

- One new **optional** workspace config field `api_base_url`. When set it wins.
- When unset, `gitprovider.APIBase(provider Provider, remoteURL string) string`
  derives it:
  - GitHub: `github.com` → `https://api.github.com`; any other host `H` →
    `https://H/api/v3` (GitHub Enterprise).
  - GitLab: host `H` → `https://H/api/v4` (public and self-hosted alike).
  - Gerrit: `https://H` (the Gerrit host; used only for URL parsing, no REST call).
  - Plain: `""` (REST strategies are unsupported on plain, gated upstream).
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
- Gate-before-mutation is preserved: an unsupported or (transitionally)
  unimplemented strategy is still rejected before the base is fetched/pushed.
- No retries: one attempt inside the existing publish timeout; fail-closed.

## Testing

- `httptest` fakes for GitHub and GitLab that assert **create-then-update**: the
  first publish `POST`s, the second `PATCH`/`PUT`s the same PR/MR — no duplicate.
- A generated self-signed cert served by the fake, trusted via `ca_path`, proving
  `CAPath` → `RootCAs` works and that a wrong/absent CA fails closed.
- A Gerrit fake = a bare repo accepting `refs/for/*`; assert one change across two
  pushes (stable Change-Id) and that a pre-existing Change-Id is respected.
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
