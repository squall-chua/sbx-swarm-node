# Git lifecycle commands are node-local config, never wire-borne

The pre and publish git command pipelines for a git-backed workspace are defined in each **node's local
config**, attached to that workspace, as argv-array steps run **without a shell**. The swarm protocol
and the public API never carry commands: a provision/publish request references a workspace **by name**
and may supply only validated *values* (branch, commit message), bound as discrete argv via a fixed
variable set (`{branch}`, `{base_ref}`, `{remote}`, `{sandbox_remote}`, `{commit_message}`). The owner
node runs its own configured pipeline.

Why: this removes remote code execution as a concern entirely. No API-key holder and no peer node (even
a compromised coordinator) can induce arbitrary command execution on a node — it can only trigger that
node's pre-configured pipeline with validated values. Per-repo customization (LFS, submodules, tags,
sparse-checkout) is achieved through local config, where the operator is already the trust authority.
An executable allowlist (default `git`, `git-lfs`) is defense-in-depth.

Trade-off: per-*task* command variation is not possible via the API; command changes require node
config — and, since workspaces are node-local, pipelines may legitimately differ per node for the same
workspace name. Accepted: per-repo command needs are an operator/config concern; per-task variation is
covered by validated values.
