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
