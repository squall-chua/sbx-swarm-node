# Wire-protocol compatibility and per-node capability filtering

The swarm supports **mixed-version nodes within a major version**. gRPC protobuf evolves
**additively only** (never renumber/remove fields); gossip push/pull state tolerates unknown fields; a
`protocol_version` rides in `NodeMeta`. A node joins only if its protocol **major** matches the
swarm's (loud alert otherwise); **minor skew is tolerated**, enabling rolling upgrades and
rejoin-after-upgrade.

Each node advertises a **capability set** derived from its sbx/SDK version (`clone`, `stats`, `logs`,
`structured_policy`, …); the scheduler adds a **capability predicate** to its filter (e.g. a clone-mode
request → clone-capable nodes only), turning a would-be runtime failure into a clean "no eligible
node."

Why: nodes drop out and rejoin (a stated requirement) and rolling upgrades mean heterogeneous
versions/daemons coexist. Without evolution-safe protocols the swarm fractures on upgrade; without
capability filtering the scheduler places work on a node whose daemon can't run it.

Considered: a homogeneous-swarm assumption (all nodes identical version/capabilities) — simpler, but
every upgrade becomes a full-swarm restart and rejoin-after-upgrade is unsafe.
