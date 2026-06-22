package sandbox

import (
	"context"
	"path/filepath"
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
