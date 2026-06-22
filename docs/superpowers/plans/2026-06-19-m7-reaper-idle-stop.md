# M7 Reaper / Idle-stop Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A per-node background sweep that idle-stops sandboxes after a configurable timeout (auto-publishing git-backed ones first), plus a `KeepAlive` RPC so consumers can keep a sandbox alive on demand.

**Architecture:** A new `Record.LastActivity` timestamp is bumped by control-plane interactions (Create/Start/Exec/AgentRun/KeepAlive) and by observed CPU (the existing 10s stats poll). `Manager.IdleRunning(now, timeout)` selects running, non-exempt records past the timeout; `SandboxService.ReapIdle(now)` publishes-then-stops them. A dedicated ticker in `node.go` runs the sweep only when `idle_timeout > 0`. Idle-stop **stops, never deletes** (a stopped sandbox still counts against reserved capacity).

**Tech Stack:** Go 1.25, gRPC + grpc-gateway (buf), bbolt KV store, testify, in-memory `Fake` backend for tests.

## Global Constraints

- **Go 1.25.** Build with the `go` toolchain; after `buf generate`, gopls may show **false** "undefined/redeclared" errors — trust `go build ./...` / `go vet ./...`, not the editor.
- **Codegen flow:** edit `proto/sbxswarm/v1/sandbox.proto` → `buf generate` → `go build ./...` → commit the regenerated `internal/gen/sbxswarm/v1/*` (git-tracked). `go build` does NOT compile tests; run `go vet ./...` / `go test ./...` to catch test breakage.
- **Any new gRPC method MUST be classified** in `internal/apiserver/authz.go` (`mutatingMethods` / `readMethods` / `internalMethods`), or `TestAuthz_AllMethodsClassified` fails.
- **REST gateway marshaler is `UseProtoNames`** (snake_case JSON). New REST routes stay snake_case.
- **`gofmt` realigns struct field-comment columns** when you add a field — expect a slightly wider diff; it is not you reformatting.
- **Idle-stop = stop, never delete.** A `"stopped"` record still counts against this node's provision capacity (`costSum` excludes only `"lost"`). Do not change `costSum`.
- **Activity = control-plane interaction OR observed CPU** (ADR-0016). `LastActivity` is the single timestamp; idle is `now - LastActivity > timeout` (strict `>`), running records only, skipping `Labels["idle-stop"] == "off"`.
- **No remote on this repo:** "merge" = local ff-merge; the user drives merges — do not merge to `main`. Work stays on branch `m7-reaper-idle-stop`. Interactive rebase / `git add -i` are unavailable.

Reference spec: [docs/superpowers/specs/2026-06-19-m7-reaper-idle-stop-design.md](../specs/2026-06-19-m7-reaper-idle-stop-design.md) · ADR: [docs/adr/0016-idle-stop-activity-signal.md](../../adr/0016-idle-stop-activity-signal.md)

## File Structure

| File | Responsibility | Tasks |
|---|---|---|
| `internal/sandbox/record.go` | add `LastActivity` field | 1 |
| `internal/sandbox/manager.go` | `Create` stamps `LastActivity`; `BumpActivity`; `Start` bumps; `IdleRunning`; `CreateSpec.Labels`→`rec.Labels` | 1, 2, 3 |
| `internal/sandbox/backend.go` | add `CreateSpec.Labels` | 2 |
| `internal/sandbox/manager_test.go` | activity + idle-selector tests | 1, 3 |
| `internal/config/config.go` | `IdleTimeout`, `Validate`, `IdleTimeoutDuration` | 4 |
| `internal/config/config_test.go` | config tests | 4 |
| `internal/apiserver/sandboxservice.go` | `toSpec` maps labels; `SetIdleTimeout`; Exec/AgentRun bumps; `ReapIdle`; `KeepAlive` | 2, 5, 6, 7 |
| `internal/apiserver/sandboxservice_test.go` | labels round-trip, Exec-bump, ReapIdle, KeepAlive tests | 2, 5, 6, 7 |
| `proto/sbxswarm/v1/sandbox.proto` + `internal/gen/...` | `KeepAlive` RPC | 7 |
| `internal/apiserver/authz.go` | classify `KeepAlive` mutating | 7 |
| `internal/apiserver/forward.go` | `KeepAlive` reply in `newReplyFor` | 7 |
| `internal/node/node.go` | `SetIdleTimeout`, reaper ticker, CPU-as-activity bump, `reapInterval` | 8 |
| `internal/node/node_test.go` | `reapInterval` + boot-with-idle smoke | 8 |

---

### Task 1: `Record.LastActivity` + activity bumps in the Manager

**Files:**
- Modify: `internal/sandbox/record.go`
- Modify: `internal/sandbox/manager.go` (`Create` ~133-153, `Start` ~202-204; add `BumpActivity`)
- Test: `internal/sandbox/manager_test.go`

**Interfaces:**
- Produces: `Record.LastActivity time.Time`; `func (m *Manager) BumpActivity(ctx context.Context, id string) error`; `Manager.Create` and `Manager.Start` set/bump `LastActivity` via `m.now()`.

- [ ] **Step 1: Write the failing test**

Add to `internal/sandbox/manager_test.go`:

```go
func TestManager_LastActivity_StampAndBump(t *testing.T) {
	m, _ := newMgr(t)
	t0 := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return t0 } // unexported field, same package
	ctx := context.Background()

	rec, err := m.Create(ctx, CreateSpec{})
	require.NoError(t, err)
	require.Equal(t, t0, rec.LastActivity, "Create stamps LastActivity")

	t1 := t0.Add(time.Hour)
	m.now = func() time.Time { return t1 }
	require.NoError(t, m.BumpActivity(ctx, rec.ID))
	got, err := m.Get(ctx, rec.ID)
	require.NoError(t, err)
	require.Equal(t, t1, got.LastActivity, "BumpActivity advances LastActivity")

	require.ErrorIs(t, m.BumpActivity(ctx, "n1.missing"), ErrNotFound)
}

func TestManager_Start_BumpsActivity(t *testing.T) {
	m, _ := newMgr(t)
	t0 := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return t0 }
	ctx := context.Background()
	rec, err := m.Create(ctx, CreateSpec{})
	require.NoError(t, err)
	require.NoError(t, m.Stop(ctx, rec.ID))

	t1 := t0.Add(2 * time.Hour)
	m.now = func() time.Time { return t1 }
	require.NoError(t, m.Start(ctx, rec.ID))
	got, err := m.Get(ctx, rec.ID)
	require.NoError(t, err)
	require.Equal(t, t1, got.LastActivity, "Start counts as Activity")
}
```

Add `"time"` to the test imports if not present.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/sandbox/ -run 'TestManager_LastActivity_StampAndBump|TestManager_Start_BumpsActivity' -v`
Expected: FAIL — `rec.LastActivity` is zero (field missing / not stamped), `BumpActivity` undefined.

- [ ] **Step 3: Add the field**

In `internal/sandbox/record.go`, add to `Record` (after `UpdatedAt`):

```go
	UpdatedAt    time.Time         `json:"updated_at"`
	LastActivity time.Time         `json:"last_activity,omitempty"`
	LastPublish  time.Time         `json:"last_publish,omitempty"`
```

- [ ] **Step 4: Stamp + bump in the Manager**

In `internal/sandbox/manager.go` `Create`, replace the record literal so a single `now` stamps both timestamps:

```go
	now := m.now()
	rec := &Record{
		ID: id, BackendName: backendName, OwnerNode: m.nodeID,
		Spec: spec, Status: bs.Status, CreatedAt: now, LastActivity: now,
	}
```

Add the `BumpActivity` method (near `SetLastPublish`):

```go
// BumpActivity records that the sandbox was just used (control-plane Activity),
// resetting its idle clock. Returns ErrNotFound if the sandbox is gone.
func (m *Manager) BumpActivity(ctx context.Context, id string) error {
	rec, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	rec.LastActivity = m.now()
	return m.save(rec)
}
```

Make `Start` count as Activity (replace the one-line `Start`):

```go
func (m *Manager) Start(ctx context.Context, id string) error {
	if err := m.lifecycle(ctx, id, func(n string) error { return m.backend.Start(ctx, n) }, "running"); err != nil {
		return err
	}
	return m.BumpActivity(ctx, id) // Start is Activity (prevents immediate re-reap)
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/sandbox/ -run 'TestManager_LastActivity_StampAndBump|TestManager_Start_BumpsActivity' -v`
Expected: PASS.

- [ ] **Step 6: Full package + commit**

Run: `go test ./internal/sandbox/ && go vet ./internal/sandbox/`
Expected: PASS.

```bash
git add internal/sandbox/record.go internal/sandbox/manager.go internal/sandbox/manager_test.go
git commit -m "feat(sandbox): Record.LastActivity + BumpActivity (Create/Start stamp it)"
```

---

### Task 2: Persist sandbox labels (CreateSpec.Labels → Record.Labels)

**Files:**
- Modify: `internal/sandbox/backend.go` (`CreateSpec` ~19-29)
- Modify: `internal/sandbox/manager.go` (`Create` record literal)
- Modify: `internal/apiserver/sandboxservice.go` (`toSpec` ~135-144)
- Test: `internal/apiserver/sandboxservice_test.go`

**Interfaces:**
- Consumes: `Record.Labels` (already exists), `toProto` (already copies `rec.Labels`).
- Produces: `CreateSpec.Labels map[string]string`; `toSpec` maps `r.Labels`; `Manager.Create` sets `rec.Labels = spec.Labels`. (Task 3's `IdleRunning` relies on `rec.Labels` being populated.)

- [ ] **Step 1: Write the failing test**

Add to `internal/apiserver/sandboxservice_test.go`:

```go
func TestCreate_PersistsLabels(t *testing.T) {
	svc := newSandboxSvc(t)
	ctx := context.Background()
	rec, err := svc.mgr.Create(ctx, sandbox.CreateSpec{Labels: map[string]string{"idle-stop": "off"}})
	require.NoError(t, err)

	got, err := svc.GetSandbox(ctx, &sbxv1.GetSandboxRequest{Id: rec.ID})
	require.NoError(t, err)
	require.Equal(t, "off", got.Labels["idle-stop"], "labels persist through Create and toProto")

	spec := toSpec(&sbxv1.CreateSandboxRequest{Labels: map[string]string{"team": "eng"}})
	require.Equal(t, "eng", spec.Labels["team"], "toSpec carries request labels")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/apiserver/ -run TestCreate_PersistsLabels -v`
Expected: FAIL — `CreateSpec` has no `Labels` field (compile error).

- [ ] **Step 3: Add `Labels` to `CreateSpec`**

In `internal/sandbox/backend.go`, add to `CreateSpec` (after `Env`):

```go
	Env         map[string]string
	Labels      map[string]string // sandbox's own labels (e.g. idle-stop: off)
```

- [ ] **Step 4: Map labels in `toSpec` and persist in `Create`**

In `internal/apiserver/sandboxservice.go` `toSpec`, add `Labels` to the returned struct:

```go
	return sandbox.CreateSpec{
		Agent: r.Agent, Template: r.Template, CPUs: int(r.Cpus),
		MemoryBytes: r.MemoryBytes, DiskGB: r.DiskGb, Clone: r.Clone, Branch: r.Branch, Workspaces: ws, Env: r.Env, Labels: r.Labels,
	}
```

In `internal/sandbox/manager.go` `Create`, add `Labels` to the record literal (alongside the Task 1 change):

```go
	rec := &Record{
		ID: id, BackendName: backendName, OwnerNode: m.nodeID,
		Spec: spec, Status: bs.Status, CreatedAt: now, LastActivity: now, Labels: spec.Labels,
	}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/apiserver/ -run TestCreate_PersistsLabels -v`
Expected: PASS.

- [ ] **Step 6: Build + commit**

Run: `go build ./... && go test ./internal/sandbox/ ./internal/apiserver/`
Expected: PASS.

```bash
git add internal/sandbox/backend.go internal/sandbox/manager.go internal/apiserver/sandboxservice.go internal/apiserver/sandboxservice_test.go
git commit -m "feat(sandbox): persist sandbox labels (CreateSpec.Labels -> Record.Labels)"
```

---

### Task 3: `Manager.IdleRunning` selector

**Files:**
- Modify: `internal/sandbox/manager.go` (add `IdleRunning`)
- Test: `internal/sandbox/manager_test.go`

**Interfaces:**
- Consumes: `Record.LastActivity`, `Record.Labels`, `Record.Status` (Tasks 1-2).
- Produces: `func (m *Manager) IdleRunning(ctx context.Context, now time.Time, timeout time.Duration) ([]*Record, error)` — running, non-exempt records with `now - LastActivity > timeout`.

- [ ] **Step 1: Write the failing test**

Add to `internal/sandbox/manager_test.go`:

```go
func TestManager_IdleRunning(t *testing.T) {
	m, _ := newMgr(t)
	t0 := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return t0 }
	ctx := context.Background()
	timeout := time.Hour

	active, err := m.Create(ctx, CreateSpec{})
	require.NoError(t, err)
	exempt, err := m.Create(ctx, CreateSpec{Labels: map[string]string{"idle-stop": "off"}})
	require.NoError(t, err)
	stopped, err := m.Create(ctx, CreateSpec{})
	require.NoError(t, err)
	require.NoError(t, m.Stop(ctx, stopped.ID))

	// Exactly at the boundary: not idle (strict >).
	require.Empty(t, mustIdle(t, m, t0.Add(timeout), timeout))

	// Past the boundary: only the plain running sandbox is idle.
	idle := mustIdle(t, m, t0.Add(timeout+time.Nanosecond), timeout)
	require.Len(t, idle, 1)
	require.Equal(t, active.ID, idle[0].ID)
	require.NotContains(t, idsOf(idle), exempt.ID, "idle-stop:off is exempt")
	require.NotContains(t, idsOf(idle), stopped.ID, "stopped is never idle-running")

	// Re-reap regression: Start the would-be-idle sandbox far in the future; it must
	// no longer be selected at that same now (Start bumped LastActivity).
	m.now = func() time.Time { return t0.Add(2 * timeout) }
	require.NoError(t, m.Stop(ctx, active.ID))
	require.NoError(t, m.Start(ctx, active.ID))
	require.Empty(t, mustIdle(t, m, t0.Add(2*timeout), timeout), "Started sandbox is not immediately idle")
}

func mustIdle(t *testing.T, m *Manager, now time.Time, timeout time.Duration) []*Record {
	t.Helper()
	out, err := m.IdleRunning(context.Background(), now, timeout)
	require.NoError(t, err)
	return out
}

func idsOf(recs []*Record) []string {
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, r.ID)
	}
	return ids
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/sandbox/ -run TestManager_IdleRunning -v`
Expected: FAIL — `IdleRunning` undefined.

- [ ] **Step 3: Implement `IdleRunning`**

In `internal/sandbox/manager.go` (after `List`):

```go
// IdleRunning returns running, non-exempt records whose last Activity precedes
// now-timeout (strict >). A record labeled idle-stop:off is exempt. timeout<=0
// returns nothing. now is a parameter so the boundary is deterministically testable.
func (m *Manager) IdleRunning(ctx context.Context, now time.Time, timeout time.Duration) ([]*Record, error) {
	if timeout <= 0 {
		return nil, nil
	}
	recs, err := m.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []*Record
	for _, rec := range recs {
		if rec.Status != "running" || rec.Labels["idle-stop"] == "off" {
			continue
		}
		if now.Sub(rec.LastActivity) > timeout {
			out = append(out, rec)
		}
	}
	return out, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/sandbox/ -run TestManager_IdleRunning -v`
Expected: PASS.

- [ ] **Step 5: Commit**

Run: `go test ./internal/sandbox/`
Expected: PASS.

```bash
git add internal/sandbox/manager.go internal/sandbox/manager_test.go
git commit -m "feat(sandbox): Manager.IdleRunning idle selector (strict >, exempt label, re-reap fix)"
```

---

### Task 4: Config `idle_timeout`

**Files:**
- Modify: `internal/config/config.go` (`Config` struct ~15-35, `Validate` ~184-235; add `IdleTimeoutDuration`)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `Config.IdleTimeout string` (yaml `idle_timeout`); `func (c *Config) IdleTimeoutDuration() time.Duration` (`""`→0); `Validate` rejects unparseable/negative durations.

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config_test.go`:

```go
func TestIdleTimeout_ValidateAndDuration(t *testing.T) {
	c := Default()
	require.Equal(t, "", c.IdleTimeout) // disabled by default
	require.Equal(t, time.Duration(0), c.IdleTimeoutDuration())

	c.IdleTimeout = "30m"
	require.NoError(t, c.Validate())
	require.Equal(t, 30*time.Minute, c.IdleTimeoutDuration())

	c.IdleTimeout = "garbage"
	require.Error(t, c.Validate())

	c.IdleTimeout = "-5m"
	require.Error(t, c.Validate())
}
```

Add `"time"` to the test imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestIdleTimeout_ValidateAndDuration -v`
Expected: FAIL — `IdleTimeout` / `IdleTimeoutDuration` undefined.

- [ ] **Step 3: Add the field, validation, and accessor**

In `internal/config/config.go`, add `"time"` to imports. Add the field to `Config` (after `Backend`):

```go
	Backend                 string            `yaml:"backend"` // "fake" (default) | "sdk"
	IdleTimeout             string            `yaml:"idle_timeout"` // Go duration, e.g. "30m"; "" or <=0 disables idle-stop
```

In `Validate`, before the final `return nil`:

```go
	if c.IdleTimeout != "" {
		d, err := time.ParseDuration(c.IdleTimeout)
		if err != nil {
			return fmt.Errorf("idle_timeout: %w", err)
		}
		if d < 0 {
			return fmt.Errorf("idle_timeout must not be negative, got %q", c.IdleTimeout)
		}
	}
```

Add the accessor (after `Validate`):

```go
// IdleTimeoutDuration parses IdleTimeout (already validated; "" yields 0).
func (c *Config) IdleTimeoutDuration() time.Duration {
	d, _ := time.ParseDuration(c.IdleTimeout)
	return d
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -run TestIdleTimeout_ValidateAndDuration -v`
Expected: PASS.

- [ ] **Step 5: Commit**

Run: `go test ./internal/config/`
Expected: PASS.

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): idle_timeout knob (validated Go duration; \"\" disables)"
```

---

### Task 5: SandboxService activity bumps + `SetIdleTimeout`

**Files:**
- Modify: `internal/apiserver/sandboxservice.go` (struct ~26-37; `Exec` ~282-292; `AgentRun` ~294-329; add `SetIdleTimeout`)
- Test: `internal/apiserver/sandboxservice_test.go`

**Interfaces:**
- Consumes: `Manager.BumpActivity` (Task 1).
- Produces: `SandboxService.idleTimeout time.Duration`; `func (s *SandboxService) SetIdleTimeout(d time.Duration)`; `Exec` and `AgentRun` bump activity (AgentRun also throttle-bumps in its poll loop).

- [ ] **Step 1: Write the failing test**

Add to `internal/apiserver/sandboxservice_test.go`:

```go
func TestExec_BumpsActivity(t *testing.T) {
	svc := newSandboxSvc(t)
	ctx := context.Background()
	rec, err := svc.mgr.Create(ctx, sandbox.CreateSpec{})
	require.NoError(t, err)
	before := rec.LastActivity

	_, err = svc.Exec(ctx, &sbxv1.ExecRequest{Id: rec.ID, Cmd: []string{"echo", "hi"}})
	require.NoError(t, err)

	got, err := svc.mgr.Get(ctx, rec.ID)
	require.NoError(t, err)
	require.True(t, got.LastActivity.After(before), "Exec bumps LastActivity")
}

func TestSetIdleTimeout(t *testing.T) {
	svc := newSandboxSvc(t)
	svc.SetIdleTimeout(15 * time.Minute)
	require.Equal(t, 15*time.Minute, svc.idleTimeout)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/apiserver/ -run 'TestExec_BumpsActivity|TestSetIdleTimeout' -v`
Expected: FAIL — `SetIdleTimeout` undefined; `Exec` does not yet bump.

- [ ] **Step 3: Add the field + setter**

In `internal/apiserver/sandboxservice.go`, add to the `SandboxService` struct (after `events`):

```go
	events           events.Publisher
	idleTimeout      time.Duration
```

Add the setter (near `SetEvents`):

```go
// SetIdleTimeout configures the idle-stop threshold. 0 disables both the reaper
// sweep and the agent-run keepalive throttle.
func (s *SandboxService) SetIdleTimeout(d time.Duration) { s.idleTimeout = d }
```

- [ ] **Step 4: Bump on Exec and AgentRun**

In `Exec`, after the `Resolve` error check (before the `mgr.Backend().Exec` call), add:

```go
	_ = s.mgr.BumpActivity(ctx, r.Id) // Exec is Activity
```

In `AgentRun`, bump at the start of the run closure and throttle inside the poll loop. Replace the `s.ops.Run(op.ID, func() (string, error) { ... })` body with:

```go
	s.ops.Run(op.ID, func() (string, error) {
		_ = s.mgr.BumpActivity(context.Background(), sbID) // run started = Activity
		did, derr := s.mgr.Backend().ExecDetached(context.Background(), name, cmd, opts)
		if derr != nil {
			return "", derr
		}
		lastTouch := time.Now()
		for { // poll to completion (M1c: simple loop; M1d streams progress)
			st, perr := s.mgr.Backend().PollDetached(context.Background(), name, did)
			if perr != nil {
				return "", perr
			}
			if st.Done {
				if st.ExitCode != 0 {
					return sbID, status.Errorf(codes.Internal, "agent run exited %d", st.ExitCode)
				}
				if publishOnSuccess {
					s.maybeAutoPublish(context.Background(), sbID) // best-effort
				}
				return sbID, nil
			}
			// Keep a long-running agent's sandbox alive: bump on a timeout/2 throttle
			// so it is never idle-stopped mid-run (skip when the reaper is disabled).
			if s.idleTimeout > 0 && time.Since(lastTouch) > s.idleTimeout/2 {
				_ = s.mgr.BumpActivity(context.Background(), sbID)
				lastTouch = time.Now()
			}
			time.Sleep(200 * time.Millisecond)
		}
	})
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/apiserver/ -run 'TestExec_BumpsActivity|TestSetIdleTimeout' -v`
Expected: PASS.

- [ ] **Step 6: Full package + commit**

Run: `go test ./internal/apiserver/ && go vet ./internal/apiserver/`
Expected: PASS.

```bash
git add internal/apiserver/sandboxservice.go internal/apiserver/sandboxservice_test.go
git commit -m "feat(apiserver): bump Activity on Exec/AgentRun + SetIdleTimeout (timeout/2 keepalive throttle)"
```

---

### Task 6: `SandboxService.ReapIdle`

**Files:**
- Modify: `internal/apiserver/sandboxservice.go` (add `ReapIdle`)
- Test: `internal/apiserver/sandboxservice_test.go`

**Interfaces:**
- Consumes: `Manager.IdleRunning` (Task 3), `maybeAutoPublish` (existing), `Manager.Stop`, `s.idleTimeout` (Task 5).
- Produces: `func (s *SandboxService) ReapIdle(ctx context.Context, now time.Time) int` — publishes-then-stops each idle sandbox, returns the count stopped.

- [ ] **Step 1: Write the failing test**

Add to `internal/apiserver/sandboxservice_test.go`. This mirrors `TestStopSandbox_AutoPublishesThenStops` (the git harness), then drives `ReapIdle` instead of `StopSandbox`:

```go
func TestReapIdle_PublishesThenStops(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	upstream := filepath.Join(root, "up.git")
	base := filepath.Join(root, "base.git")
	sbx := filepath.Join(root, "sbx")
	for _, c := range [][]string{
		{"git", "init", "--bare", upstream},
		{"git", "clone", upstream, sbx},
	} {
		out, err := exec.Command(c[0], c[1:]...).CombinedOutput()
		require.NoError(t, err, string(out))
	}
	run := func(dir string, a ...string) {
		c := exec.Command("git", a...)
		c.Dir = dir
		out, err := c.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	run(sbx, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "--allow-empty", "-m", "init")
	run(sbx, "push", "origin", "HEAD:main")
	out, err := exec.Command("git", "clone", "--bare", upstream, base).CombinedOutput()
	require.NoError(t, err, string(out))
	run(sbx, "checkout", "-b", "agent/x")
	run(sbx, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "--allow-empty", "-m", "work")

	ws := git.New(git.Spec{
		Name: "repo", Base: base, Remote: "origin", DefaultBranch: "main", AllowPush: true,
		PreSteps:     [][]string{{"git", "fetch", "{remote}", "+refs/heads/*:refs/heads/*"}},
		PublishSteps: [][]string{{"git", "fetch", "{sandbox_remote}", "+refs/heads/{branch}:refs/heads/{branch}"}, {"git", "push", "{remote}", "{branch}"}},
		Allowlist:    []string{"git"},
	})

	mgr := newTestManager(t)
	rec, err := mgr.AdmitAndCreate(context.Background(), sandbox.CreateSpec{
		Agent: "shell", Clone: true, Branch: "agent/x", Workspaces: []sandbox.WorkspaceMount{{Name: "repo"}},
	})
	require.NoError(t, err)
	addRemote := exec.Command("git", "remote", "add", "sandbox-"+rec.BackendName, sbx)
	addRemote.Dir = base
	out, err = addRemote.CombinedOutput()
	require.NoError(t, err, string(out))

	svc := NewSandboxService(mgr, newTestOps(t))
	svc.SetGit(map[string]*git.Workspace{"repo": ws})
	svc.SetIdleTimeout(time.Hour)

	// Not yet idle: now == create time, elapsed ~0 < 1h.
	require.Equal(t, 0, svc.ReapIdle(context.Background(), time.Now()))
	got, _ := mgr.Get(context.Background(), rec.ID)
	require.Equal(t, "running", got.Status)

	// Idle: now far past the timeout.
	require.Equal(t, 1, svc.ReapIdle(context.Background(), time.Now().Add(2*time.Hour)))

	bo, _ := func() ([]byte, error) { c := exec.Command("git", "branch", "--list", "agent/x"); c.Dir = upstream; return c.CombinedOutput() }()
	require.Contains(t, string(bo), "agent/x", "publish ran before stop")
	got, err = mgr.Get(context.Background(), rec.ID)
	require.NoError(t, err)
	require.Equal(t, "stopped", got.Status)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/apiserver/ -run TestReapIdle_PublishesThenStops -v`
Expected: FAIL — `ReapIdle` undefined.

- [ ] **Step 3: Implement `ReapIdle`**

In `internal/apiserver/sandboxservice.go` (after `maybeAutoPublish`):

```go
// ReapIdle idle-stops every running, non-exempt sandbox past the idle timeout,
// auto-publishing git-backed ones first (publish-before-stop: the live daemon is
// needed for the sandbox-<name> fetch). now is a parameter for testability.
// Returns the number stopped. A publish failure does NOT skip the stop (parity
// with graceful StopSandbox).
func (s *SandboxService) ReapIdle(ctx context.Context, now time.Time) int {
	idle, err := s.mgr.IdleRunning(ctx, now, s.idleTimeout)
	if err != nil {
		slog.Warn("reaper: list idle failed", "err", err)
		return 0
	}
	n := 0
	for _, rec := range idle {
		s.maybeAutoPublish(ctx, rec.ID) // best-effort, before stop
		if serr := s.mgr.Stop(ctx, rec.ID); serr != nil {
			slog.Warn("reaper: stop failed", "sandbox", rec.ID, "err", serr)
			continue
		}
		slog.Info("idle-stopped sandbox", "sandbox", rec.ID, "idle", now.Sub(rec.LastActivity).String())
		n++
	}
	return n
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/apiserver/ -run TestReapIdle_PublishesThenStops -v`
Expected: PASS (or SKIP if git is unavailable).

- [ ] **Step 5: Commit**

Run: `go test ./internal/apiserver/`
Expected: PASS.

```bash
git add internal/apiserver/sandboxservice.go internal/apiserver/sandboxservice_test.go
git commit -m "feat(apiserver): ReapIdle publishes-then-stops idle sandboxes"
```

---

### Task 7: `KeepAlive` RPC

**Files:**
- Modify: `proto/sbxswarm/v1/sandbox.proto` (service block ~46-48)
- Regenerate: `internal/gen/sbxswarm/v1/*` (via `buf generate`)
- Modify: `internal/apiserver/sandboxservice.go` (add `KeepAlive` handler)
- Modify: `internal/apiserver/authz.go` (`mutatingMethods`)
- Modify: `internal/apiserver/forward.go` (`newReplyFor` ~77-104)
- Test: `internal/apiserver/sandboxservice_test.go`

**Interfaces:**
- Consumes: `Manager.BumpActivity` (Task 1), existing `IdRequest`/`Sandbox` protos, `Forwarder.newReplyFor`.
- Produces: `func (s *SandboxService) KeepAlive(ctx context.Context, r *sbxv1.IdRequest) (*sbxv1.Sandbox, error)`; gRPC method `/sbxswarm.v1.SandboxService/KeepAlive`; REST `POST /v1/sandboxes/{id}/keepalive`.

- [ ] **Step 1: Add the proto method + regenerate**

In `proto/sbxswarm/v1/sandbox.proto`, inside `service SandboxService`, after the `PublishSandbox` rpc:

```proto
  rpc KeepAlive(IdRequest) returns (Sandbox) {
    option (google.api.http) = {post: "/v1/sandboxes/{id}/keepalive"};
  }
```

Run: `buf generate && go build ./...`
Expected: regenerates `internal/gen/sbxswarm/v1/sandbox*.pb.go` + gateway; build succeeds. (Editor may show false errors — trust the toolchain.)

- [ ] **Step 2: Write the failing test**

Add to `internal/apiserver/sandboxservice_test.go`:

```go
func TestKeepAlive_BumpsAndNotFound(t *testing.T) {
	svc := newSandboxSvc(t)
	ctx := context.Background()
	rec, err := svc.mgr.Create(ctx, sandbox.CreateSpec{})
	require.NoError(t, err)
	before := rec.LastActivity

	sb, err := svc.KeepAlive(ctx, &sbxv1.IdRequest{Id: rec.ID})
	require.NoError(t, err)
	require.Equal(t, rec.ID, sb.Id)

	got, err := svc.mgr.Get(ctx, rec.ID)
	require.NoError(t, err)
	require.True(t, got.LastActivity.After(before), "KeepAlive bumps LastActivity")

	_, err = svc.KeepAlive(ctx, &sbxv1.IdRequest{Id: "n1.missing"})
	require.Equal(t, codes.NotFound, status.Code(err))
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/apiserver/ -run TestKeepAlive_BumpsAndNotFound -v`
Expected: FAIL — `svc.KeepAlive` undefined.

- [ ] **Step 4: Implement the handler**

In `internal/apiserver/sandboxservice.go` (after `StopSandbox`):

```go
// KeepAlive records consumer Activity on a sandbox, resetting its idle clock.
func (s *SandboxService) KeepAlive(ctx context.Context, r *sbxv1.IdRequest) (*sbxv1.Sandbox, error) {
	if err := s.mgr.BumpActivity(ctx, r.Id); err == sandbox.ErrNotFound {
		return nil, status.Error(codes.NotFound, "sandbox not found")
	} else if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return s.GetSandbox(ctx, &sbxv1.GetSandboxRequest{Id: r.Id})
}
```

- [ ] **Step 5: Classify in authz + register in the forwarder**

In `internal/apiserver/authz.go`, add to `mutatingMethods`:

```go
	"/sbxswarm.v1.SandboxService/KeepAlive":      true,
```

In `internal/apiserver/forward.go` `newReplyFor`, add a case (next to `StopSandbox`):

```go
	case "/sbxswarm.v1.SandboxService/KeepAlive":
		return new(sbxv1.Sandbox)
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/apiserver/ -run 'TestKeepAlive_BumpsAndNotFound|TestAuthz_AllMethodsClassified' -v`
Expected: PASS (the classification drift-guard stays green).

- [ ] **Step 7: Full build/test + commit (including regenerated files)**

Run: `go build ./... && go test ./internal/apiserver/`
Expected: PASS.

```bash
git add proto/sbxswarm/v1/sandbox.proto internal/gen/sbxswarm/v1 internal/apiserver/sandboxservice.go internal/apiserver/authz.go internal/apiserver/forward.go internal/apiserver/sandboxservice_test.go
git commit -m "feat(apiserver): KeepAlive RPC (consumer keep-alive ping; mutating, owner-routed)"
```

---

### Task 8: node.go wiring — reaper ticker + CPU-as-activity bump

**Files:**
- Modify: `internal/node/node.go` (sandboxes wiring ~99-105; 10s ticker ~113-135; after the tickers ~136; add `reapInterval` near `runTicker` ~424)
- Test: `internal/node/node_test.go`

**Interfaces:**
- Consumes: `SandboxService.SetIdleTimeout`/`ReapIdle` (Tasks 5-6), `Config.IdleTimeoutDuration` (Task 4), `Manager.BumpActivity` (Task 1), `StatsCollector.Latest` (existing).
- Produces: `func reapInterval(timeout time.Duration) time.Duration` (= `min(timeout, time.Minute)`); a reaper ticker started only when `idle_timeout > 0`; CPU-as-activity bump folded into the existing 10s ticker.

- [ ] **Step 1: Write the failing test**

Add to `internal/node/node_test.go`:

```go
func TestReapInterval(t *testing.T) {
	require.Equal(t, 30*time.Second, reapInterval(30*time.Second))
	require.Equal(t, time.Minute, reapInterval(10*time.Minute))
	require.Equal(t, time.Minute, reapInterval(time.Minute))
}

func TestNode_BootWithIdleTimeout(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.IdleTimeout = "50ms" // reaper enabled; fast sweep
	require.NoError(t, cfg.Validate())

	n, err := New(cfg, obs.NewLogger("error", io.Discard), "test")
	require.NoError(t, err)
	require.NoError(t, n.Start())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, n.Stop(ctx))
}
```

Ensure `"time"` is imported in the test file.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/node/ -run 'TestReapInterval|TestNode_BootWithIdleTimeout' -v`
Expected: FAIL — `reapInterval` undefined.

- [ ] **Step 3: Add `reapInterval` and the package const**

In `internal/node/node.go`, near `runTicker` (~424), add:

```go
// reapInterval is the reaper sweep cadence: min(timeout, 1m) — minute-scale for
// real timeouts, responsive for small ones.
// ponytail: a fixed 1m feels broken for a 10s test timeout; a separate
// sweep-interval knob is YAGNI.
func reapInterval(timeout time.Duration) time.Duration {
	if timeout < time.Minute {
		return timeout
	}
	return time.Minute
}

// cpuActiveThreshold: per-sandbox CPU% at/above which observed work counts as
// Activity (ADR-0016). ponytail: 5% counts as "doing work"; tune if barely-busy
// sandboxes get reaped. Dynamic only on the real backend (fake reports a fixed 10%).
const cpuActiveThreshold = 5.0
```

- [ ] **Step 4: Wire `SetIdleTimeout`, the CPU bump, and the reaper ticker**

In `internal/node/node.go`, after `sandboxes.SetEvents(bus)` (~104), add:

```go
	sandboxes.SetIdleTimeout(cfg.IdleTimeoutDuration())
```

Before the 10s ticker (`go runTicker(nctx, 10*time.Second, ...)`, ~113), capture:

```go
	idleEnabled := cfg.IdleTimeoutDuration() > 0
```

Inside that ticker's record loop (the `for _, r := range recs` block computing `counts`), add the CPU-as-activity bump:

```go
			for _, r := range recs {
				counts[r.Status]++
				if idleEnabled && r.Status == "running" {
					if u, ok := statsC.Latest(r.BackendName); ok && u.CPUPercent >= cpuActiveThreshold {
						_ = mgr.BumpActivity(nctx, r.ID) // observed work counts as Activity
					}
				}
			}
```

After the netlog ticker (`go runTicker(nctx, 15*time.Second, ...)`, ~136), add the reaper ticker:

```go
	if idle := cfg.IdleTimeoutDuration(); idle > 0 {
		go runTicker(nctx, reapInterval(idle), func() { sandboxes.ReapIdle(nctx, time.Now()) })
	}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/node/ -run 'TestReapInterval|TestNode_BootWithIdleTimeout' -v`
Expected: PASS. (The boot test proves wiring; it does **not** assert idle-stop — the fake's fixed 10% CPU keeps sandboxes alive via the bridge, so end-to-end idle-stop is covered by Task 6's `ReapIdle` test.)

- [ ] **Step 6: Whole-repo verification + commit**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS (the only pre-existing red test is the env-gated `TestSDKBackend_CreateExecRemove` in `internal/sandbox`, which needs a real sbx daemon — unrelated to M7).

```bash
git add internal/node/node.go internal/node/node_test.go
git commit -m "feat(node): wire idle-stop reaper ticker + CPU-as-activity bump"
```

---

## Final verification

- [ ] `go build ./... && go vet ./... && go test ./...` (whole repo green except the pre-existing env-gated SDK test)
- [ ] `go test -tags integration ./...` (integration suites still green)
- [ ] `git log --oneline` shows the 8 task commits on `m7-reaper-idle-stop` (after the `docs(m7)` design commit)

## Self-Review notes (coverage against the spec)

- **§1 Activity** → Tasks 1 (Create/Start/BumpActivity), 5 (Exec/AgentRun + throttle), 7 (KeepAlive), 8 (CPU bridge). ✓
- **§2 IdleRunning** → Task 3 (strict `>`, running-only, `idle-stop:off`). ✓
- **§3 ReapIdle** (publish-then-stop, stop-regardless, count) → Task 6. ✓
- **§4 ticker + §1.3 CPU bridge** → Task 8 (`reapInterval`, dedicated ticker, bridge in the 10s loop). ✓
- **§5 config + labels** → Task 4 (`idle_timeout`), Task 2 (labels persist). ✓
- **§5b KeepAlive** (proto/handler/authz/forward) → Task 7. ✓
- **§6 testing** → boundary (T3), ReapIdle (T6), BumpActivity/Start re-reap (T1/T3), labels (T2), KeepAlive (T7), config (T4); CPU bridge inspection-only per spec (T8). ✓
- **§7 deliberately skipped** (disk enforcement, push-veto callback, sandbox.reaped event, separate sweep interval, capacity reclaim) → no tasks, by design. ✓
