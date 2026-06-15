# Multi-resource scoring by dominant resource, hash tie-break

The scheduler scores each candidate node on its **post-placement dominant-resource ratio**:
`max((allocated.cpu+req.cpu)/limit.cpu, (allocated.mem+req.mem)/limit.mem)`. `least-loaded` minimizes
it, `bin-pack` maximizes it (subject to ≤ 1.0), `spread` minimizes by sandbox count / label
distribution. **Ties are broken by `hash(request_id ⊕ node_id)`.**

Why: ratios make heterogeneous nodes comparable; taking the dominant (most-constrained) resource
prevents packing a node that's fine on cpu but saturated on mem (or vice-versa). The hash tie-break
makes independent coordinators rank the **same** node first for a given request — important in the
leaderless model, where it reduces admission bounces — while varying per request so ties **spread**
across nodes instead of hotspotting the lowest `node_id`.

Considered: single-resource or weighted-sum scoring (simpler/tunable, but mis-handles the constrained
dimension or needs hand-tuned weights); lowest-`node_id` tie-break (deterministic but creates a
hotspot).
