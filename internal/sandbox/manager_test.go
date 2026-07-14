package sandbox

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
)

func newMgr(t *testing.T) (*Manager, *Fake) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	f := NewFake()
	return NewManager("node1", f, st, ids.NewGen("node1")), f
}

func TestManager_CreateGetListDelete(t *testing.T) {
	m, _ := newMgr(t)
	ctx := context.Background()

	rec, err := m.Create(ctx, CreateSpec{CPUs: 1, MemoryBytes: 1 << 30})
	require.NoError(t, err)
	require.Contains(t, rec.ID, "node1.")
	require.Equal(t, "running", rec.Status)

	got, err := m.Get(ctx, rec.ID)
	require.NoError(t, err)
	require.Equal(t, rec.ID, got.ID)

	all, err := m.List(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)

	require.NoError(t, m.Delete(ctx, rec.ID))
	_, err = m.Get(ctx, rec.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestManager_DeleteGCsCustomSecrets(t *testing.T) {
	m, f := newMgr(t)
	ctx := context.Background()

	rec, err := m.Create(ctx, CreateSpec{CPUs: 1, MemoryBytes: 1 << 30})
	require.NoError(t, err)

	// A proxy-injected custom secret is scoped to the sandbox's BackendName.
	require.NoError(t, f.SecretSet(ctx, rec.BackendName, CustomSecret{Host: "api.minimax.io", Env: "MINIMAX_API_KEY", Value: "sk-secret"}))
	secs, err := f.SecretList(ctx, rec.BackendName)
	require.NoError(t, err)
	require.Len(t, secs.Custom, 1)

	require.NoError(t, m.Delete(ctx, rec.ID))

	// The secret must not orphan once the sandbox is gone.
	secs, err = f.SecretList(ctx, rec.BackendName)
	require.NoError(t, err)
	require.Empty(t, secs.Custom)
}

// A node that restarts with a fresh identity/store (new node id) leaves the sbx
// daemon holding the old container with no matching record. Terminate for that
// id must still reap the live container by name, not orphan it on a store miss.
func TestManager_DeleteReapsRecordlessOrphan(t *testing.T) {
	m, f := newMgr(t)
	ctx := context.Background()

	rec, err := m.Create(ctx, CreateSpec{})
	require.NoError(t, err)

	// Simulate the restart: drop the store record, keep the backend container.
	require.NoError(t, m.store.Delete(bucket, rec.ID))
	_, live := f.sandboxes[rec.BackendName]
	require.True(t, live, "backend container should still be alive")

	// Terminate must reach through to the daemon and remove it.
	require.NoError(t, m.Delete(ctx, rec.ID))
	_, live = f.sandboxes[rec.BackendName]
	require.False(t, live, "orphaned container must be removed")
}

// fakeNotifier records the owned-id sets pushed by the Manager.
type fakeNotifier struct{ sets [][]string }

func (f *fakeNotifier) UpdateLocalSandboxIDs(ids []string) {
	cp := append([]string(nil), ids...)
	f.sets = append(f.sets, cp)
}

func TestManager_NotifiesOwnedIDsOnCreateDelete(t *testing.T) {
	m, _ := newMgr(t)
	n := &fakeNotifier{}
	m.SetOwnedIDsNotifier(n)
	ctx := context.Background()

	rec, err := m.Create(ctx, CreateSpec{})
	require.NoError(t, err)
	require.Len(t, n.sets, 1, "create should notify once")
	require.Equal(t, []string{rec.ID}, n.sets[0])

	require.NoError(t, m.Delete(ctx, rec.ID))
	require.Len(t, n.sets, 2, "delete should notify once")
	require.Empty(t, n.sets[1], "owned set is empty after delete")
}

func TestManager_OwnedIDsNotifier_NilSafe(t *testing.T) {
	m, _ := newMgr(t)
	ctx := context.Background()
	// No notifier wired — must not panic.
	rec, err := m.Create(ctx, CreateSpec{})
	require.NoError(t, err)
	require.NoError(t, m.Delete(ctx, rec.ID))
}

func TestManager_ReconcileDropsVanishedRecords(t *testing.T) {
	m, f := newMgr(t)
	ctx := context.Background()
	rec, err := m.Create(ctx, CreateSpec{})
	require.NoError(t, err)

	// Backend loses the sandbox out-of-band.
	require.NoError(t, f.Remove(ctx, rec.BackendName))

	require.NoError(t, m.Reconcile(ctx))
	got, err := m.Get(ctx, rec.ID)
	require.NoError(t, err)
	require.Equal(t, "lost", got.Status)
}

func TestManager_AdmitAndCreate_NacksOverLimit(t *testing.T) {
	mgr, _ := newMgr(t)
	mgr.SetCapacity(NewCapacity(2, 1e9, 1e9)) // 2 cores

	_, err := mgr.AdmitAndCreate(context.Background(), CreateSpec{CPUs: 2, MemoryBytes: 1})
	require.NoError(t, err)

	_, err = mgr.AdmitAndCreate(context.Background(), CreateSpec{CPUs: 1, MemoryBytes: 1})
	require.ErrorIs(t, err, ErrNoCapacity)
}

func TestManager_ReconcileSetsBaseFromRecords(t *testing.T) {
	mgr, _ := newMgr(t)
	capt := NewCapacity(0, 0, 0)
	mgr.SetCapacity(capt)

	_, err := mgr.AdmitAndCreate(context.Background(), CreateSpec{CPUs: 3, MemoryBytes: 2048, DiskGB: 4})
	require.NoError(t, err)
	require.NoError(t, mgr.Reconcile(context.Background()))

	cpu, mem, disk := capt.Snapshot()
	require.Equal(t, 3.0, cpu)
	require.Equal(t, 2.0, mem) // 2048 bytes -> 2 KB
	require.Equal(t, 4.0, disk)
}

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

func TestManager_ConcurrentBumpDoesNotResurrectStopped(t *testing.T) {
	m, _ := newMgr(t)
	ctx := context.Background()
	for range 200 {
		rec, err := m.Create(ctx, CreateSpec{})
		require.NoError(t, err)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = m.BumpActivity(ctx, rec.ID) }()
		go func() { defer wg.Done(); _ = m.Stop(ctx, rec.ID) }()
		wg.Wait()
		got, err := m.Get(ctx, rec.ID)
		require.NoError(t, err)
		require.Equal(t, "stopped", got.Status,
			"a concurrent BumpActivity must not revert a Stop to running")
	}
}

func TestManager_ConcurrentBumpDoesNotResurrectDeleted(t *testing.T) {
	m, _ := newMgr(t)
	ctx := context.Background()
	for range 200 {
		rec, err := m.Create(ctx, CreateSpec{})
		require.NoError(t, err)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = m.BumpActivity(ctx, rec.ID) }()
		go func() { defer wg.Done(); _ = m.Delete(ctx, rec.ID) }()
		wg.Wait()
		_, err = m.Get(ctx, rec.ID)
		require.ErrorIs(t, err, ErrNotFound,
			"a concurrent BumpActivity must not re-create a deleted record")
	}
}
