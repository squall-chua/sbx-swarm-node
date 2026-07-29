# Kit references are node config, never request input

A kit is named in a create request and resolved to a reference by node config. The request carries
kit **names** only; a kit **reference** — a local directory, a ZIP, or an OCI reference — is never
accepted over the API. An unknown name fails create — asynchronously, as `op.Error`, since
`CreateSandbox` returns an `Operation`; in the swarm path it usually fails earlier still, as
`ErrNoEligibleNode`, because no candidate node advertises the name at all.

Why: `sbx kit add` takes a reference, so the obvious API is to accept one. Two things make that a
bad trade. A local reference is a host filesystem path, and letting a client name a path on the
node's disk is the exposure `workspaceResolver` was built to avoid — the same reason a workspace is
addressed by logical name. And the daemon records a sandbox's kit list verbatim, then re-resolves
the whole list on every later `kit add`, resolving a relative path against its own working
directory rather than the caller's. The add still reports success, so a wrong path stays invisible
until an unrelated later add fails and that sandbox can then take no kit at all. A reference
arriving over the network is one hop further from the filesystem that would make it correct.

Keeping references in config removes the hazard rather than guarding against it. The node also
never calls `AddKit`, so there is no later add for the daemon to re-resolve, and the SDK makes a
local reference absolute when it builds the argument vector.

Trade-off: a caller cannot use a kit the operator has not declared, so a new kit needs a config
edit and a node restart — the same as a template, a workspace, or a git workspace today. A kit
name is also only as trustworthy as the operator's config: node A's `my-tools` and node B's
`my-tools` may point at different references, and the scheduler filters on name alone. That is
accepted, and it is the exposure Workspace names already carry. Comparing references across nodes
was considered and rejected: the same OCI tag can be re-pushed and two copies of a path can
differ, so the check would prove a string matched, not that the content did.

This ADR's concern is inbound: a client naming a host path in a request. A reference can still
leave the node outbound, and that is not a violation of it. A kept-but-unloadable kit fails at
create, and the CLI's own error text — which can contain the absolute path or the OCI reference —
becomes `op.Error` (`internal/ops/ops.go`). `ListOperations` is classified as a read method
(`internal/apiserver/authz.go`), so a `read-only` principal can see it. This is not new: the same
class of disclosure already exists for git and workspace errors.
