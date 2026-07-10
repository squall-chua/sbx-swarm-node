# Upstream git credentials are operator host-side config, not swarm-managed

> Superseded for registered provider workspaces by ADR-0019 (node-side credential feeds git + REST); non-provider git-backed workspaces are unaffected.

For a git-backed workspace, the credentials the node uses to reach the upstream remote — both the **PRE
fetch** and the **publish push** — are configured on the host by the operator who prepares the bare/mirror
base, using standard git mechanisms (a deploy SSH key, a credential helper, or an authenticated `origin`
URL). sbx-swarm-node never stores, injects, or manages an upstream token, and there are no credential
fields in workspace config. The runner sets `GIT_TERMINAL_PROMPT=0` so a missing/expired credential fails
fast instead of hanging.

Why: the alternative — in-process injection via `GIT_ASKPASS` plus a config-named secret (the original
design §12) — exists to keep the credential scoped, ungossiped, and out of logs. But standard host-side
git credential setup already achieves all of that: it is node-local, scoped per-remote (each bare repo has
its own `origin` URL / SSH identity), never gossiped or logged, and **invisible to the agent** — upstream
fetch/push run host-side, while the agent's in-container clone is sourced only from the local bind-mounted
base. So in-process token management is redundant complexity. This is the same trust model already accepted
for the *commands* in ADR-0003: the operator is the trust authority for node-local config.

Trade-off: there is no swarm-level place to set an upstream credential — every node that hosts a given
git-backed workspace must have its own host-side git auth configured by the operator. Accepted: workspaces
are node-local anyway, and per-node credential setup is an ordinary operator task. `allow_push` (config)
still gates whether a workspace may publish at all.
