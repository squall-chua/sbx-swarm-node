# sbx-swarm-node

A decentralized control plane for Docker sandboxes: each node wraps a host's local sandbox daemon and
gossips with peers to place and manage sandboxes without a central controller.

## Language

**Node**:
A single host running the sbx-swarm-node process alongside its local `sandboxd` daemon; the unit of
membership in a swarm.
_Avoid_: server, instance, machine

**Node key**:
A per-node keypair generated on first run. Its public key is pinned across the swarm via gossip, and
its hash forms the `node_id` — giving each node a self-certifying identity.
_Avoid_: cert, certificate

**Revoke / Revoked** (node):
An operator action that places a Node key's `node_id` on a swarm-wide denylist, permanently rejecting
that node's authentication across the swarm (eventually consistent). A revoked node can no longer make
node-to-node calls; it returns only by generating a new Node key. Distinct from Cordon, which merely
stops new placements on a still-trusted node.
_Avoid_: ban, block, evict (evicting a revoked node from routing is separate and deferred)

**Swarm**:
The set of nodes that gossip together as one peer-to-peer group, identified by a Swarm ID. A node can
run solo (a swarm of one) or join others.
_Avoid_: cluster, fleet

**Swarm ID**:
A stable UUID identifying a swarm across membership changes and partitions, distinct from the join
secret. Minted by the first node; adopted and persisted by joiners.
_Avoid_: cluster id

**Swarm name**:
A human-readable, non-unique label for a swarm, used for operator display.

**Cluster secret**:
The shared key that encrypts gossip and gates who may join a swarm. It is the auth boundary, not the
swarm's identity.
_Avoid_: gossip key, password

**Sandbox**:
A disposable, isolated micro-VM managed by a node's local daemon, in which agents or commands run.
_Avoid_: container, box

**Workspace**:
A named host directory a node advertises for mounting or cloning into sandboxes. Identified by logical
name; the backing data is operator-managed and node-local.
_Avoid_: mount point, volume

**Provision**:
To place and create a sandbox on a chosen node (maps to the SDK's `Create`). The swarm never uses the
SDK's interactive `Run`.
_Avoid_: run, launch, spin up

**Operation**:
An asynchronous, tracked unit of work against the swarm (provision, stop, remove, agent run, git
publish). Clients receive an operation id and observe progress through events.
_Avoid_: job, task, action

**Agent run**:
A headless agent execution inside a sandbox, launched via the SDK's `ExecDetached` and tracked as an
Operation.
_Avoid_: run (bare), job

**Unreachable** (sandbox state):
The sandbox's owner node is suspect or dead per gossip, so its true state is unknown — it may still be
alive behind a partition. A peer's non-destructive guess; only the owner ever moves it off this state.
_Avoid_: lost, dead, offline

**Lost** (sandbox state):
Owner-confirmed gone: the owner rejoined and reconciliation found the sandbox absent from its daemon.
Terminal; triggers capacity reclaim and cleanup. Only the owner declares it.
_Avoid_: unreachable, missing

**Git-backed workspace**:
A workspace whose `host_path` is a bare/mirror git repo the swarm owns exclusively. Provisioning clones
it into the sandbox (SDK `WithClone`), and the owner node runs the workspace's configured pre/publish
pipelines around the sandbox lifecycle.

**Publish**:
The post-sandbox step that retrieves the agent's branch from the sandbox clone and pushes it upstream,
executed via the workspace's configured, node-local pipeline.
_Avoid_: post, push (bare)

**Template**:
A reusable sandbox base image saved locally on a node (SDK `SaveTemplate`). A node advertises the
templates it holds; provisioning that requests a template is filtered to nodes that have it. The swarm
does not move templates between nodes (v1).
_Avoid_: image (bare), base

**Blocked egress**:
A distinct (host, sandbox) pair the egress proxy denied, surfaced as a security audit view with
synthesized first/last-seen timestamps. Attempt frequency is not available from the SDK (v0.1.2).
_Avoid_: block count, denied request (as a rate)

**Placement constraint**:
A hard predicate a node must satisfy to be eligible for a sandbox — required workspaces, template,
capabilities, node-label affinity/anti-affinity, free capacity, and not cordoned. Constraints filter the
candidate set; a Placement strategy then ranks the survivors. Affinity matches a node's own labels
(`zone`, `rack`, `gpu`, …), not the sandbox's labels — the two are distinct namespaces (wire fields
`node_affinity`/`node_anti_affinity`).
_Avoid_: filter (bare), selector, rule, pod-affinity (no sandbox-to-sandbox affinity exists)

**Placement strategy**:
The scoring rule that ranks the nodes passing every Placement constraint — least-loaded (default),
bin-pack, spread, or least-actual-load. The first three score on **reserved** allocation; least-actual-load
scores on **actual** gossiped CPU/mem utilization (packs by real usage, not reservations). Round-robin is
intentionally excluded (no honest semantics without a shared cursor in a leaderless swarm). Label affinity
is a Placement constraint, not a strategy.
_Avoid_: scheduling policy, algorithm
