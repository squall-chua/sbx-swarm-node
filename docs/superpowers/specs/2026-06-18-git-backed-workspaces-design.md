# Git-backed workspaces (clone mode) — design

**Date:** 2026-06-18 · **Status:** approved (brainstorm) · **Supersedes:** the relevant parts of
`docs/superpowers/plans/2026-06-15-sbx-swarm-node-m6-git-workspaces.md` (predates M5/security/operability;
its in-process credential design is dropped here).

Realizes §12 of `docs/superpowers/specs/2026-06-15-sbx-swarm-node-design.md` against current code, with
two changes from that spec: (1) upstream credentials are operator host-side git config, not swarm-managed
(ADR-0014); (2) auto-publish targets the branch recorded at provision (ADR-0015).

## Goal

Implement the clone-mode git lifecycle: provision a sandbox on a **private in-container clone** of a
git-backed workspace (`sbx --clone`), the node freshens the base from upstream **before** cloning (PRE),
the agent works in its private clone, and the node **publishes** the agent's branch upstream — via
**node-local, shell-free, argv-step pipelines** (ADR-0003), serialized by a **per-workspace lock**, with
the credential as a host-side operator concern and an audit trail. The agent sandbox stays credential-free.

## Verified feasibility (the crux)

`sbx create --clone` mounts the workspace's `host_path` **read-only**, clones it in-container, and exposes
the agent's commits to the host via a **`sandbox-<name>` git remote** (wired by a git-daemon). Confirmed
from the installed `sbx` binary help:

> `--clone`: "Run the agent on a private in-container clone of the host Git repository (mounted
> read-only) … the agent's commits are accessible via the `sandbox-<name>` git remote on the host"

So the publish transport in the original §12 (`git fetch sandbox-<name> <branch>` host-side, then
`git push origin <branch>`) is the **native** path. A `git bundle` + SDK `CopyFrom` alternative was
considered and rejected — it reinvents what `sbx` already provides. **The host-side fetch from
`sandbox-<name>` requires the sandbox to be running** (the git-daemon is tied to the live sandbox); this
single fact drives the publish ordering throughout.

## Architecture & components

New package **`internal/git/`** — declarative, shell-free git pipelines (ADR-0003):

| File | Responsibility |
|---|---|
| `builder.go` | `Build(steps [][]string, v Vars) ([][]string, error)` — substitute validated `{branch}`/`{base_ref}`/`{remote}`/`{sandbox_remote}` into each argv token; reject injection. |
| `runner.go` | `Runner{allow map[string]bool}`; `Run(ctx, dir, env, steps) ([]StepResult, error)` — exec argv via `os/exec` (no shell), allowlist-gated, stop-on-error, capture combined output. |
| `workspace.go` | `Workspace` per git-backed workspace; **per-workspace mutex**; `Pre(ctx, vars, steps)` and `Publish(ctx, vars, steps)` run pipelines in the bare base dir. |

**Bound values** `Vars{Branch, BaseRef, Remote, SandboxRemote}` (sources, not all request-supplied):
- `{branch}` — request (`PublishSandbox.branch`) or the recorded `Record.Branch` (set from
  `CreateSandboxRequest.branch`). The **only** request-supplied value; validated as a ref.
- `{remote}` — `GitConfig.Remote` (config).
- `{sandbox_remote}` — `"sandbox-" + <sandbox name>` (derived).
- `{base_ref}` — `GitConfig.DefaultBranch` (config); available for custom pipelines (e.g. open a PR into
  base). Default pipelines don't reference it.

`{commit_message}` from the original §12 var set is **deferred** — no default pipeline uses it and no
request field carries it; add it with a request field when a custom pipeline needs it (YAGNI).

No `creds.go` — see Credentials/Security below. The runner sets `GIT_TERMINAL_PROMPT=0` so a
missing/expired credential fails fast instead of hanging on a prompt.

### Config

Extend `WorkspaceConfig` (`internal/config/config.go`) with an optional `Git *GitConfig`:

```go
type WorkspaceConfig struct {
    Name     string     `yaml:"name"`
    HostPath string     `yaml:"host_path"`
    ReadOnly bool       `yaml:"read_only"`
    Git      *GitConfig `yaml:"git,omitempty"`
}

type GitConfig struct {
    Remote        string     `yaml:"remote"`         // default "origin"
    DefaultBranch string     `yaml:"default_branch"` // default base_ref
    AllowPush     bool       `yaml:"allow_push"`
    PreSteps      [][]string `yaml:"pre_steps"`      // default: refs-only fetch (below)
    PublishSteps  [][]string `yaml:"publish_steps"`  // default: fetch sandbox-<name> + push (below)
    ExecAllowlist []string   `yaml:"exec_allowlist"` // default ["git","git-lfs"]
}
```

A workspace is **git-backed** iff `Git != nil`; its `host_path` is the bare/mirror base. There is no
`bare` flag (presence of `Git` is the signal) and **no auth fields**.

Defaults filled when unset:
- `Remote = "origin"`; `ExecAllowlist = ["git","git-lfs"]`.
- `PreSteps = [["git","fetch","{remote}","+refs/heads/*:refs/heads/*"]]`.
- `PublishSteps = [["git","fetch","{sandbox_remote}","{branch}"],["git","push","{remote}","{branch}"]]`.

`config.Validate`: a git-backed workspace needs a non-empty `host_path`. At boot the node checks
`host_path` exists; git-repo-ness surfaces at first PRE with a clear error (avoids a redundant probe).

### Proto (`proto/sbxswarm/v1/sandbox.proto`)

- `CreateSandboxRequest`: add `string branch = 13;` (recorded branch — the only new request field).
  `{base_ref}` comes from config `default_branch`, so no request field is added for it.
- New RPC: `rpc PublishSandbox(PublishSandboxRequest) returns (Operation)` →
  `POST /v1/sandboxes/{id}/git/publish` (body `*`).
  `PublishSandboxRequest { string id = 1; string branch = 2; }` (branch optional → recorded branch).
- `Sandbox` (the GET view): add `string branch = 6;` and `string last_publish = 7;`.
- `AgentRunRequest.publish_on_success` (field 5) **already exists** — this milestone wires it up.

Codegen: edit `.proto` → `buf generate` (repo root) → commit regenerated `internal/gen/sbxswarm/v1/*`.

### Wiring touch-points (reuse existing patterns)

- `internal/apiserver/forward.go` — register the `PublishSandbox` reply type so an id-bearing request
  routes to the **owner node** automatically (existing OwnerProxy forwarder).
- `internal/apiserver/authz.go` — classify `PublishSandbox` as **mutating** (admin-only) or
  `TestAuthz_AllMethodsClassified` fails.
- `internal/ops` — new `"publish"` operation type; reuses op tracking, the `RecoverInterrupted` boot
  sweep, and the M3 audit log.
- `internal/node/node.go` — build one `git.Workspace` per git-backed workspace at boot; inject into
  `SandboxService`.
- Sandbox `Record` (`internal/sandbox`) — store `Branch` (at provision) and `LastPublish`.

## Data flow

### A. Provision (clone mode) — owner node, in `CreateSandbox`

1. **Validate** clone-mode constraints (§12): exactly **one** workspace and it must be git-backed
   (`Git != nil`); reject otherwise (`InvalidArgument`). Non-clone provisions are unchanged.
2. **Record** the request's `branch` on the sandbox `Record` (`base_ref` is config-sourced).
3. **PRE** (under the workspace lock): `git.Workspace.Pre` runs `PreSteps` in the bare base — refs only,
   no working tree.
4. **Create**: `backend.Create(WithWorkspace(<bare host_path>), WithClone())` (both already wired in the
   SDK adapter). `sbx` mounts the base read-only, clones it in-container, and wires the host
   `sandbox-<name>` remote.

**Locking:** the per-workspace mutex is held across **PRE + the Create clone-sourcing** so a concurrent
provision's PRE-write cannot race this one's clone-read (§12).
`// ponytail: per-workspace lock spans PRE+Create; narrow to the clone-read window only if same-workspace
provision throughput ever matters.`

### B. Publish — owner node (forwarder routes by id), op type `"publish"`

1. Resolve sandbox → workspace → `GitConfig`. Reject if not git-backed or `allow_push=false`
   (`FailedPrecondition`).
2. **Resolve branch:** request branch (explicit) → else recorded `Record.Branch`. Empty →
   `FailedPrecondition`.
3. **Precheck running state** (explicit path): a stopped sandbox → `FailedPrecondition`
   ("sandbox not running; cannot reach sandbox-<name>") rather than a cryptic fetch error.
4. Under the workspace lock, `git.Workspace.Publish` runs `PublishSteps` in the bare base (default:
   `git fetch sandbox-<name> {branch}` then `git push {remote} {branch}`). `{sandbox_remote}` is
   `sandbox-<sandbox-name>`.
5. **Audit** (workspace/branch/remote/outcome — never secrets); set `Record.LastPublish`.

### C. Triggers (all converge on one internal `publishSandbox` helper)

- **Explicit** — `PublishSandbox` RPC; branch from request or recorded.
- **Agent-run success** — in `AgentRun`, after the detached exec exits `0`, if `publish_on_success`:
  publish the **recorded** branch. Best-effort (see below).
- **On graceful stop** — in `StopSandbox`, if clone-mode + `allow_push` + a recorded branch is present:
  **publish first, then stop** (the `sandbox-<name>` fetch needs the live git-daemon). No recorded branch
  → silent skip (audited as no-op), not an error.

## Security

Removing remote code execution is the milestone's purpose:

- **No shell, ever.** `exec.CommandContext(argv[0], argv[1:]...)`. The builder rejects request values with
  a leading `-`, control chars, `..`, or anything outside `[A-Za-z0-9._/\-]`. `{branch}` is the only
  request-supplied value, so it is the only one needing ref validation.
- **Commands come only from node config** (ADR-0003). The wire carries a workspace *name* + validated
  *values*, never argv. No API-key holder and no peer (even a compromised coordinator) can induce
  arbitrary execution on a node — only that node's pre-configured pipeline with validated values.
- **Exec allowlist** (default `git`, `git-lfs`) — the runner refuses any other binary. Defense in depth.
- **Credentials** are operator host-side git config (ADR-0014): never in our process, gossip, or logs; the
  **agent sandbox never sees them** (push is host-side). `GIT_TERMINAL_PROMPT=0` fail-fast.
- **authz:** `PublishSandbox` is mutating → admin-only; node-key peers cannot trigger publish. Every git
  op is audited (workspace/branch/remote/outcome, never secrets).

## Error handling & edges

- **PRE fails** → provision fails before `Create`; no sandbox created; op errors cleanly.
- **Explicit publish on a stopped sandbox** → `FailedPrecondition` (running-state precheck).
- **`allow_push=false` / non-git-backed workspace** → `FailedPrecondition` before any git runs.
- **Auto-publish is best-effort:** a failed `publish_on_success`/on-stop publish is audited and reflected
  in the op, but does **not** roll back the agent run or block the stop. Explicit publish surfaces the
  error directly.
- **Crash mid-publish** → the boot sweep marks the `publish` op `error` (log-only, M3). Re-publish is safe:
  `git fetch`/`push` of the same branch is idempotent.
- **Standalone node** (no cluster) → publish runs locally (owner = self); git workspaces have **no cluster
  dependency** (honors the standalone-must-work invariant).
- **Concurrent publishes, same workspace** → serialized by the per-workspace lock (per-workspace, not
  per-branch). `// ponytail:` per-branch locking only if it ever matters.

## Testing

- **Unit (no git):** builder substitution + injection rejection; runner allowlist + stop-on-error (via
  `echo`/`false`); `GitConfig` defaults + validation.
- **Git-level (needs `git`, skip if absent):** `workspace_test` builds a real upstream → mirror base →
  "agent branch" and asserts PRE freshens the base and PUBLISH pushes the branch (the agent's clone stood
  in by a push to the base).
- **apiserver (fake `git.Workspace` via interface):** `PublishSandbox` branch resolution + `allow_push`
  gate + `FailedPrecondition` paths + op type `"publish"` + audit called; `AgentRun`
  publish-on-success-exit-0 path; `StopSandbox` **publish-then-stop** ordering.
- **Auto-covered:** the `authz` drift-guard test forces `PublishSandbox` classification.
- **`-tags integration`:** publish **forwarding to owner** with a fake backend.

**Honest limitation (documented, not hidden):** the *real* `sbx --clone` → `sandbox-<name>` git-daemon
fetch needs docker + the `sbx` binary, absent in CI. That exact transport is verified by the git-level
stand-in + a **manual** check — same posture as the m5-latents deferred integration tests.

**Verification bar:** `go build/vet/test ./...`; run the `-tags integration` suite (publish changes
cross-node behavior); ff-merge to main locally; Opus whole-branch review before merge.

## ADRs

- **ADR-0014** (definite): *Upstream git credentials are operator host-side git config, not
  swarm-managed.* Overrides §12's in-process `GIT_ASKPASS`/`auth_secret_ref` approach. Rationale: standard
  host-side git credential setup (deploy SSH key, credential helper, authenticated `origin` URL) is
  already node-local, per-remote-scoped, ungossiped, and invisible to the agent (push is host-side) — so
  in-process token management is redundant. Same trust model as ADR-0003 (operator owns node-local config).
- **ADR-0015** (decide during grill): *Auto-publish targets the branch recorded at provision.* Captures
  the recorded-vs-pattern-vs-trigger choice and the best-effort policy for auto-triggers.

## Scope boundary

| In | Out / deferred (explicit) |
|---|---|
| `internal/git` (builder, runner, workspace) | Reaper idle-stop auto-publish → **M7** |
| `GitConfig` + defaults + validation | In-process credential injection (dropped, ADR-0014) |
| proto: `branch`, `PublishSandbox`, git fields on `Sandbox` | Clone + extra bind-mount workspaces (§12 defers vs sbx) |
| clone-mode PRE + single-workspace constraint | Pattern/multi-branch publish (chose recorded-branch, ADR-0015) |
| publish: explicit + agent-run + on-stop (owner-local) | Cross-node workspace data replication (standing non-goal) |
| `"publish"` op + audit + crash-recovery (reuse) | Automated CI of the real sbx clone fetch (env lacks docker/sbx) |

## Glossary impact (CONTEXT.md)

`Git-backed workspace` and `Publish` are already defined and remain accurate. Add a note that the upstream
credential is operator host-side config (no swarm-managed secret). No new terms otherwise.
