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
  `Capacity().Snapshot()` for alloc; config workspaces + backend-derived
  templates + capabilities for predicates) **+** gossiped `cluster.PeerStates()`.
  In standalone mode (no cluster) it returns self only, so single-node placement
  degrades to pure local admission — preserving M1–M4 behavior.
- **Over-admission** (two concurrent A-requests both pick B before gossip
  refreshes B's alloc) is resolved by the **target**: B's `AdmitAndCreate`
  re-checks its *real* local capacity and returns `accepted=false`; the
  coordinator retries the next candidate. The hash tie-break (ADR-0007) spreads
  ties across nodes per request rather than hotspotting the lowest node id; its
  "independent coordinators rank the same node first" property is latent in M5
  (each request has a single entry-node coordinator) but harmless.
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
  `bin-pack` puts the fuller node first. **Score ties break by locality first**
  (the entry node `req.Local` wins, so an unconstrained create stays where it was
  requested when that node can take it; a loaded entry node is beaten on score and
  offloads), **then by `hash(requestID ⊕ nodeID)`** among remaining peers (stable
  across calls, varies per request). This preserves the POST-to-node model without
  losing balancing under load (ADR-0007).
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
- **`TryReserve(cpu,mem,disk) (id int, ok bool)`** — checks `used+req ≤ limit`
  for all three (a 0 limit is non-binding) **and reserves under a single mutex
  hold**, returning `ok=false` if it doesn't fit. Admission MUST be one atomic
  op: a split `CanAdmit`-then-`Reserve` is a TOCTOU race that lets two concurrent
  Provisions both pass the check and both reserve, over-admitting and defeating
  target-authoritative admission (the leaderless-correctness backstop). `Release(id)`
  frees a reservation.
- **Convert-on-success (the lifecycle rule):** `Manager.AdmitAndCreate` does
  `TryReserve →` (not ok → `ErrNoCapacity`) `→ backend.Create →` on success
  **resync `base` *absolutely* from durable records (`CommitBase`, which sets
  base from `List()` — now including the new record — and drops the reservation
  in one lock hold)**, on failure **`Release(id)`**. Using an absolute resync
  (identical to `SetBase`) rather than an incremental `base += cost` means a
  concurrent `Reconcile` cannot double-count the just-persisted record. The
  incremental `Commit` (base += cost) remains only as a fallback if the
  post-create `List` fails. This avoids both double-counting and the
  over-admission gap (release-then-wait-for-reconcile). *Rejected alternative:
  reconcile-only base updates, which leaves a window where a just-created
  sandbox is uncounted.*
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
  mapping `accepted=false`→`ErrNack`. A **dial failure** (addr-miss *or*
  `Conn` error, e.g. pin not yet gossiped) is also mapped to `ErrNack` (logged)
  so one unreachable peer doesn't abort placement — the loop tries the next
  candidate. The post-dial RPC error stays surfaced (an in-flight create may have
  succeeded; retrying could duplicate the sandbox).

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
M5 adds two fields to that message (additive): `disk_gb` (also added to
`sandbox.CreateSpec`) and `strategy` (client-selectable placement strategy).

**Request assembly** (entry node, `CreateSandbox`):
- **Effective sizing (no untracked sandboxes):** each resource's effective size
  = `request value` if >0, else `config.DefaultSandboxResources` if set, else a
  **built-in floor** (`// ponytail:` constant ~1 core / 512 MiB / 1 GiB; upgrade
  path: source from the daemon once the SDK exposes its defaults — `DaemonInfo`
  in v0.1.2 returns only socket paths, and `sandbox.Create` omits `--cpus`/
  `--memory` when zero, applying *hidden* daemon defaults). The filled-in size is
  applied once at `toSpec` so the **same** values flow to the scheduler,
  `Capacity` reservation, **and** `backend.Create` (passed explicitly to
  `WithCPUs`/`WithMemory`) — the daemon creates exactly that footprint and swarm
  accounting matches reality. Disk stays scheduling-only (no daemon flag) but its
  effective value still feeds disk capacity gating.
- **Strategy precedence:** `request.strategy` → `config.DefaultStrategy` →
  `"least-loaded"`. A non-empty `request.strategy` outside
  `{least-loaded, bin-pack, spread}` is rejected with `InvalidArgument` (input
  validation at the trust boundary — not silently defaulted).
- **RequestID = the operation id.** It is idempotency-deduped (a retried
  same-key `CreateSandbox` returns the existing op, so it never re-places with a
  different tie-break), unique per logical request, and stable — exactly what
  the ADR-0007 hash tie-break needs. (The sandbox id can't serve: the target
  assigns it only *after* placement.)

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
propagation is vNext). Recorded as **ADR-0011**.

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
- `DefaultStrategy string` (default `least-loaded`).
- `DefaultSandboxResources{CPUCores float64, MemoryBytes int64, DiskGB float64}`
  — per-sandbox defaults for unsized requests (see §6 effective sizing); operator
  tunes to mirror the daemon's real defaults.
- `ProvisionLimits.DiskGB float64`.

Templates are **backend-derived**, not config (matches CONTEXT.md glossary): the
`Backend` interface gains `ListTemplates(ctx) ([]string, error)` — `SDKBackend`
wraps the SDK's `template.List`; the `Fake` returns a test-settable set.
`node.go` advertises that list in `NodeState.Templates`, refreshed on the same
periodic tick as capacity.

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
  - capacity — TryReserve/release; convert-on-success base update;
    SetBase-from-records; auto-detect fallback to unlimited; **atomic-admission
    race test** (`-race`: N goroutines TryReserve a limit that fits only K,
    assert exactly K succeed).
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
- ~~**Cordon staleness at the target**~~ — **RESOLVED (post-M5 hardening).** The
  target's `Provision` now re-checks its own cordon state (`tbl.IsCordoned(self)`,
  updated synchronously by `Cluster.SetCordoned`) and returns
  `accepted=false, reason="cordoned"` → the coordinator NACKs and retries the next
  candidate. Closes the window where a node cordoned *after* the entry node's
  snapshot could still receive a forwarded provision before gossip propagated.
- **`ProvisionRequest.request_id`** is reserved (unused in v1): the hash
  tie-break runs at the entry node using the operation id, so the field is not
  populated/read yet — kept additively for future cross-node idempotency/trace.

---

## 10. Invariants preserved

- Secrets/keys/tokens never logged, persisted, or gossiped (spec §11).
- All gRPC methods classified (drift-guard); `Provision` is node-gated, not open.
- Wire evolution additive-only (ADR-0009); new `NodeState`/proto fields are
  backward-compatible.
- Standalone nodes (no cluster, no provision limits) keep working: self-only
  candidate set, auto-detected limits.
