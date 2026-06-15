# sbx-swarm-node M2 — Observability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.
>
> **Forward-looking:** depends on M1 types (`sandbox.Backend`/`Manager`, `events.Bus`, `apiserver`, proto). Reconcile signatures against the real M1 code when implementing.

**Goal:** Per-sandbox stats (`exec.Stats`, polled + cached + streamed), continuous logs (`exec.Logs` follow), blocked-egress audit collection (`policy.Log`, distinct pairs), and Prometheus domain metrics.

**Architecture:** Extend `sandbox.Backend` with `Stats`, `Logs`, `BlockedEgress`. A `StatsCollector` polls running sandboxes on an interval with bounded concurrency, caches the latest `Usage`, and computes `actual_util` by reconstructing absolutes (spec §9). A `NetLogCollector` polls `policy.Log`, dedupes to distinct `(host, sandbox)` pairs with synthesized timestamps. Stats/logs/network are exposed as unary + SSE; domain counters feed `/metrics`.

**Tech Stack:** Go 1.23, M1 stack, `golang.org/x/sync/errgroup` (bounded polling).

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/sandbox/backend.go` | add `Usage`, `BlockedHost`, `Stats`/`Logs`/`BlockedEgress` to `Backend` |
| `internal/sandbox/fake.go` | fake implementations |
| `internal/sandbox/sdkbackend.go` | map to `exec.Stats`/`exec.Logs`/`policy.Log` |
| `internal/obsd/stats.go` | `StatsCollector` (poll/cache/actual_util) |
| `internal/obsd/netlog.go` | `NetLogCollector` (poll/diff/accumulate) |
| `internal/obs/metrics.go` | domain Prometheus collectors |
| `internal/apiserver/observe.go` | stats/logs/network unary + SSE handlers |
| `internal/node/node.go` | start collectors; pass to apiserver |

---

## Task 1: Extend Backend with Stats/Logs/BlockedEgress

**Files:** `internal/sandbox/backend.go`, `fake.go`, test `internal/sandbox/observe_fake_test.go`

- [ ] **Step 1: Failing test**

```go
package sandbox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFake_StatsAndBlocked(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	_, _ = f.Create(ctx, CreateSpec{Name: "s1"})

	u, err := f.Stats(ctx, "s1")
	require.NoError(t, err)
	require.Equal(t, 2, u.Cores)

	f.SetBlocked([]BlockedHost{{Host: "evil.example", VMName: "s1"}})
	bl, err := f.BlockedEgress(ctx)
	require.NoError(t, err)
	require.Len(t, bl, 1)
}
```

- [ ] **Step 2: Run → FAIL** (`f.Stats undefined`): `go test ./internal/sandbox/ -run TestFake_StatsAndBlocked -v`

- [ ] **Step 3: Add to `backend.go`**

```go
// Usage is a point-in-time per-sandbox resource snapshot (exec.Stats).
type Usage struct {
	Cores          int
	CPUPercent     float64
	MemTotalKB     int64
	MemUsedKB      int64
	DiskTotalGB    float64
	DiskUsedGB     float64
	UptimeSeconds  int64
}

// BlockedHost is one denied egress attempt (policy.Log): host + sandbox name.
type BlockedHost struct {
	Host   string
	VMName string
}

// LogLine is one streamed log line.
type LogLine struct {
	Line string
	Err  error // set on stream error/EOF
}
```

Add to the `Backend` interface:

```go
	Stats(ctx context.Context, name string) (Usage, error)
	// Logs follows logs at path; lines are sent to out until ctx is done or the
	// stream ends (final LogLine carries Err).
	Logs(ctx context.Context, name, path string, out chan<- LogLine) error
	// BlockedEgress returns the daemon-wide set of blocked (host, vm) pairs.
	BlockedEgress(ctx context.Context) ([]BlockedHost, error)
```

- [ ] **Step 4: Add to `fake.go`**

```go
func (f *Fake) Stats(_ context.Context, name string) (Usage, error) {
	if _, err := f.Get(context.Background(), name); err != nil {
		return Usage{}, err
	}
	return Usage{Cores: 2, CPUPercent: 10, MemTotalKB: 1 << 20, MemUsedKB: 1 << 18}, nil
}

func (f *Fake) Logs(ctx context.Context, name, _ string, out chan<- LogLine) error {
	if _, err := f.Get(ctx, name); err != nil {
		return err
	}
	go func() {
		select {
		case out <- LogLine{Line: "log from " + name}:
		case <-ctx.Done():
		}
	}()
	return nil
}

// SetBlocked sets the fake's blocked-egress list (test helper).
func (f *Fake) SetBlocked(b []BlockedHost) { f.mu.Lock(); f.blocked = b; f.mu.Unlock() }

func (f *Fake) BlockedEgress(_ context.Context) ([]BlockedHost, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]BlockedHost(nil), f.blocked...), nil
}
```

Add `blocked []BlockedHost` to the `Fake` struct.

- [ ] **Step 5: Run → PASS, commit**

```bash
go test ./internal/sandbox/ -v
git add internal/sandbox/ && git commit -m "feat(sandbox): Backend Stats/Logs/BlockedEgress + fake"
```

> SDK adapter (`sdkbackend.go`): map `Stats`→`exec.Stats`, `Logs`→`exec.Logs` (drain the returned `AttachSession` into `out`), `BlockedEgress`→`policy.Log` (flatten `LogResult.BlockedHosts`). Keep the `var _ Backend` assertion green. Verify signatures against the SDK.

---

## Task 2: StatsCollector (poll + cache + actual_util)

**Files:** `internal/obsd/stats.go`, test `internal/obsd/stats_test.go`

- [ ] **Step 1: Failing test**

```go
package obsd

import (
	"context"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/stretchr/testify/require"
)

func TestStatsCollector_PollComputesActualUtil(t *testing.T) {
	f := sandbox.NewFake()
	ctx := context.Background()
	_, _ = f.Create(ctx, sandbox.CreateSpec{Name: "s1"})

	c := NewStatsCollector(f, listFn(f), provisionLimit{CPU: 4, MemKB: 1 << 21}, 4)
	require.NoError(t, c.PollOnce(ctx))

	u, ok := c.Latest("s1")
	require.True(t, ok)
	require.Equal(t, 2, u.Cores)

	au := c.ActualUtil()
	require.InDelta(t, (10.0/100*2)/4, au.CPU, 0.001) // (cpu% * cores)/limit
}

// listFn adapts the fake's List to the names-only signature.
func listFn(f *sandbox.Fake) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		bs, err := f.List(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]string, len(bs))
		for i, b := range bs {
			out[i] = b.Name
		}
		return out, nil
	}
}
```

- [ ] **Step 2: Run → FAIL**: `go test ./internal/obsd/ -run TestStatsCollector -v`

- [ ] **Step 3: Implement `stats.go`**

```go
// Package obsd holds background observability collectors (stats, network log).
package obsd

import (
	"context"
	"sync"

	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"golang.org/x/sync/errgroup"
)

type provisionLimit struct {
	CPU   float64
	MemKB float64
}

// Util is normalized actual utilization (0..1+) vs the provision limit.
type Util struct{ CPU, Mem float64 }

// StatsCollector polls running sandboxes for usage and caches the latest.
type StatsCollector struct {
	backend     sandbox.Backend
	list        func(context.Context) ([]string, error)
	limit       provisionLimit
	concurrency int

	mu     sync.RWMutex
	latest map[string]sandbox.Usage
	util   Util
}

// NewStatsCollector builds a collector polling at most `concurrency` sandboxes at once.
func NewStatsCollector(b sandbox.Backend, list func(context.Context) ([]string, error), limit provisionLimit, concurrency int) *StatsCollector {
	if concurrency <= 0 {
		concurrency = 4
	}
	return &StatsCollector{backend: b, list: list, limit: limit, concurrency: concurrency, latest: map[string]sandbox.Usage{}}
}

// PollOnce polls all sandboxes once (bounded concurrency), updating the cache.
func (c *StatsCollector) PollOnce(ctx context.Context) error {
	names, err := c.list(ctx)
	if err != nil {
		return err
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(c.concurrency)
	var mu sync.Mutex
	got := map[string]sandbox.Usage{}
	for _, name := range names {
		name := name
		g.Go(func() error {
			u, err := c.backend.Stats(gctx, name)
			if err != nil {
				return nil // skip this sandbox; don't fail the whole poll
			}
			mu.Lock()
			got[name] = u
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	var sumCPU, sumMem float64
	for _, u := range got {
		sumCPU += (u.CPUPercent / 100.0) * float64(u.Cores)
		sumMem += float64(u.MemUsedKB)
	}
	c.mu.Lock()
	c.latest = got
	c.util = Util{CPU: safeDiv(sumCPU, c.limit.CPU), Mem: safeDiv(sumMem, c.limit.MemKB)}
	c.mu.Unlock()
	return nil
}

// Latest returns the cached usage for a sandbox.
func (c *StatsCollector) Latest(name string) (sandbox.Usage, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	u, ok := c.latest[name]
	return u, ok
}

// ActualUtil returns the aggregate normalized utilization.
func (c *StatsCollector) ActualUtil() Util {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.util
}

func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}
```

- [ ] **Step 4: Run → PASS** (`go get golang.org/x/sync` first), **commit**

```bash
go get golang.org/x/sync && go mod tidy && go test ./internal/obsd/ -v
git add internal/obsd/stats.go internal/obsd/stats_test.go go.mod go.sum
git commit -m "feat(obsd): stats collector with actual_util reconstruction"
```

> A `Run(ctx, interval)` loop calling `PollOnce` on a ticker (with jitter) is added in Task 5 wiring.

---

## Task 3: NetLogCollector (poll + diff + accumulate distinct pairs)

**Files:** `internal/obsd/netlog.go`, test `internal/obsd/netlog_test.go`

- [ ] **Step 1: Failing test**

```go
package obsd

import (
	"context"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/stretchr/testify/require"
)

func TestNetLog_AccumulatesDistinctPairs(t *testing.T) {
	f := sandbox.NewFake()
	c := NewNetLogCollector(f, func(vm string) (string, bool) { return "sbid-" + vm, true })

	f.SetBlocked([]sandbox.BlockedHost{{Host: "a.com", VMName: "s1"}})
	require.NoError(t, c.PollOnce(context.Background()))
	require.NoError(t, c.PollOnce(context.Background())) // same pair again

	pairs := c.ForSandbox("sbid-s1")
	require.Len(t, pairs, 1) // deduped
	require.Equal(t, "a.com", pairs[0].Host)
	require.Equal(t, 1, c.DistinctCount())
}
```

- [ ] **Step 2: Run → FAIL**: `go test ./internal/obsd/ -run TestNetLog -v`

- [ ] **Step 3: Implement `netlog.go`**

```go
package obsd

import (
	"context"
	"sync"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
)

// BlockedPair is a distinct (host, sandbox) egress denial with synthesized times.
type BlockedPair struct {
	Host      string    `json:"host"`
	SandboxID string    `json:"sandbox_id"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// NetLogCollector polls policy.Log and accumulates distinct blocked pairs.
type NetLogCollector struct {
	backend sandbox.Backend
	resolve func(vmName string) (sandboxID string, ok bool)
	now     func() time.Time

	mu    sync.RWMutex
	pairs map[string]*BlockedPair // key host|sandboxID
}

// NewNetLogCollector builds the collector. resolve maps a backend VM name to a
// swarm sandbox id.
func NewNetLogCollector(b sandbox.Backend, resolve func(string) (string, bool)) *NetLogCollector {
	return &NetLogCollector{backend: b, resolve: resolve, now: time.Now, pairs: map[string]*BlockedPair{}}
}

// PollOnce reads the current blocked set and merges it (first/last seen).
func (c *NetLogCollector) PollOnce(ctx context.Context) error {
	cur, err := c.backend.BlockedEgress(ctx)
	if err != nil {
		return err
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, b := range cur {
		sbID, ok := c.resolve(b.VMName)
		if !ok {
			sbID = b.VMName
		}
		key := b.Host + "|" + sbID
		if p, exists := c.pairs[key]; exists {
			p.LastSeen = now
		} else {
			c.pairs[key] = &BlockedPair{Host: b.Host, SandboxID: sbID, FirstSeen: now, LastSeen: now}
		}
	}
	return nil
}

// ForSandbox returns the blocked pairs for a sandbox.
func (c *NetLogCollector) ForSandbox(sandboxID string) []BlockedPair {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []BlockedPair
	for _, p := range c.pairs {
		if p.SandboxID == sandboxID {
			out = append(out, *p)
		}
	}
	return out
}

// DistinctCount returns the number of distinct pairs (the only valid aggregate).
func (c *NetLogCollector) DistinctCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.pairs)
}
```

- [ ] **Step 4: Run → PASS, commit**

```bash
go test ./internal/obsd/ -v
git add internal/obsd/netlog.go internal/obsd/netlog_test.go
git commit -m "feat(obsd): blocked-egress collector (distinct pairs, no fake counts)"
```

---

## Task 4: Domain Prometheus metrics

**Files:** `internal/obs/metrics.go`, test `internal/obs/metrics_test.go`

- [ ] **Step 1: Failing test**

```go
package obs

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func TestMetrics_SandboxGauge(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.SetSandboxes("running", 3)

	require.Equal(t, 3, testutil.CollectAndCount(m.sandboxes))
	_ = reg
}
```

- [ ] **Step 2: Run → FAIL**: `go test ./internal/obs/ -run TestMetrics -v`

- [ ] **Step 3: Implement `metrics.go`**

```go
package obs

import "github.com/prometheus/client_golang/prometheus"

// Metrics holds the node's domain metrics.
type Metrics struct {
	sandboxes  *prometheus.GaugeVec
	opsTotal   *prometheus.CounterVec
}

// NewMetrics registers and returns the domain metrics.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		sandboxes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sbx_sandboxes", Help: "Sandboxes by status.",
		}, []string{"status"}),
		opsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sbx_operations_total", Help: "Operations by type and final state.",
		}, []string{"type", "state"}),
	}
	reg.MustRegister(m.sandboxes, m.opsTotal)
	return m
}

// SetSandboxes sets the gauge for a status.
func (m *Metrics) SetSandboxes(status string, n int) { m.sandboxes.WithLabelValues(status).Set(float64(n)) }

// IncOp counts a completed operation.
func (m *Metrics) IncOp(opType, state string) { m.opsTotal.WithLabelValues(opType, state).Inc() }
```

- [ ] **Step 4: Run → PASS, commit**

```bash
go test ./internal/obs/ -v
git add internal/obs/metrics.go internal/obs/metrics_test.go
git commit -m "feat(obs): domain prometheus metrics"
```

---

## Task 5: Observe API (stats/logs/network) + wiring

**Files:** `proto/sbxswarm/v1/sandbox.proto` (extend), `internal/apiserver/observe.go`, `internal/node/node.go`

- [ ] **Step 1: Add proto RPCs + regenerate**

Add to `SandboxService`:

```proto
  rpc GetStats(IdRequest) returns (Stats) {
    option (google.api.http) = {get: "/v1/sandboxes/{id}/stats"};
  }
  rpc ListBlocked(IdRequest) returns (ListBlockedResponse) {
    option (google.api.http) = {get: "/v1/sandboxes/{id}/network/blocked"};
  }
```

```proto
message Stats {
  int32 cores = 1;
  double cpu_percent = 2;
  int64 mem_total_kb = 3;
  int64 mem_used_kb = 4;
}
message Blocked { string host = 1; string first_seen = 2; string last_seen = 3; }
message ListBlockedResponse { repeated Blocked blocked = 1; int32 distinct_count = 2; }
```

Run: `buf generate && go build ./...`

- [ ] **Step 2: Implement unary handlers + SSE in `observe.go`** (TDD: write a test calling `GetStats`/`ListBlocked` against a service backed by the collectors with a fake; then implement)

```go
package apiserver

import (
	"context"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/obsd"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ObserveDeps are the collectors the observe RPCs read from.
type ObserveDeps struct {
	Stats  *obsd.StatsCollector
	NetLog *obsd.NetLogCollector
	Mgr    *sandbox.Manager
}

// GetStats returns the latest cached usage for a sandbox (?fresh handled in M2+).
func (s *SandboxService) GetStats(ctx context.Context, r *sbxv1.IdRequest) (*sbxv1.Stats, error) {
	name, err := s.mgr.Resolve(ctx, r.Id)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	u, ok := s.obs.Stats.Latest(name)
	if !ok {
		return &sbxv1.Stats{}, nil
	}
	return &sbxv1.Stats{Cores: int32(u.Cores), CpuPercent: u.CPUPercent, MemTotalKb: u.MemTotalKB, MemUsedKb: u.MemUsedKB}, nil
}

// ListBlocked returns the distinct blocked-egress pairs for a sandbox.
func (s *SandboxService) ListBlocked(_ context.Context, r *sbxv1.IdRequest) (*sbxv1.ListBlockedResponse, error) {
	pairs := s.obs.NetLog.ForSandbox(r.Id)
	out := &sbxv1.ListBlockedResponse{DistinctCount: int32(s.obs.NetLog.DistinctCount())}
	for _, p := range pairs {
		out.Blocked = append(out.Blocked, &sbxv1.Blocked{Host: p.Host, FirstSeen: p.FirstSeen.Format("2006-01-02T15:04:05Z07:00"), LastSeen: p.LastSeen.Format("2006-01-02T15:04:05Z07:00")})
	}
	return out, nil
}
```

Add an `obs ObserveDeps` field to `SandboxService` and an `WithObserve(ObserveDeps)` setter; guard nil in handlers (return Unimplemented if `s.obs.Stats == nil`). Logs/network SSE reuse the M1d `events.Bus` pattern: a `GET /v1/sandboxes/{id}/logs` handler that subscribes to `backend.Logs` lines and writes SSE frames; a `GET /v1/sandboxes/{id}/stats` SSE handler emitting the cache on a ticker. Register these on the rest mux in `Build` when `Events`/collectors are present.

- [ ] **Step 3: Wire collectors + ticker loops in `node.New`**

```go
	statsC := obsd.NewStatsCollector(backend, namesList(mgr), provisionLimitFromCfg(cfg), 4)
	netC := obsd.NewNetLogCollector(backend, mgr.ResolveVMToID)
	// background loops (store cancel in Node for Stop):
	go runTicker(n.ctx, 10*time.Second, func() { _ = statsC.PollOnce(n.ctx) })
	go runTicker(n.ctx, 15*time.Second, func() { _ = netC.PollOnce(n.ctx) })
	sandboxes.WithObserve(apiserver.ObserveDeps{Stats: statsC, NetLog: netC, Mgr: mgr})
```

Add `mgr.ResolveVMToID(vm string)(string,bool)` (reverse of `Resolve`, scanning records), a `runTicker` helper, and a node-level `ctx`/`cancel` for the loops (cancel in `Stop`).

- [ ] **Step 4: Run all tests + manual**

Run: `go test ./...`
Manual: create a sandbox, `curl -sk -N -H "Authorization: Bearer adm" https://localhost:8443/v1/sandboxes/<id>/logs` streams a line; `.../stats` returns cached usage.

- [ ] **Step 5: Commit**

```bash
git add proto/ internal/gen/ internal/apiserver/observe.go internal/node/
git commit -m "feat(observe): stats/logs/network API + background collectors"
```

---

## Self-Review

**Spec coverage (M2):** per-sandbox stats poll+cache+`actual_util` reconstruction (§9) → Tasks 1,2 ✓; logs follow (`exec.Logs`) → Tasks 1,5 ✓; blocked-egress distinct-pair audit (no fake counts) → Tasks 1,3,5 ✓; Prometheus domain metrics → Task 4 ✓; SSE streaming reuses M1d → Task 5 ✓. **Deferred:** `?fresh=true` forced probe (note in GetStats) and gRPC server-stream variants — additive once the unary + SSE paths are in.

**Placeholder scan:** Task 5 references concrete helpers (`namesList`, `runTicker`, `ResolveVMToID`, `provisionLimitFromCfg`) — these are small and specified by behavior; implement as described. The SDK adapter passthroughs are guarded by the `var _ Backend` assertion. No TBD/TODO.

**Type consistency:** `sandbox.Usage`/`BlockedHost`/`LogLine`; `obsd.NewStatsCollector(...).{PollOnce,Latest,ActualUtil}`; `obsd.NewNetLogCollector(...).{PollOnce,ForSandbox,DistinctCount}`; `obs.NewMetrics(reg).{SetSandboxes,IncOp}`. Proto `Stats`/`Blocked` match handlers.
