# Git Provider host-side support (Agency slice #20)

Status: approved design, ready for planning.

## Goal

Implement the node half of a pluggable Git Provider so real clones and real
publishes work against GitHub, GitLab, Gerrit, and plain remotes. The Agency
control plane already ships its half: the contract, the fakes, and the wiring.
The Agency carries **no** git secret — every credential and CA-trust decision is
the node's. The Agency passes only non-secret workspace names and strategy
selectors.

Two things to build:

1. **Registered named workspaces** (operator-facing) that hold the remote +
   credential + trust, one binding per workspace name.
2. A new synchronous **`PublishWork`** RPC, plus **clone-by-registered-name** on
   the existing `CreateSandbox`.

## What already exists (and stays untouched)

- Workspace-by-name resolution: `gitWS[mount.Name] -> *git.Workspace`.
- Declarative, shell-free pipeline model (`internal/git`, ADR-0003): argv steps
  from node config, validated values only, executable allowlist.
- A bare/mirror base per workspace with a per-workspace lock.
- Branch-only publish: async `PublishSandbox` + `doPublish` (bundle branch out of
  the live sandbox -> host file -> configured publish steps -> push).
- The clone <=> git-backed bijection (ADR-0015): clone mode requires exactly one
  git-backed workspace.

This feature adds a **parallel, imperative path** for the richer strategies. The
existing `publish_on_success` / async `PublishSandbox` branch-only recovery push
is left as-is; `PublishWork` is additive.

## The wire contract (already called by the Agency, `sbxswarm.v1`)

New RPC:

```proto
rpc PublishWork(PublishWorkRequest) returns (PublishResult) {
  option (google.api.http) = {post: "/v1/sandboxes/{id}/git/publish-work" body: "*"};
}

message PublishWorkRequest {
  string id = 1;       // sandbox id; SOURCE BRANCH = the sandbox's recorded branch
  string strategy = 2; // branch|patch|pull_request|merge_request|gerrit_change
  string target = 3;   // branch: push dest; PR/MR: base branch; gerrit: refs/for/<target>
  string title = 4;    // PR/MR title
  string body = 5;     // PR/MR body
}

message PublishResult {
  string ref = 1;          // pushed ref (refs/heads/... or refs/for/...)
  string delivery_url = 2; // PR/MR/Change URL; EMPTY for a plain branch push
  string change_id = 3;    // gerrit only
  bytes  patch = 4;        // patch strategy only
}
```

`PublishWork` is synchronous — block until the push / API call completes
(seconds-scale). The Agency calls it while the sandbox is still alive and turns
the result into an artifact.

Clone-by-name extends the existing `CreateSandbox`, which already carries
`repeated WorkspaceMount workspaces` and `bool clone`, where
`WorkspaceMount{ string name; bool read_only }`.

## Design decisions

> Refined during a grilling session (2026-07-09). The nine resolutions are folded
> into the sections below; the notable divergences from the original task text are
> flagged inline (source branch, Gerrit mechanism).

### ADR-0020 (new): the node auto-manages the mirror base from remote_url

The clone source stays a host-side bare/mirror repo the sandbox mounts read-only
(ADR-0015) — the credential never enters the sandbox; it only ever touches the
host-side fetch. The change from ADR-0014's world: the node **creates and
initializes** that base itself. On first clone (base dir missing/empty) the node
runs `git clone --mirror <remote_url>` host-side with the vaulted credential + CA
into a node-managed data dir (e.g. `<data>/git-workspaces/<name>.git`); thereafter
it `fetch`es (the existing PRE pipeline). The operator supplies only
`remote_url` + credential — no manual base prep. `host_path` becomes **optional**
for a provider workspace (defaults to the managed dir).

### ADR-0019 (new): registered provider workspaces hold a node-side credential

ADR-0014 says "no credential fields — use ambient host-side git config." That
cannot feed a GitHub/GitLab **REST** `Authorization` header, and this task
requires per-workspace vaulted credentials + CA trust applied to both git
transport and REST. ADR-0019 supersedes ADR-0014 **for registered provider
workspaces**: the node holds a per-workspace credential (via `token_env` or
`ssh_key_path`) plus an optional `ca_path`, applied host-side to both the git
transport and REST calls. Never gossiped, never placed in Sandbox-visible state,
never returned to the Agency, never logged. Config-declared, so still the
ADR-0003 operator-trust model (the operator is the trust authority for
node-local config).

### Credential surface: file/env refs in config, 1:1 per workspace

Each workspace entry holds its own remote **and** its own credential + CA, side by
side. No global/shared credential. Two workspaces = two remotes = two credentials.

```yaml
workspaces:
  - name: acme-app
    git:
      remote_url: https://github.com/acme/app   # HTTPS or SSH
      provider: github        # github|gitlab|gerrit|plain, or "" to derive from URL
      default_branch: main
      token_env: ACME_GH_TOKEN     # HTTPS credential (env var name; read once at boot)
      # ssh_key_path: /etc/sbx/acme.key   # SSH credential (alternative to token)
      ca_path: /etc/sbx/acme-ca.pem     # optional internal-CA / self-signed PEM

  - name: internal-svc
    git:
      remote_url: ssh://git@gerrit.corp.internal:29418/svc
      provider: gerrit              # explicit: not host-derivable (see Provider derivation)
      ssh_key_path: /etc/sbx/gerrit.key
      ssh_known_hosts_path: /etc/sbx/gerrit_known_hosts   # optional; pins the SSH host key
      ca_path: /etc/sbx/corp-ca.pem
```

- Secret material is referenced, not inlined: `token_env` (env var name),
  `ssh_key_path`, `ssh_known_hosts_path`, `ca_path` (file paths). No plaintext
  token/PEM in the main YAML.
- `ca_path` is **optional** (TLS only): omit for public hosts (system trust covers
  them), provide only for an internal CA / self-signed **HTTPS** host. It does
  nothing for SSH — see host-key handling below.
- `host_path` is **optional** for a provider workspace (the node auto-manages the
  mirror base, ADR-0020).
- Existing `remote` / `pre_steps` / `publish_steps` / `allow_push` stay for
  back-compat; the new fields are additive. `allow_push` still gates writes.
- Read once at boot into an in-memory, per-workspace
  `Credential{ Token, SSHKeyPath, CAPath string }`.

### Correlation (how workspaces map to credentials at runtime)

The workspace name is the single key that resolves the remote **and** its matching
credential:

- **Clone** — `CreateSandbox` carries `WorkspaceMount{name}`; resolve
  `name -> workspace`; clone that workspace's remote with that workspace's
  credential + CA. An empty/absent name keeps today's default behavior.
- **PublishWork** — resolve the sandbox record -> the workspace it was cloned from
  (`rec.Spec.Workspaces[0].Name`, ADR-0015 guarantees exactly one) -> publish with
  the same credential + CA + derived provider. A GitHub token can never leak onto a
  GitLab/Gerrit push because they are separate workspace entries resolved
  independently by name.

One workspace = one remote (clone-from and publish-to are the same remote).
Cloning from A but publishing to a different fork B is out of scope.

### Auth / trust application (host-side, no shell)

- **HTTPS token** -> injected via `GIT_CONFIG_COUNT` / `GIT_CONFIG_KEY_n` /
  `GIT_CONFIG_VALUE_n` env (git >= 2.31) setting
  `http.<url>.extraheader=Authorization: ...`. Keeps the token out of argv / `ps`.
- **SSH key** -> `GIT_SSH_COMMAND="ssh -i <key> -o IdentitiesOnly=yes ..."`.
- **SSH host key** -> if `ssh_known_hosts_path` is set: `-o StrictHostKeyChecking=yes
  -o UserKnownHostsFile=<path>` (pinned). If absent: `-o StrictHostKeyChecking=accept-new`
  (trust-on-first-use, pins the first key seen) with a one-line warning logged.
  **Never** `StrictHostKeyChecking=no`.
- **CA (HTTPS)** -> `GIT_SSL_CAINFO=<ca_path>` for git; `tls.Config.RootCAs` for REST.
  TLS verification is never disabled.
- **REST** -> a per-workspace `*http.Client` built with that CA pool + the
  `Authorization` header.

The existing `git.Runner` already accepts an `env []string`; the workspace computes
the credential-env in one place and passes it in.

### `internal/gitprovider` (new package)

The declarative ADR-0003 pipeline cannot express REST calls or Change-Id logic, so
a new imperative package carries the provider behavior:

- `Derive(remoteURL, explicit) -> Provider`. **Explicit `provider:` is
  authoritative** — it always wins. Derivation is best-effort for obvious public
  hosts **only**: host `== github.com` -> github; host contains `gitlab` -> gitlab;
  **everything else -> `plain`** (never guess github/gitlab/gerrit for an arbitrary
  internal host). Self-hosted GitLab/Gerrit therefore *require* an explicit
  `provider:`; we fail loud rather than mis-derive.
- `plain` supports only `branch` + `patch`. `pull_request`/`merge_request`/
  `gerrit_change` against a `plain` (or wrongly-defaulted) workspace hit the same
  "unsupported strategy for provider" error, whose message tells the operator to
  set `provider:` explicitly.
- Per-strategy functions: `branch`, `patch`, `pull_request` (GitHub REST),
  `merge_request` (GitLab REST), `gerrit_change` (git-only, no REST).
- Each validates that the derived provider supports the requested strategy, else a
  clear error the Agency surfaces as-is (e.g. `gerrit_change on github:
  unsupported`).

### REST base URL (self-hosted / enterprise)

Derived from the remote **host**, independent of transport (an SSH `remote_url`
still yields an HTTPS API host) — required for the `ca_path` self-hosted case:

- **GitHub:** host `== github.com` -> `https://api.github.com`; else (Enterprise)
  -> `https://<host>/api/v3`.
- **GitLab:** `https://<host>/api/v4` (same for gitlab.com and self-hosted).
- **Gerrit:** no REST client. See the gerrit strategy below.

The REST client carries the per-workspace CA pool + `Authorization` header.
- Idempotency: PR/MR "find by head -> update in place, else create" (no duplicate);
  gerrit ensures a stable `Change-Id` trailer so a re-publish lands as a new
  patchset on the same Change (no duplicate Change).

### `PublishWork` handler (in `SandboxService`, next to `PublishSandbox`)

- Add `PublishWork` to `sandbox.proto`, regenerate.
- Synchronous; blocks until done. **Requires a live sandbox** for all strategies
  (it bundles the source branch out of the running sandbox); sandbox gone ->
  `FailedPrecondition`.
- **Source branch** (never caller-supplied): read live HEAD in the sandbox
  (`git rev-parse --abbrev-ref HEAD`). If HEAD is a real branch (not detached, not
  `"HEAD"`) -> **live HEAD**. If detached/undeterminable -> **recorded branch**
  (`rec.Spec.Branch`, captured at clone). Live HEAD wins so the agent's actual work
  is published even if it switched branches; the recorded branch is the
  detached-HEAD safety net. (This refines the original task's "always the recorded
  branch": the caller still never supplies it, but the sandbox's own HEAD takes
  precedence over the recorded name.)
- Reuse the existing `bundleBranches` to pull the source branch out of the live
  sandbox into the node-managed base under the per-workspace lock, then dispatch to
  the provider strategy (a temporary worktree off the base for strategies that
  rewrite a commit, i.e. gerrit).
- Map result -> `PublishResult{ ref, delivery_url, change_id, patch }`.

### Clone-by-name (mostly exists)

Resolution by mount name already works. The deltas:

- The PRE fetch / base clone uses the **vaulted credential-env + CA** instead of
  ambient host git config.
- The workspace `default_branch` is recorded as `rec.Spec.Branch` (the source
  branch PublishWork will use).

## Per-strategy behavior

Source branch is always the sandbox's recorded branch. Reject a strategy the
derived provider does not support with a clear error.

- **branch** — `git push origin <recorded>:<target || recorded>`. Return `ref`;
  `delivery_url` empty.
- **patch** — `git format-patch` host-side; return bytes in `PublishResult.patch`.
  No remote write. Safe at Job-terminal time because the git-backed workspace
  survives Sandbox teardown.
- **pull_request** (GitHub) / **merge_request** (GitLab) — `target` (the base
  branch) is **required**; empty -> `InvalidArgument` before any push. Push the
  source branch first, then via REST: derive `owner/repo` from the `remote_url`
  path; look up an **open** PR/MR with `head = source-branch` AND `base = target`.
  If found -> PATCH title/body (the commits are already current from the push;
  no duplicate). If none open -> create. A **merged/closed** PR/MR is *not*
  reopened — a new one is created (and fails cleanly with the provider's own error
  if there are no new commits). Return the PR/MR URL in `delivery_url`.
- **gerrit_change** — no REST client. Read HEAD's commit message host-side; if it
  already carries a `Change-Id:` trailer, keep it, else `git commit --amend`
  (message-only) to inject a **deterministic** `Change-Id:
  I<sha1hex(workspace \0 sandbox-id \0 source-branch)>` — keyed on identity, not
  commit content, so it is stable across re-publish and across added commits.
  (Git hooks are never cloned, so the sandbox has no `commit-msg` hook and the
  commit normally lacks a trailer; amend host-side is the only mechanism — a hook
  can't run on an already-created commit.) Then `git push HEAD:refs/for/<target>`.
  Gerrit prints the Change URL + number on the push's stderr; parse it. Re-publish
  with the same `Change-Id` lands as a new patchset on the same Change (no
  duplicate). Return `ref = refs/for/<target>`, `delivery_url` = the URL from
  stderr, `change_id` = the `Change-Id` trailer (the stable id, not the Change
  number).

## Result mapping (cross-check with the Agency)

- `ref` -> artifact Path
- `delivery_url` -> artifact Content (PR/MR/Change URL; empty for a plain branch push)
- `patch` (non-empty) -> a file artifact
- `change_id` -> returned to the API caller only (gerrit)

## Testing bar (both)

Hermetic tests are the always-on CI bar; an env-gated real smoke gives manual
high-fidelity verification.

Hermetic (CI, no network/tokens):

- `httptest` servers stand in for the GitHub / GitLab / Gerrit REST APIs.
- Local bare repos over `file://` and a real SSH transport for clone/push.
- A generated self-signed cert + its CA PEM exercises `ca_path`.
- Gerrit faked as a bare repo accepting `refs/for/*` + a `Change-Id` assertion.

Coverage:

- clone-by-registered-name over HTTPS and SSH, including a self-signed host via
  `ca_path`;
- branch and patch on a plain remote;
- pull_request on GitHub incl. update-in-place (second publish updates, no
  duplicate);
- merge_request on GitLab likewise;
- gerrit_change incl. stable Change-Id (second publish = new patchset, no duplicate
  Change);
- provider-mismatch rejection (gerrit_change on GitHub errors);
- **leak test** (the security bar): register the hermetic workspace with sentinel
  token/SSH-key/CA values; run clone-by-name + every publish strategy **and one
  forced git failure** (error paths leak most). Assert the sentinel appears in
  **none** of: (1) the returned `PublishResult` (incl. `patch` bytes), (2) emitted
  events, (3) audit records, (4) the persisted/gossiped Sandbox record, (5)
  captured logs (incl. the failure's error string). Plus a structural check that
  the credential reaches git only via `cmd.Env`, never argv; and that the clone
  succeeds while the sandbox never receives the token.

Env-gated real smoke (`//go:build integration`, skipped in CI): real
GitHub/GitLab/Gerrit behind env tokens.

## Non-goals / boundaries

- Never send any credential, token, SSH key, or CA material back to the Agency.
- Never take a source branch from the caller — it is the sandbox's own state
  (live HEAD, or the recorded branch when HEAD is detached), never a caller field.
- In-VM per-job branch selection is out of scope; check out the workspace default
  branch and publish the recorded branch.
- One workspace = one remote; clone-from-A publish-to-B is out of scope.
- Note: the Agency currently sends a registered workspace name only for durable
  Agents and workspace-bearing Jobs (clone-mode). A deferred Agency follow-up
  rejects `Workspace != "" && !Durable`. Does not affect this side.

## Phasing

- **P1** (green, zero external network): ADR-0019 + ADR-0020; config surface
  (incl. `ssh_known_hosts_path`); credential-env plumbing (HTTPS token, SSH key +
  host-key handling, CA); node auto-managed mirror base + clone-by-name; source-
  branch resolution (live HEAD / recorded fallback); `PublishWork` proto + handler;
  `branch` + `patch` strategies; provider derivation + mismatch rejection; leak test.
- **P2**: `pull_request` / `merge_request` / `gerrit_change` incl. update-in-place;
  REST clients; gerrit Change-Id; the `httptest` matrix + env-gated real smoke.
