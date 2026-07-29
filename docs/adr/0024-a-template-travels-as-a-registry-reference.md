# A template travels as a registry reference

A template that must run on more than one node is named by a registry reference, and each node's
daemon pulls it. The swarm never moves image bytes between nodes. Placement follows the reference: a
registry-shaped reference — the first path component contains a `.` or a `:`, or is `localhost` — is
eligible on every node, while a bare tag like `myimage:v1` is only eligible where it was saved.
Holding the image already is a tie-break, not a requirement.

Why: the daemon pulls remote references itself. Creating a sandbox with `-t
ghcr.io/nonexistent-org-xyz/nope:1` fails with `pull failed for image`, which means the pull was
attempted, and templates are images — `template.List` is `GET /docker/images`. Distribution was
therefore never a transport problem; the only thing refusing a registry-hosted template was our own
scheduler, which required a node to hold the exact reference already. Two lines fixed what looked
like a milestone.

Trade-offs: the rule is a heuristic on the reference's shape, so a private registry reachable from
only some nodes is assumed reachable from all. That failure is visible and local — a create error on
the chosen node, naming the pull — rather than a silent bad placement. Sharing a template also now
depends on infrastructure outside the swarm: an operator with no registry can save a template, but it
stays on the node that saved it, and placement will honestly refuse the other nodes.

Considered: exporting the image and streaming it peer to peer. `sbx template save SANDBOX TAG
--output FILE` produces a tar and `sbx template load FILE` imports it, so the raw material exists.
Rejected for what surrounds it — a streaming transfer path the node does not have, disk headroom for
a multi-gigabyte tar on both ends, and a story for a half-received image — when a registry solves the
same problem with infrastructure operators already run. Also considered: dropping the template filter
entirely, which turns a typo in a local tag into a create failure on some other node instead of an
honest refusal up front; and an explicit `pull` flag on the create request, which asks the caller to
restate what the reference already says.
