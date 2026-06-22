# M7 — reaper / idle-stop (auto-publish as the 4th trigger)

**Date:** 2026-06-19 · **Status:** approved + grilled, pre-plan · **Builds on:**
**ADR-0016** (idle = wall-clock since last Activity, where Activity = control-plane
interaction OR observed CPU — the signal decision this spec implements), ADR-0015
(git-backed workspaces are clone-only — the publish gate the reaper reuses), the
M6 auto-publish path (`maybeAutoPublish`, the publish-before-stop ordering
invariant). Glossary: CONTEXT.md gained **Activity** and **Idle-stop**.

A background sweep — the **reaper** — that **idle-stops** sandboxes idle past a
configurable timeout, auto-publishing git-backed ones first. This is the **4th
publish trigger** after explicit `PublishSandbox` RPC, graceful `StopSandbox`, and
`AgentRun` exit-0 with `publish_on_success`. Per-node, opt-in, **off by default**.

**Idle-stop = stop, never delete** (decided): it transitions running→stopped. A
stopped sandbox **still counts against this node's provision capacity** (`costSum`
excludes only `"lost"`), so idle-stop frees host CPU/memory and preserves the
agent's work — **not** reserved scheduling headroom. Full reclaim remains an
explicit `DeleteSandbox`. Per-sandbox **disk enforcement** stays **out of scope**
(sbx-go-sdk v0.1.2 `Create` has no per-sandbox disk option).

## 0. Reconciliation (verified against code, not assumptions)

- `sandbox.Record` (`internal/sandbox/record.go`) has `CreatedAt`/`UpdatedAt`/
  `LastPublish` but **no last-activity field**. `UpdatedAt` is set by
  `Manager.save` (`manager.go:125`) on every persist — Create, lifecycle
  (Start/Stop), `SetLastPublish` — but **not** on `Exec`/`AgentRun`, which call
  `mgr.Backend()` directly and never persist the record. So `UpdatedAt` is a
  lifecycle-write timestamp, *not* an activity signal. A dedicated `LastActivity`
  field is required.
- `Manager.Start`→`lifecycle`→`save` bumps `UpdatedAt` but would **not** touch a
  `LastActivity` field. **Re-reap bug:** without bumping `LastActivity` on
  `Start`, a revived sandbox keeps its stale activity time and the next sweep
  idle-stops it immediately. `Start` **must** count as Activity.
- `obsd.StatsCollector` (`internal/obsd/stats.go`) already polls per-sandbox
  `CPUPercent` into `Latest(name)` on the existing 10s ticker (`node.go:113`).
  This is the **observed-work** Activity source (ADR-0016). The dynamic CPU signal
  matters on the real backend; the **fake returns a fixed `CPUPercent: 10`**
  (`fake.go:152`), so with idle enabled the CPU bridge would continuously bump fake
  sandboxes — they never idle-stop *end-to-end on the fake*. Deterministic
  idle-stop tests therefore call `ReapIdle` directly, bypassing the node ticker and
  its bridge (§6).
- **Sandbox labels are a dead wire field today.** `CreateSandboxRequest.labels`
  (proto field 8) and `Sandbox.labels` (field 5) exist, but `toSpec`
  (`sandboxservice.go:135`) drops them, `CreateSpec` has no `Labels`, and
  `Manager.Create` never sets `rec.Labels` — so labels are silently discarded and
  always read back `nil`. The `idle-stop: off` opt-out requires making them
  persist (§5); no proto change, no `buf generate`.
- `SandboxService.maybeAutoPublish` (`sandboxservice.go:258`) is the reusable
  publish step: loads the record, checks clone-mode + a git-backed workspace +
  `ws.AllowPush()`, attributes the actor from the ctx principal else `"system"`,
  and calls `doPublish` (best-effort; failures audited+logged, never block). A
  reaper sweep has no principal ⇒ records `"system"` (correct).
- **Ordering invariant:** `StopSandbox` (`sandboxservice.go:248`) runs
  `maybeAutoPublish` **before** `mgr.Stop` because `doPublish` fetches from the
  `sandbox-<BackendName>` remote, which needs the **live daemon**, and hard-requires
  `rec.Status == "running"` (`:386`). The reaper publishes while still running,
  then stops. Same order.
- `ops.Operation.SandboxID` (`internal/ops/ops.go:24`) is populated **only when
  the op completes** — so while an Agent run is in flight there is no op→sandbox
  mapping, and an ops-based "skip sandboxes with a running op" guard cannot work.
  The long-run guard lives in the Agent-run poll loop instead (§1).
- `Manager.now func() time.Time` (`manager.go:40`) is unexported with no setter.
  The reaper avoids needing a clock seam by taking `now` as a **parameter** (§2,
  §3), so every unit is testable with plain time arithmetic.
- `node.New` runs background work via `runTicker(ctx, interval, fn)`
  (`node.go:425`): a 10s ticker (stats + metrics + `Reconcile` + load) and a 15s
  netlog ticker. `runTicker` calls `fn` synchronously per tick, so a slow sweep
  drops ticks rather than overlapping.
- `authz.go` classifies every **gRPC** method; `TestAuthz_AllMethodsClassified`
  fails on any unclassified one. The reaper *sweep* adds no gRPC method, but the
  **`KeepAlive`** RPC (§5b) does — it must be classified (mutating) and added to
  the forwarder's `newReplyFor` reply map. `KeepAlive` reuses the existing
  `IdRequest` (which already has `GetId()`, so id-routing is automatic) and adds
  one method to the proto (REST + `buf generate`); **no message changes**. (The
  labels fix in §5 needs no proto change — those fields already exist.)

---

## 1. Activity tracking — `Record.LastActivity`

**Goal:** a per-sandbox "last time real work or interaction happened," so the
reaper distinguishes an abandoned sandbox from a busy one (ADR-0016).

- **`Record`** — new field, persisted (survives restart, by design):
  ```go
  LastActivity time.Time `json:"last_activity,omitempty"`
  ```
- **`Manager`** — stamp + bump:
  - `Create` sets `rec.LastActivity = m.now()` alongside `CreatedAt`.
  - New `func (m *Manager) BumpActivity(ctx, id string) error`: load → set
    `LastActivity = m.now()` → `save`. `ErrNotFound` is returned and ignored by
    callers (best-effort — a racing delete must not fail the exec).

**Activity = the union of three bump sources (ADR-0016):**

1. **Control-plane** (`SandboxService`): `Create` (above), `Start`
   (`StartSandbox` → bump after `mgr.Start`, fixes the re-reap bug), `Exec`
   (bump after `Resolve`), `AgentRun` (bump at the start of the run closure), and
   **`KeepAlive`** — the explicit consumer ping (§5b).
2. **Long-running Agent run** — the poll loop (`sandboxservice.go:311`) bumps on
   a **`s.idleTimeout/2` throttle** (local `lastTouch`), so an agent pegged near
   0% CPU on the network is never reaped mid-run. `timeout/2` (not a magic 30s)
   guarantees ≥2 bumps per idle window for any timeout.
   ```go
   // ponytail: timeout/2 throttle => a running agent always bumps at least once
   // per idle window; skip entirely when idleTimeout==0 (reaper disabled).
   ```
3. **Observed work** (CPU-as-activity, in `node.go`'s existing 10s ticker): after
   `statsC.PollOnce`, for each running record with
   `statsC.Latest(rec.BackendName).CPUPercent >= cpuActiveThreshold` (~5%), call
   `mgr.BumpActivity(nctx, rec.ID)`. Reuses the record loop already in that ticker;
   gated on `idleTimeout > 0` to avoid writes when disabled.
   ```go
   // ponytail: 5% CPU counts as "doing work"; tune cpuActiveThreshold if
   // barely-busy sandboxes get reaped. Dynamic only on the real backend; the fake
   // reports a fixed 10%, which is why end-to-end idle-stop is unit-tested via
   // ReapIdle (bypassing this bridge), not a fake-backed node sweep.
   ```

`SandboxService` learns the timeout via `SetIdleTimeout(d time.Duration)` (wired
in `node.go`); it is the single source for both the throttle (§1.2) and the sweep
(§3).

---

## 2. Idle selector — `Manager.IdleRunning`

**Goal:** a pure, side-effect-free function the reaper can unit-test exhaustively.

```go
// IdleRunning returns running, non-exempt records whose last activity precedes
// now-timeout. now is a parameter (no clock seam) for deterministic boundary tests.
func (m *Manager) IdleRunning(ctx context.Context, now time.Time, timeout time.Duration) ([]*Record, error)
```

Selects `rec` where all hold:
- `rec.Status == "running"`,
- `rec.Labels["idle-stop"] != "off"` — the exemption (§5),
- `now.Sub(rec.LastActivity) > timeout` — strict `>` (a sandbox exactly at the
  boundary is not reaped; one tick later it is).
- `timeout <= 0` ⇒ returns nothing (defensive; the ticker also isn't started).

---

## 3. Reap orchestration — `SandboxService.ReapIdle`

**Goal:** publish-then-stop each idle sandbox, preserving every gate and the
ordering invariant.

```go
// ReapIdle stops every idle running sandbox (auto-publishing git-backed ones
// first) and returns the number reaped. now is a parameter for testability;
// the timeout comes from SetIdleTimeout.
func (s *SandboxService) ReapIdle(ctx context.Context, now time.Time) int
```

For each record from `mgr.IdleRunning(ctx, now, s.idleTimeout)`:
1. `s.maybeAutoPublish(ctx, rec.ID)` — best-effort; reuses the clone-mode +
   `AllowPush` gates (publish gating correct **by construction**), runs while
   still running, attributes `"system"`.
2. `s.mgr.Stop(ctx, rec.ID)` — running→stopped, emits the existing
   `sandbox.stopped` event. **Publish-failure policy (decided): stop regardless**
   — identical to graceful `StopSandbox`; unpushed work is recoverable
   (`StartSandbox` → republish) and a `sandbox.publish_failed` event already fires
   as an alerting hook. On `Stop` error: log and continue (one bad sandbox must
   not block the sweep); do **not** count it as reaped.
3. `slog.Info("idle-stopped sandbox", "sandbox", id, "idle", now.Sub(rec.LastActivity))`.

No new event type and no audit for the stop itself — matches today's
`StopSandbox`. (The *publish* in step 1 still audits `git.publish`.)

---

## 4. Where it runs — a dedicated ticker

In `node.New`, after the existing tickers, **only when enabled**:

```go
sandboxes.SetIdleTimeout(d) // d from cfg.IdleTimeoutDuration()
if d > 0 {
    go runTicker(nctx, reapInterval(d), func() { sandboxes.ReapIdle(nctx, time.Now()) })
}
```

- **Dedicated** (not folded into the 10s loop) so a slow `git push` inside
  `maybeAutoPublish` cannot stall the stats/metrics/reconcile path. (The
  *CPU-as-activity bump* in §1.3 does live in the 10s loop — it's a cheap map
  lookup + occasional write, never a network call.)
- **Sweep cadence** `reapInterval(d) = min(d, time.Minute)` — minute-scale for
  real timeouts, responsive for small test timeouts; worst-case over-idle bounded
  by `timeout + sweep ≤ 2·timeout`.
  ```go
  // ponytail: min(timeout, 1m); a fixed 1m feels broken for a 10s test timeout,
  // and a separate sweep-interval knob is YAGNI.
  ```
- **Per-node** by construction: `Manager` owns only this node's records.

---

## 5. Config + labels

**One knob:**
```go
// Config (internal/config/config.go)
IdleTimeout string `yaml:"idle_timeout"` // Go duration, e.g. "30m"; "" or <=0 disables
```
- **Default** `""` (disabled) — opt-in, matching `Backend`/`DefaultStrategy`.
- **`Validate()`** — if non-empty, must parse with `time.ParseDuration` and be
  `>= 0`; a parse error or negative is rejected. (Empty stays valid = disabled.)
- **`IdleTimeoutDuration() time.Duration`** — parses (already-validated), `""`→0.
- Chose a Go-duration **string** over int seconds (operator-friendly) and over a
  custom `time.Duration` YAML type (yaml.v3 won't unmarshal `"30m"`; a custom
  `UnmarshalYAML` is an abstraction one knob doesn't earn). Parsed in two
  validated places.

**Make sandbox labels real (required for the `idle-stop: off` opt-out):**
- add `Labels map[string]string` to `sandbox.CreateSpec`,
- map `r.Labels` in `toSpec`,
- set `rec.Labels = spec.Labels` in `Manager.Create`.

Side effect (strictly more correct): `GetSandbox`/`ListSandboxes` start returning
create-time labels instead of always `nil` (`toProto` already copies `rec.Labels`).
The reaper then skips any record with `Labels["idle-stop"] == "off"` (§2). These
are the *sandbox's own* labels — distinct from `node_affinity` node-labels
(glossary), no namespace clash.

---

## 5b. `KeepAlive` — consumer keep-alive ping

**Goal:** let a consumer that *knows* a sandbox is in use — an attached-but-quiet
session, the gap CPU and the static `idle-stop: off` label can't cover — reset the
idle clock on demand. The **pull** counterpart to the deferred veto callback
(ADR-0016): the app tells the node, so there is no outbound call, no
fail-open/closed policy, no endpoint auth.

- **proto** (`sandbox.proto`) — one method, reusing the existing `IdRequest`:
  ```proto
  rpc KeepAlive(IdRequest) returns (Sandbox) {
    option (google.api.http) = {post: "/v1/sandboxes/{id}/keepalive"};
  }
  ```
  `IdRequest` already has `GetId()`, so `Forwarder` routes it to the owner by id
  with no new code beyond the reply map. One method, **no message changes**;
  requires `buf generate`.
- **handler** (`SandboxService`): resolve → `BumpActivity` → return the sandbox.
  ```go
  func (s *SandboxService) KeepAlive(ctx, r *IdRequest) (*Sandbox, error) {
      if err := s.mgr.BumpActivity(ctx, r.Id); err == sandbox.ErrNotFound {
          return nil, status.Error(codes.NotFound, "sandbox not found")
      } else if err != nil {
          return nil, status.Error(codes.Internal, err.Error())
      }
      return s.GetSandbox(ctx, &sbxv1.GetSandboxRequest{Id: r.Id})
  }
  ```
  No status precondition — keep-alive on a stopped sandbox is a harmless no-op bump
  (YAGNI to special-case).
- **authz** (`authz.go`): classify **mutating (admin)**. A `read-only` principal
  pinning sandboxes alive indefinitely is a resource-exhaustion vector. (Required,
  or `TestAuthz_AllMethodsClassified` fails.)
- **forwarding** (`forward.go`): add
  `case ".../KeepAlive": return new(sbxv1.Sandbox)` to `newReplyFor` (owner-routed
  by id, like Start/Stop).

Then it is just another control-plane Activity source (§1.1):
`KeepAlive`→`BumpActivity`→`LastActivity` reset. Distinct from `idle-stop: off` —
**dynamic** (ping while in use, lapse when done) vs. a **static, permanent**
exemption.

---

## 6. Testing

Fake backend, no sleeps, deterministic via the `now` parameter:

- **`Manager.IdleRunning`** (sandbox pkg): boundary — `now == LastActivity+timeout`
  not selected, `+timeout+1ns` selected; non-`running` never selected; a record
  labeled `idle-stop: off` never selected even when past timeout.
- **`SandboxService.ReapIdle`** (apiserver pkg): `SetIdleTimeout(timeout)`; create
  2 sandboxes (one git-backed clone-mode with `AllowPush`); `ReapIdle(ctx,
  t0+timeout+1s)` stops both and the git-backed one records a `LastPublish` +
  `git.publish` audit; `ReapIdle(ctx, t0)` reaps none.
- **`BumpActivity` / `Start` re-reap**: `BumpActivity` advances `LastActivity`;
  `ErrNotFound` on a missing id; a record that is idle, then `Start`ed, is **not**
  selected by `IdleRunning` (re-reap regression).
- **labels persist**: a `CreateSpec.Labels` round-trips to `rec.Labels` and back
  through `toProto`.
- **`KeepAlive`** (apiserver pkg): a since-idle sandbox is no longer selected by
  `IdleRunning` after a `KeepAlive`; `NotFound` on a missing id. `KeepAlive` is in
  `mutatingMethods` (covered by `TestAuthz_AllMethodsClassified`).
- **config**: `idle_timeout` default empty; `Validate` rejects `"garbage"` and
  `"-5m"`, accepts `""` and `"30m"`; `IdleTimeoutDuration` round-trips.
- **CPU-as-activity** (§1.3): the fake's fixed `CPUPercent: 10` means a fake-backed
  node sweep would never idle-stop (the bridge keeps bumping), so end-to-end
  idle-stop is unit-tested via `ReapIdle` **directly** (bypassing the node ticker).
  The bridge itself is a 3-line threshold check in `node.go`, verified by
  inspection; its dynamic value is on the SDK backend (manual e2e carry-forward).

## 7. Deliberately skipped

- **Per-sandbox disk enforcement** — deferred (SDK gap).
- **Dynamic *node→app* veto callback** (in-process hook or admission webhook) so
  the node can ask an owning application to override a stop — **deferred, no
  consumer today** (ADR-0016 "Considered"). The *app→node* pull model is **in
  scope** as `KeepAlive` (§5b), which covers the common "consumer knows it's in
  use" case without the webhook's fail-policy/auth cost; the push model remains a
  later additive nil-safe decider check in `ReapIdle` if a consumer appears.
- **A `sandbox.reaped` event / stop audit** — reuses `sandbox.stopped`.
- **A separate sweep-interval config** — derived from the timeout (§4).
- **Reclaiming reserved capacity on idle** (delete-on-idle, or excluding
  `"stopped"` from `costSum`) — out of scope; idle-stop is stop-only by decision.
