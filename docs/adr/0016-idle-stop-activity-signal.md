# Idle-stop measures wall-clock since last Activity, where Activity = control-plane interaction OR observed CPU

The M7 reaper idle-stops a sandbox after `idle_timeout` of inactivity. "Inactive"
is defined as **wall-clock elapsed since the last Activity**, where **Activity is
the union of two signals**: a control-plane interaction (Provision / Start / Exec /
Agent run) *or* observed work — per-sandbox CPU utilization at/above a small
threshold, sampled by the node's existing periodic stats poll. A `running`
sandbox is idle only when, for the **entire** window, it saw neither. We use
neither a pure control-plane timer nor pure resource utilization, but their union.

## Why not the obvious single signals

The codebase visibly already collects per-sandbox CPU (`obsd.StatsCollector`), so
the natural question is "why not just reap on low CPU?" — and equally "why not
just an API-activity timer?" Both single signals are wrong in opposite ways:

- **Pure control-plane timer** (time since last Exec/Agent-run): stops a sandbox
  doing **autonomous work with no API calls** — a two-hour build, a server it
  launched, a background job. The sandbox is plainly alive but receives no
  interaction, so a pure API timer reaps it mid-work. This is the failure that
  forced the redefinition.
- **Pure CPU utilization:** (a) **noisy** — a process blocked on I/O or sleeping
  sits at ~0% CPU and is indistinguishable from an abandoned one; (b)
  **SDK-only** — the in-memory fake backend reports no meaningful CPU, so the
  signal can't be exercised in tests; (c) **misses network-bound agents** — an
  Agent run mostly waiting on LLM/API calls idles near 0% CPU yet is the most
  important work to protect.

The **union** fixes each: control-plane interaction gives a deterministic,
backend-independent, testable core and a clean grace period after the last
command; observed CPU keeps autonomous in-sandbox work alive without any API
call; and an in-flight Agent run bumps Activity on a `timeout/2` throttle so a
near-0%-CPU agent is never reaped while running.

## Scope

- **Resets only; never an admission/eligibility input.** Activity bumps a single
  per-sandbox `LastActivity` timestamp on `Record`. Idle is the single comparison
  `now - LastActivity > idle_timeout`, evaluated only for `running` records.
- **CPU-as-activity reuses existing infrastructure.** The node already runs a 10s
  stats ticker computing per-sandbox `CPUPercent` (`StatsCollector.Latest`); the
  bridge bumps `LastActivity` for any sandbox at/above `cpuActiveThreshold`. No
  new poll, no new gossip. The dynamic CPU signal matters on the real backend; the
  fake returns a fixed 10%, so deterministic idle-stop tests drive Activity through
  the control-plane path and a parameterized `now`, exercising `ReapIdle` directly
  rather than a fake-backed node sweep.
- **Opt-in and per-node.** Off unless `idle_timeout > 0`. A node only owns its own
  records, so the sweep is naturally per-node with no cross-node coordination.

## Consequences

- **Residual blind spots are covered by a label, not a signal.** A process blocked
  at ~0% CPU, or a sandbox serving a published port with traffic the SDK does not
  expose, can still read as idle. The `idle-stop: off` exemption label is the
  operator's escape hatch for these, rather than inventing a fragile liveness
  probe.
- **Threshold is a heuristic knob.** `cpuActiveThreshold` (~5%) trades false-reap
  of barely-busy sandboxes against false-keep of idle-noise sandboxes. It is a
  tunable constant, not a contract.
- **Survives restart by design.** `LastActivity` is persisted on the record, so
  idle time spans a node restart (a sandbox idle before a restart is still idle
  after). Correct, but means a node coming back up may idle-stop accumulated-idle
  sandboxes on its first sweep — not a bug.

## Considered

- **Pure `LastActivity` from control-plane events only** — rejected: reaps
  sandboxes doing autonomous work (the redefinition trigger).
- **Pure CPU / utilization** — rejected: noisy on blocked processes, untestable on
  the fake, misses network-bound agents.
- **A dynamic accept/deny callback (in-process hook or admission webhook)** so an
  owning application can veto a stop with out-of-band liveness knowledge —
  deferred: no consumer exists today, the webhook form is its own subsystem
  (fail-open/closed policy, mutual auth, sweep latency) deserving its own ADR, and
  it slots into `ReapIdle` additively when a consumer appears. Meanwhile the
  `idle-stop: off` label covers known static exemptions, and a consumer-driven
  `KeepAlive` ping — the *pull* model (app→node), a thin Activity bump with none of
  the webhook's failure modes — covers dynamic "this sandbox is in use" signalling.
  Only the *push* model (node→app veto) remains deferred.
