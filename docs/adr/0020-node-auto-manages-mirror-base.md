# The node auto-manages the mirror base for provider workspaces

For a registered provider workspace, the node creates and owns the host-side bare/mirror base
itself, instead of requiring the operator to prepare it. On first use (the base directory missing or
empty) the node runs `git clone --mirror <remote_url>` host-side, using that workspace's vaulted
credential and `ca_path` (ADR-0019), into a node-managed data directory (e.g.
`<data>/git-workspaces/<name>.git`); on every subsequent use it `fetch`es, reusing the existing PRE
pipeline. `host_path` becomes **optional** for a provider workspace — when omitted, it defaults to
the node-managed directory. The sandbox still mounts that base **read-only** and clones it inside the
container (ADR-0015 unchanged): the credential only ever touches the host-side fetch/push, never the
sandbox.

Why: ADR-0014's world assumed an operator-prepared `host_path` — someone runs `git clone --mirror`
by hand before the workspace is usable. Once the node holds a vaulted credential per workspace
(ADR-0019), it already has everything it needs to create the base itself, so requiring a manual
prep step adds an operational hop with no corresponding safety benefit: the same credential and the
same `git clone --mirror` command run either way, the only difference is who types it. Auto-managing
the base also keeps `remote_url` + credential as the single source of truth for a provider workspace
— there is no separate host-side state that can drift out of sync with config (wrong remote, stale
mirror, missing base after a node rebuild).

Trade-off: the node now owns base creation and its on-disk layout for provider workspaces, rather
than an operator-prepared `host_path` the operator fully controls. An operator who wants to seed a
base from an existing local clone, or place it on a specific volume, must still set `host_path`
explicitly — omitting it commits to the node's managed directory and layout.

## P1 status

`EnsureBase` is implemented, unit-tested, and **wired into both runtime call sites**: it runs before
the PRE fetch in `ProvisionLocal` (sandbox create) and before the bundle in `PublishWork`. A provider
workspace with `remote_url` and no `host_path` is therefore created on first use from the node-managed
directory — no operator-prepared base is required. (P2 adds the PR/MR/Gerrit REST strategies; the
base mechanism itself is complete in P1.)

## Amendment: the base is a non-bare detached checkout, not a bare mirror

Originally `EnsureBase` ran `git clone --mirror` (a bare repo), matching how a server-side
fetch/push base is normally built. That breaks the clone-mode contract: the node hands the base
**directly to `sbx --clone`** as the primary workspace, and `sbx --clone` requires a working tree —
it rejects a bare repo (`--clone requires a Git repository, but <base> is not in a Git repository`).

One base must serve two masters: a working tree for `sbx --clone`, and a repo the server side can
`fetch +refs/heads/*:refs/heads/*` / `push` into. The fix is a **non-bare clone with a DETACHED
HEAD** (`git clone <remote_url> <base>` then `git checkout --detach`). A working tree satisfies
`sbx --clone`; a detached HEAD leaves no branch checked out, so the PRE fetch and Publish's
fetch-into-base update `refs/heads/*` without the "refusing to fetch into checked-out branch" error.
A plain (non-mirror) clone also keeps a normal `origin`, so refspec pushes are not rejected the way
a `--mirror` origin would reject them (no `remote.origin.mirror` to clear). The path `<name>.git` is
kept for continuity even though the repo is no longer bare.
