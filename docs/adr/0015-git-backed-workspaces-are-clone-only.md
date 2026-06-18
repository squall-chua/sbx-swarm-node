# Git-backed workspaces are clone-only

A git-backed workspace may be used only in clone mode, and a clone-mode provision must reference exactly
one git-backed workspace — a strict bijection enforced at provision. `clone:true` requires exactly one
workspace with a `git` config; a non-clone provision that references *any* git-backed workspace is rejected
with `InvalidArgument`. Both directions are checked; either violation is a hard error.

Why: a git-backed workspace's `host_path` is the swarm-owned bare/mirror base. `sbx --clone` mounts that
base **read-only** and clones it inside the sandbox, so the clone path can never let the agent write to the
base. But an ordinary bind-mount of the same workspace in a *non-clone* sandbox would expose the bare base
as an agent-writable directory, and a misbehaving or malicious agent could corrupt it — silently breaking
every future provision and publish for that workspace. Rejecting the non-clone case closes that hole and
makes "git-backed" and "clone" the same thing from the API's point of view.

Trade-off: you cannot mount a git-backed repo as a plain read-only "browse the code" workspace in a
non-clone sandbox; expose a separate, non-git workspace for that. The rejection is deliberate — recorded
here so it is not mistaken for a missing feature and "fixed" by relaxing the check.
