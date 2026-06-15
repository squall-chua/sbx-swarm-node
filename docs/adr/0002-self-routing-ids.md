# Self-routing IDs for v1

Sandbox and operation IDs embed the responsible node's ID as a routing prefix:
`sandbox_id = <ownerNodeID>.<ulid>`, `op_id = <coordinatorNodeID>.<ulid>`. Any node routes a request
by reading the prefix and forwarding via the membership address table — no gossiped lookup required,
and operations need no gossip at all. The gossiped `sandbox_id → owner` map remains the authority and
the `lost`-cleanup index; the prefix additionally closes the post-create gossip-propagation race.

Why: v1 has no sandbox migration, so encoding placement in the ID is harmless, and it removes the
scatter-query / retry-on-miss machinery that opaque IDs would otherwise require — especially for
short-lived operations, which are not gossiped.

Trade-off / reversal: IDs are longer and leak placement, and would "lie" if sandbox migration is added
later. When migration arrives, switch to opaque ULIDs routed via the (already-present) owner map and
treat the prefix as a legacy hint.

Considered: opaque ULIDs + gossiped owner map + scatter-query/retry-on-miss — cleaner, migration-safe
IDs, but requires a fan-out query for correctness on a map miss and a separate location mechanism for
operations.
