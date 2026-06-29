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

**Terminal session**:
A live, TTY-attached, bidirectional exec stream into a running Sandbox, opened via the SDK's interactive
attach (`ExecInteractive` with a pseudo-TTY). Distinct from Exec (one-shot, captured output) and Agent run
(headless, detached). It is **not** an Operation — there is no operation id and no tracked async progress;
it is an ephemeral live stream, like the event firehose. Does not violate Provision's "never interactive
`Run`" rule: that forbids interactive *provisioning* of a sandbox, whereas a Terminal session attaches to a
command inside an already-Provisioned Sandbox.
_Avoid_: console, shell, ssh, session (bare)

**File transfer**:
A synchronous copy of a single file between an operator and a Sandbox — **upload**
(operator → sandbox) or **download** (sandbox → operator). Admin-only. Like a Terminal
session it is **not** an Operation: no operation id, no tracked async progress.
_Avoid_: file API, sync, transfer job, cp (bare)

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

**Activity** (sandbox):
What resets a sandbox's idle clock for Idle-stop. Two kinds count: a **control-plane** interaction —
Provision (create), Start, Exec, Agent run, File transfer upload, or an explicit KeepAlive ping — and observed **work**, i.e.
CPU utilization at or above a small threshold seen by the node's periodic stats poll. Either kind resets the clock, so a sandbox doing
autonomous work (a long build, a server) is not idle-stopped even with no API calls, and a long-running
Agent run waiting near-zero CPU on the network is kept alive while it is in flight. Reads
(Get/List/ListPorts), File transfer download, and explicit Publish do not count. The two residual blind
spots — a process blocked at ~0% CPU, and traffic to a published port (not observable from the SDK) — are
covered by the `idle-stop: off` exemption label, not by a signal. The owner records the time of the last
Activity per sandbox.
_Avoid_: usage, heartbeat, touch

**Idle-stop**:
The owner node automatically stops a sandbox that has had no Activity for longer than the configured idle
timeout, after first running auto-publish for a git-backed workspace. It transitions the sandbox
running→stopped and **never deletes it**. Reserved capacity is unchanged (a stopped sandbox still counts),
so idle-stop frees host CPU/memory and preserves the agent's work — not scheduling headroom. Opt-in (off
by default); the background sweep is informally the "reaper". A sandbox carrying the label `idle-stop: off`
is exempt and never auto-stopped.
_Avoid_: reap (as delete), kill, evict

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
