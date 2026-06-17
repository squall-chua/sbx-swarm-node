# M5 — Scheduling / Placement Design

**Date:** 2026-06-17 · **Milestone:** M5 (scheduling) · **Status:** approved, pre-plan

Constraint-based placement for the swarm: pick a node for each new sandbox by
filtering candidates on hard predicates, scoring survivors by dominant-resource
ratio with a hash tie-break (ADR-0007), then provisioning on the chosen node
with **target-authoritative admission** and NACK retry. Builds on M4's gossiped
`NodeState` and `routing.Table`, and the security follow-on's node-key auth.

This design supersedes the forward-looking plan
`docs/superpowers/plans/2026-06-15-sbx-swarm-node-m5-scheduling.md` where they
differ (that plan was written before M4/security landed and drifted on
signatures, units, and the auth model).

---

## 1. Goal & scope

A new sandbox request is placed on the best eligible node in the swarm. "Best"
= passes all hard constraints (workspaces, template, capabilities, labels,
capacity, not cordoned) and minimizes (or, per strategy, maximizes) the
post-placement dominant-resource ratio. Placement is **leaderless**: every node
can coordinate; correctness under concurrent coordinators comes from
target-authoritative admission, not a lock.

**In scope:** the pure scheduler, soft-reservation capacity accounting with
auto-detected limits, the coordinator (scheduler + attempt + retry), the
internal node→node `Provision` RPC with node-gated authz, three placement
strategies, and the wiring into `CreateSandbox`.

**Out of scope (deferred, see §9):** per-sandbox disk *enforcement* by the
daemon, a usage-based `least-actual-load` strategy, and gossiped reservation
sharing.

---

## 2. Architecture & data flow

Four units. The first three are independently testable; the fourth is the RPC
boundary.

```
CreateSandbox (entry node A — authz=admin enforced here, synchronously)
   └─ op.Run (async, context.Background)         ← admin already enforced; op is detached
        └─ coordinator.Provision(req)
             ├─ scheduler.Schedule(req, candidates()) → ordered node ids
             │     (pure: filter → dominant-resource score → hash tiebreak, ADR-0007)
             └─ for nodeID in order:  attempt(nodeID)
                  ├─ local  → mgr.AdmitAndCreate(spec)                      (no RPC)
                  └─ remote → peer.Pool.Conn(addr,nodeID) → InternalService.Provision
                              accepted=false → coordinator.ErrNack → try next candidate
```

- **`candidates()`** is built fresh per request from: **self** (local
  `Capacity().Snapshot()` for alloc; config workspaces/templates + capabilities
  for predicates) **+** gossiped `cluster.PeerStates()`. In standalone mode (no
  cluster) it returns self only, so single-node placement degrades to pure local
  admission — preserving M1–M4 behavior.
- **Over-admission** (two concurrent A-requests both pick B before gossip
  refreshes B's alloc) is resolved by the **target**: B's `AdmitAndCreate`
  re-checks its *real* local capacity and returns `accepted=false`; the
  coordinator retries the next candidate. ADR-0007's hash tie-break makes
  independent coordinators rank the same node first for a given request,
  minimizing these bounces.
- The entry node's `CreateSandbox` op records the returned sandbox id (which
  carries the **target's** node prefix, ADR-0002), so later Get/Delete/Exec
  route to the owner via the existing forwarder. The op itself lives on the
  coordinating node — unchanged from M1c.

---

## 3. Scheduler (pure)

`internal/scheduler/scheduler.go` — `Schedule(req Request, cands []Candidate) ([]string, error)`.

**Three provisionable resources:** CPU (cores), memory (KB), disk (GB). Units
are chosen to match their advertised source (gossip `*MemKB`, `Usage.Disk*GB`).

- **Filter (`fits`)** — reject if cordoned; require every requested workspace,
  the template (if set), every requested capability; honor affinity
  (label==value) / anti-affinity (label!=value); finally require
  `alloc+req ≤ limit` for **all three** resources. A limit of `0` means
  "unlimited" *at the scheduler level* (see §4 — config 0 is replaced by an
  auto-detected limit before it reaches the scheduler, but the scheduler still
  treats a literal 0 limit as non-binding so the math is safe).
- **Score** — post-placement **dominant-resource ratio**:
  `max(ratio(cpu), ratio(mem), ratio(disk))` where `ratio(a,b)=a/b`, and
  `ratio(_,0)=1` (a zero/unknown limit sorts as fully loaded so real nodes win).
  `spread` strategy scores by sandbox count instead.
- **Sort** — `least-loaded` (default) and `spread` put the lighter node first;
  `bin-pack` puts the fuller node first. Ties break by
  `hash(requestID ⊕ nodeID)` (stable across calls, varies per request).
- `ErrNoEligibleNode` when no candidate passes the filter.

`Candidate` carries `NodeID, Workspaces/Templates/Capabilities (map[string]bool),
Labels, LimitCPU/Mem/Disk, AllocCPU/Mem/Disk, Sandboxes (count), Cordoned`.
`Request` carries `CPU/Mem/Disk, Workspaces, Template, Capabilities,
Affinity/AntiAffinity, Strategy, RequestID`.

---

## 4. Capacity (`internal/sandbox/capacity.go`)

Soft, in-memory CPU/mem/disk accounting against a per-node provision limit. The
durable truth is `Manager.List()`; reservations cover the in-flight create
window.

- **Limits** come from `config.ProvisionLimits`. When a resource's limit is `0`,
  it is **auto-detected from the host**: CPU = `runtime.NumCPU()`, memory =
  `unix.Sysinfo` total RAM, disk = `unix.Statfs(DataDir)` total. Auto-detection
  is Linux-only (the deploy target); if a syscall errors, that resource falls
  back to unlimited (never blocks creates). Explicit non-zero config always
  wins.
- **State:** `limit{CPU,Mem,Disk}`, `base{CPU,Mem,Disk}` (reconciled from
  durable records), and a map of soft `reservation{cpu,mem,disk}` keyed by an
  int id. `used = base + Σ reservations`.
- **`CanAdmit(cpu,mem,disk) bool`** — `used+req ≤ limit` for all three (a 0
  limit is non-binding).
- **`Reserve(cpu,mem,disk) int` / `Release(id)`** — hold/free during a create.
- **Convert-on-success (the lifecycle rule):** `Manager.AdmitAndCreate` does
  `CanAdmit → Reserve → backend.Create →` on success **`base += cost` and
  `Release(id)` atomically**, on failure **`Release(id)`**. This avoids both
  double-counting (reservation *and* base) and the over-admission gap
  (release-then-wait-for-reconcile). *Rejected alternative: reconcile-only base
  updates, which leaves a window where a just-created sandbox is uncounted.*
- **Drift correction:** `SetBase` recomputes base from durable non-terminal
  records (`List()`, sum each record's `Spec` CPU/mem/disk, exclude `lost`),
  piggybacked on the **existing 10s stats ticker** in `node.go` and at boot.
  This reclaims capacity from lost/deleted sandboxes and corrects any reservation
  drift.
- **`Snapshot() (cpu,mem,disk)`** = `used` — feeds the gossiped
  `AllocCPU/AllocMemKB/AllocDiskGB`.

`Manager` gains a `*Capacity`, `Capacity()`, `AdmitAndCreate(ctx,spec)`, and the
`ErrNoCapacity` sentinel. `AdmitAndCreate` shares `Create`'s persistence + event
+ owned-id-notify path.

---

## 5. Coordinator (`internal/coordinator/coordinator.go`)

`New(candidates func() []scheduler.Candidate, attempt AttemptFunc) *Coordinator`;
`Provision(ctx, req) (sandboxID, error)`. `attempt` is injected so ordering and
retry are unit-tested without RPC.

- Runs `scheduler.Schedule`; on `ErrNoEligibleNode` returns it unchanged.
- Tries candidates best-first. `attempt` returning `ErrNack` → try the next; any
  other error (transport, etc.) is surfaced immediately. All candidates nacked →
  `ErrNoCapacity`.
- In `node.go`, `attempt(nodeID)`: if `tbl.IsLocal`-equivalent (nodeID == self)
  call `mgr.AdmitAndCreate`; else `peer.Pool.Conn(addr,nodeID)` → `Provision`,
  mapping `accepted=false`→`ErrNack`.

---

## 6. Internal Provision RPC + auth (decision B)

`proto/sbxswarm/v1/internal.proto`:

```proto
service InternalService {
  rpc Provision(ProvisionRequest) returns (ProvisionReply); // node→node only; no HTTP annotation
}
message ProvisionRequest { CreateSandboxRequest spec = 1; string request_id = 2; }
message ProvisionReply   { bool accepted = 1; string sandbox_id = 2; string reason = 3; }
```

`ProvisionRequest.spec` **reuses** the existing `CreateSandboxRequest` (it
already carries agent/template/cpus/memory_bytes/clone/workspaces/env/labels);
M5 adds `disk_gb` to that message and to `sandbox.CreateSpec` (additive).

`apiserver/provision.go` — `InternalService.Provision` maps `spec`→`CreateSpec`,
calls `mgr.AdmitAndCreate`; `ErrNoCapacity`→`{accepted:false,reason:"no
capacity"}`, success→`{accepted:true,sandbox_id}`. Registered on `grpcSrv` only
(no gateway handler).

**Authorization — node-gated internal bucket.** `internal/apiserver/authz.go`
gains a third classification:

```
internalMethods = { "/sbxswarm.v1.InternalService/Provision": true }
```

`authorize()`: an `internalMethods` call requires `p.node == true` (a verified
swarm peer authenticated via node-key). A user-only principal is denied; the
coordinator's `peer.Pool.Conn` attaches node-key `PerRPCCredentials`
automatically, so **no user-token is threaded** through the async op.

*Why B (not "forward the admin token"):* admin is enforced **once**,
synchronously, at the entry node's `CreateSandbox` (a mutating method) before
the async op even starts. The internal hop is node→node inside the swarm trust
boundary (node-key auth + gossiped-pubkey TLS pin). Forwarding the user token is
both more plumbing *and* fragile — the create op runs detached on
`context.Background()`, and the REST role token has a 30s TTL, so a slow
provision would outlive the token and fail. The narrow relaxation (a verified
node may trigger an internal provision without a user role) is bounded by:
node-key is already the swarm membership boundary; target admission caps
resource blast radius; and revocation via the existing denylist hook (gossiped
propagation is vNext). Recorded as a new ADR.

The drift-guard `TestAuthz_AllMethodsClassified` adds
`sbxv1.InternalService_ServiceDesc` to its enumerated descriptors so `Provision`
must stay classified.

---

## 7. NodeState & config additions (additive, ADR-0009 wire-compat)

`membership.NodeState` (additive JSON fields, omitempty):
- `Workspaces []string` (advertised workspace names)
- `Templates  []string` (advertised template names)
- `LimitDiskGB float64`, `AllocDiskGB float64`

`config.Config`:
- `Workspaces []WorkspaceConfig{Name, HostPath, ReadOnly}` — names advertised
  for filtering; `HostPath` also feeds the `SDKBackend` `WorkspaceResolver`.
- `Templates []string` — advertised template names (config-sourced for v1;
  backend-derived discovery is future).
- `DefaultStrategy string` (default `least-loaded`).
- `ProvisionLimits.DiskGB float64`.

`node.go` populates the new `NodeState` fields from config + `Capacity()`. The
gossiped `LimitCPU/LimitMemKB/LimitDiskGB` come from the **resolved** capacity
limits (post auto-detect), not raw config, so peers see real capacity; the
`AllocCPU/AllocMemKB/AllocDiskGB` stay fresh from `Capacity().Snapshot()` on the
existing gossip-update path.

---

## 8. Testing

- **Unit (TDD, exhaustive):**
  - scheduler — workspace/template/capability/label/capacity filtering;
    dominant-resource scoring across all three resources; bin-pack vs
    least-loaded ordering; deterministic hash tie-break.
  - capacity — admit/reserve/release; convert-on-success base update;
    SetBase-from-records; auto-detect fallback to unlimited.
  - coordinator — tries in scheduler order; retries on NACK; `ErrNoCapacity`
    when all nack; surfaces hard errors.
- **Integration (`//go:build integration`, 2 nodes, fresh ports):** node A
  coordinates; a request whose workspace exists only on B lands on B via the
  forwarded `Provision`; a request exceeding both nodes' limits fails with `no
  capacity`. Run with the real toolchain (`go test -tags integration
  ./internal/membership/` style), plus `-race` on the concurrent packages.

---

## 9. Deferred (documented)

- **Per-sandbox disk enforcement** — sbx-go-sdk v0.1.2 `sandbox.Create` has no
  disk option, so disk is **scheduling-only** in v1 (filter + score + admission
  account for requested `DiskGB`, but the daemon does not cap actual disk).
  `exec.Stats` already exposes `DiskTotalGB/DiskUsedGB`, so usage stays
  observable. Marked with a `// ponytail:` comment at the backend gap.
- **`least-actual-load` strategy** — additive; feed `actual_util` (spec §9, M2)
  into a score branch.
- **Gossiped reservation sharing** — coordinators don't share in-flight
  reservations; target-authoritative admission is the backstop. Acceptable in
  the leaderless model.

---

## 10. Invariants preserved

- Secrets/keys/tokens never logged, persisted, or gossiped (spec §11).
- All gRPC methods classified (drift-guard); `Provision` is node-gated, not open.
- Wire evolution additive-only (ADR-0009); new `NodeState`/proto fields are
  backward-compatible.
- Standalone nodes (no cluster, no provision limits) keep working: self-only
  candidate set, auto-detected limits.
