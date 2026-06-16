package sandbox

import (
	"context"
	"path/filepath"
	"testing"

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
