# Registered provider workspaces hold a node-side credential

A registered git-provider workspace carries its own credential in node config, alongside its
`remote_url`: either `token_env` (an env var name, for HTTPS) or `ssh_key_path` (for SSH), plus an
optional `ca_path` for an internal-CA or self-signed host. The credential is read once at boot into
an in-memory, per-workspace value and applied host-side to **both** the git transport (fetch of the
mirror base, and push on publish) and REST calls to the provider's API (PR/MR creation, lookup,
update). It is scoped per-workspace: two workspaces with two remotes hold two independent
credentials, and a token registered for one workspace is never usable against another.

This supersedes ADR-0014 **for registered provider workspaces only**: ADR-0014's ambient host-side
git config (a deploy key or credential helper the operator sets up outside of sbx-swarm-node) has no
way to feed a provider's REST `Authorization` header, and this feature requires the same credential
to authenticate both the git push and the PR/MR API call it drives. A vaulted, config-referenced
credential is the minimum mechanism that covers both surfaces. Non-provider git-backed workspaces
(no `remote_url`/provider config) are unaffected and keep relying on ADR-0014's ambient host config.

Why a config reference instead of extending ADR-0014's ambient-config model: git credential helpers
and SSH agents have no equivalent for an HTTP `Authorization` header, so REST access needs its own
credential path regardless. Given that a node-side credential must exist for REST, reusing it for
the git transport as well (instead of splitting across two mechanisms) keeps one workspace = one
credential, which is simpler and easier to reason about for leak-tightness than two independently
configured secrets per workspace. The credential is never gossiped, never placed in Sandbox-visible
state, never returned to the Agency, and never logged — it is applied to `git`'s process environment
and to a per-workspace `http.Client`, not to argv or any value that crosses the sandbox boundary or
the wire back to the control plane. This is still the ADR-0003 operator-trust model: the operator is
the trust authority for node-local config, same as the commands and (for non-provider workspaces)
the git credentials it already configures.

Trade-off: every node that hosts a given provider workspace must have its own `token_env` /
`ssh_key_path` (+ optional `ca_path`) configured by the operator — the same per-node credential setup
cost ADR-0014 already accepted, now carrying a config-declared secret reference instead of purely
ambient host git config.
