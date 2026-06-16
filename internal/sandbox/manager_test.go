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
