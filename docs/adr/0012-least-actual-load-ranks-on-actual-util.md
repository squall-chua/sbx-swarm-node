# least-actual-load ranks on actual utilization, not reserved allocation

ADR-0007 defines placement scoring as the **post-placement dominant-resource
ratio over *reserved* allocation** (`(alloc+req)/limit` across CPU/mem/disk), and
admission (`fits()` + target `TryReserve`) gates on that same reserved figure.
The `least-actual-load` strategy departs from this: it ranks eligible nodes by
**actual gossiped CPU/mem utilization** (`max(ActualCPU, ActualMem)`, lighter
first) — a *different signal* from the one admission uses.

The motivation is real-usage packing. Reserved allocation over-counts idle
sandboxes: a node holding ten reserved-but-idle sandboxes looks full to
`least-loaded`, even though its CPU/mem sit near zero. `least-actual-load` places
onto whichever eligible node is genuinely least busy right now, sourced from the
M2 stats the node already collects (`StatsCollector.ActualUtil()`), gossiped on
the existing 10s `NodeState` re-advertise cadence (no new gossip churn).

## Scope of the divergence

- **Ranking only.** Eligibility is unchanged: `fits()` still filters on reserved
  alloc vs limit, and the target still admits via `TryReserve` on reserved
  capacity. `least-actual-load` re-orders survivors; it can **never over-admit**.
  The reserved ceiling is the backstop, exactly as for the other strategies.
- **Self and peer util come from the same gossiped `NodeState`** (self via
  `LocalState()`, peers via `PeerStates()`), not from a node's live local
  collector. This keeps the candidate representation symmetric so independent
  coordinators rank a given node consistently (ADR-0007's "same node first"
  intent), at the cost of ≤10s staleness — acceptable for a usage heuristic.
  Standalone nodes have a self-only candidate, so the util value is moot.
- Util is normalized per node (fraction of that node's own limit), so
  cross-node comparison is apples-to-apples. The fields are non-sensitive
  (no secret-invariant impact) and additive (ADR-0009).

## Consequences

- **Wake-up risk (accepted).** A reservation-heavy but currently-idle node can be
  preferred; when its idle sandboxes become active the node can saturate on
  actual util. This is the operator's opt-in tradeoff for usage packing; reserved
  admission still prevents exceeding the limit. Operators wanting headroom against
  wake-up should stay on `least-loaded`/`bin-pack` (reserved-based).
- **Relaxed determinism.** Because actual util is continuous and gossiped with a
  lag, scores rarely tie and may differ slightly across coordinators within a
  gossip window — so ADR-0007's hash tie-break (which only engages on exact ties)
  rarely applies here. Independent coordinators may briefly disagree on the
  least-busy node; the target's reserved admission resolves any resulting bounce.
- **Staleness/herd window.** Within a ~10s window several coordinators may pick
  the same lowest-util node before gossip reflects the new placements — the same
  bounded over-selection M5 already documented for reserved alloc, with the same
  backstop (target-authoritative admission).
- New `NodeState.ActualCPU/ActualMem` fields are additive; old peers omit them
  and read as `0` (sorts as least-loaded — a node reporting no util looks idle,
  matching the "prefer idle" intent; reserved filtering still gates it).

## Considered

- **Hybrid score** `max(actualUtil, reservedRatio)` — safer against wake-up
  saturation, but it collapses toward `least-loaded` and blunts the strategy's
  purpose (pack by real usage). Rejected; operators who want reserved-based
  ranking already have three strategies for it.
- **Live self-util from the local collector** (fresher than gossip) — rejected:
  asymmetric with peers, breaks cross-coordinator consistency, and threads the
  stats collector into candidate construction for ≤10s of freshness that a usage
  heuristic doesn't need.
- **Mutating ADR-0007** to cover both signals — rejected: 0007 stays the
  canonical reserved-alloc scoring + tie-break record; this is a separate,
  additive strategy decision.
