# Multi-resource scoring by dominant resource, hash tie-break

The scheduler scores each candidate node on its **post-placement dominant-resource ratio** across
**three** provisionable resources — CPU (cores), memory (KB), disk (GB):
`max((alloc.cpu+req.cpu)/limit.cpu, (alloc.mem+req.mem)/limit.mem, (alloc.disk+req.disk)/limit.disk)`.
`least-loaded` minimizes it, `bin-pack` maximizes it (subject to ≤ 1.0), `spread` minimizes by sandbox
count. **Score ties are broken by, in order: (1) locality — the entry/coordinating node wins, so an
unconstrained create stays where it was requested when that node can take it; (2) `hash(request_id ⊕
node_id)` among the remaining peers.** The locality bias only breaks exact ties: an unloaded entry node
ties for best and keeps the work, while a loaded entry node is beaten on score and offloads to a lighter
peer. This preserves the POST-to-node model (least surprise) without sacrificing balancing under load;
the hash tier still spreads ties across peers when the entry node is ineligible or not tied. A zero/unknown limit yields ratio 1 (sorts
as fully loaded) and is non-binding in the eligibility filter — a node with an unknown limit is eligible
but deprioritized. Disk participates in filtering and scoring (and target admission), but is
**scheduling-only**: sbx-go-sdk v0.1.2 `sandbox.Create` has no disk option, so the daemon does not
hard-cap per-sandbox disk yet (the scheduler accounts requested `disk_gb`; `exec.Stats` exposes actual
usage).

Why: ratios make heterogeneous nodes comparable; taking the dominant (most-constrained) resource
prevents packing a node that's fine on cpu but saturated on mem (or vice-versa). The hash tie-break
makes independent coordinators rank the **same** node first for a given request — important in the
leaderless model, where it reduces admission bounces — while varying per request so ties **spread**
across nodes instead of hotspotting the lowest `node_id`.

Considered: single-resource or weighted-sum scoring (simpler/tunable, but mis-handles the constrained
dimension or needs hand-tuned weights); lowest-`node_id` tie-break (deterministic but creates a
hotspot).
