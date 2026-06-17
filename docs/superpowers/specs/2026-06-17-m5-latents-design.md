# M5 scheduling latents — design

**Date:** 2026-06-17 · **Status:** approved, pre-plan · **Builds on:**
`2026-06-17-m5-scheduling-design.md` (M5 placement), ADR-0007, ADR-0009,
ADR-0011.

Closes the three latent items left after M5 + the post-M5 carry-forward
hardening (target self-cordon recheck + dial-failure NACK, already merged at
`a620f65`). Each is independent; all three are additive and preserve M5's
invariants (node-gated `Provision`, reserved-capacity admission, additive wire
evolution, no secret leakage).

## 0. Reconciliation (verified against code, not assumptions)

- `ProvisionRequest.request_id` (proto field 2) already exists, unpopulated/unread.
- `CreateSandboxRequest` has `labels` + `strategy`; **no** affinity fields yet.
- `scheduler.Request.Affinity/AntiAffinity` are already enforced by `fits()`;
  only the input path is missing.
- `NodeState` gossips **reserved** alloc, not actual util. `StatsCollector.ActualUtil()`
  (`internal/obsd/stats.go`) is local-only.
- The 10s ticker in `node.go` already calls `clusterInstance.UpdateLocalAlloc(...)`
  **unconditionally** every tick (bumps `StateVersion`, re-advertises) and already
  computes `au := statsC.ActualUtil()` for metrics. Util gossip rides this existing
  cadence — **no new gossip churn**.

---

## 1. Label-affinity input path

**Goal:** let a create request constrain placement to peers whose gossiped
`Labels` match (affinity) or differ (anti-affinity).

- **Proto** (`sandbox.proto`, `CreateSandboxRequest`):
  ```proto
  map<string, string> affinity = 11;
  map<string, string> anti_affinity = 12;
  ```
  `buf generate` → commit regenerated `internal/gen/...`.
- **Wiring** (`apiserver/sandboxservice.go`, `requestFromSpec`): set
  `Affinity: spec.Affinity, AntiAffinity: spec.AntiAffinity` on the
  `scheduler.Request`. No scheduler-logic change — `fits()` already evaluates
  both maps against `Candidate.Labels`.
- **Placement-only:** affinity is resolved at the **entry node** during
  `scheduler.Schedule`; the chosen target just creates. `ProvisionRequest` is
  untouched, so affinity never crosses the node→node hop.
- **Empty maps** = unconstrained (current behavior).

**Tests:**
- `requestFromSpec` carries affinity/anti-affinity onto the `scheduler.Request`.
- Integration-light scheduler test: two candidates, one labeled `zone=eu`;
  `affinity{zone:eu}` ⇒ only eu eligible; `anti_affinity{zone:eu}` ⇒ eu excluded.

---

## 2. request_id idempotency + RPC-error retry

**Goal:** make the node→node `Provision` RPC safely retryable so a transient
post-dial RPC error no longer either (a) aborts placement or (b) risks a
duplicate sandbox. Closes the gap documented in §5/§9 of the M5 spec.

### 2.1 Entry side
- `attemptFor` (`node.go`) is threaded the request id (`req.RequestID`, which is
  already `op.ID`, stable across op retries). The remote branch sends
  `&ProvisionRequest{Spec: spec, RequestId: requestID}`.
- **Retry:** on a *post-dial* RPC error (the `NewInternalServiceClient(...).Provision`
  call itself erroring — distinct from the `pool.Conn` dial failure, which already
  NACKs), retry the **same target once**. If the retry also errors, surface the
  error (unchanged outer contract). One retry is enough to cover a dropped
  response; idempotency makes it safe.

### 2.2 Target side (`InternalService`)
- Add an in-memory **bounded TTL dedup map**: `request_id → sandbox_id`.
  - Bound ~1024 entries, TTL ~5 min (comfortably longer than an op's create
    window). Eviction: drop expired on access + a size cap (oldest-out) to bound
    memory. A tiny mutex-guarded struct (`internal/apiserver/dedup.go` or inline).
  - In-memory is sufficient: a node crash also loses in-flight ops and capacity
    reservations, so there is nothing durable to be idempotent against.
- `Provision` flow becomes:
  1. self-cordon recheck (already merged) → `accepted=false, reason="cordoned"`.
  2. if `request_id != ""` and seen → return `accepted=true, sandbox_id=<existing>`
     (no second create, no second reservation).
  3. else admit+create as today; on success **record** `request_id → sandbox_id`
     before returning.
- Empty `request_id` ⇒ no dedup (back-compat; older peers / direct calls).

**Concurrency note:** two *concurrent* Provisions with the same request_id (rare:
entry would have to fire the retry before the first returned) could both miss the
map and both create. Acceptable for v1 — the retry is sequential (after the first
errored), so the realistic path is first-create-then-dedup. A single mutex around
check-and-reserve-slot could close it later if needed (`// ponytail:` note).

**Tests:**
- Same `request_id` twice ⇒ exactly one `AdmitAndCreate`, same `sandbox_id`
  returned both times (use the fake backend / a create counter).
- `attemptFor`: backend Provision errors once then succeeds ⇒ one retry, single
  sandbox id returned (idempotent target).
- Empty `request_id` ⇒ two creates (no dedup), preserving back-compat.

---

## 3. least-actual-load strategy

**Goal:** a placement strategy that ranks by **actual** CPU/mem utilization
(gossiped from M2 stats) rather than reserved allocation — packs by real usage,
so idle-but-reserved sandboxes don't make a node look full.

- **Gossip** (`NodeState`, additive bulk fields):
  ```go
  ActualCPU float64 `json:"util_cpu,omitempty"` // normalized 0..1+ vs this node's CPU limit
  ActualMem float64 `json:"util_mem,omitempty"` // normalized 0..1+ vs this node's mem limit
  ```
  Normalized per-node, so cross-node comparison is apples-to-apples (fraction of
  own capacity used). Non-sensitive (no secret invariant impact).
- **Publish:** the existing 10s ticker already computes `au := statsC.ActualUtil()`.
  Replace the ticker's `UpdateLocalAlloc(rc, rm, rd)` call with
  `UpdateLocalLoad(rc, rm, rd, au.CPU, au.Mem)` — one method that sets alloc + util
  and bumps `StateVersion` **once** (no extra re-advertise). `UpdateLocalAlloc` is
  removed (single caller) or kept as a thin wrapper; the plan removes it since it
  has exactly one call site.
- **Candidate** (`scheduler.Candidate`): add `ActualCPU, ActualMem float64`.
  `buildCandidates`: self from live `statsC.ActualUtil()` (freshest); peers from
  `ns.ActualCPU/ns.ActualMem`. Standalone (no cluster) ⇒ self-only, util from
  local statsC.
- **Score** (`scheduler.score`): add a branch
  ```go
  if req.Strategy == "least-actual-load" {
      return max(c.ActualCPU, c.ActualMem) // dominant actual util; lighter-first
  }
  ```
  `resolveStrategy` accepts `"least-actual-load"`.
- **Filter unchanged:** `fits()` still admits on **reserved** alloc vs limit, so
  actual util only re-ranks survivors — it can never over-admit (reserved
  capacity stays the admission backstop).
- **Missing util** (peer that hasn't gossiped util yet, or limit unknown): a
  zero `ActualCPU/Mem` sorts as least-loaded. Acceptable — a node with no
  reported util looks empty, matching the “prefer idle” intent; reserved-alloc
  filtering still gates it.

**Tests:**
- `score` with `least-actual-load`: candidate with lower `max(util)` ranks first;
  ties fall through to locality/hash (existing tiebreak).
- `least-loaded` (reserved) unchanged when reservations are equal but util differs.
- `buildCandidates` populates self util from statsC and peer util from NodeState
  (light unit/integration check).

---

## 4. Out of scope / deferred (unchanged)

- Per-sandbox disk **enforcement** — still blocked by sbx-go-sdk v0.1.2 (no disk
  option on `Create`); disk stays scheduling-only.
- Gossiped reservation sharing — target-authoritative admission remains the
  backstop (leaderless model).
- Strong concurrent-dup protection on the request_id map (see §2.2 note).

## 5. Invariants preserved

- Additive proto + gossip fields only (ADR-0009); old peers ignore new fields.
- `Provision` stays node-gated (ADR-0011); dedup/idempotency adds no new authz
  surface (still inside `internalMethods`).
- Reserved-capacity admission unchanged — least-actual-load only affects ranking.
- Secrets/keys/tokens never logged/persisted/gossiped — util is non-sensitive.
- Standalone (no cluster) keeps working: self-only candidates, util from local
  statsC, no gossip.
