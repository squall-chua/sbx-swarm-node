# Event firehose is a best-effort notification bus, not a durable log

The SSE / `WatchEvents` event stream is a **best-effort live notification bus** — not an event-sourcing
log and not a source of truth. The state of record lives in `bbolt` + the SDK; events are ephemeral,
served from a **bounded per-node replay buffer**. Per-node ordering is total (monotonic id); cross-node
ordering is **best-effort by wall-clock `ts`** (NTP assumed, small skew tolerated). `Last-Event-ID`
resume is same-node best-effort via an opaque compound cursor (per-source high-water marks); a
reconnect to a different node or after buffer rotation is **at-least-once with possible gaps**. Clients
that need certainty **reconcile against state** (`GET`). The **audit log** (git/policy/secret actions)
is kept separate and **durably persisted**.

Why: a globally-ordered, exactly-once, durable event log would require per-node durable logs and
cross-node ordering/consensus — large scope, at odds with the leaderless AP model — for little v1
benefit. The console and reactive clients only need near-real-time notifications and can reconcile
against authoritative state.

Considered: an event-sourced durable log (replayable and ordered, but heavy and needs ordering
guarantees the AP design deliberately avoids).
