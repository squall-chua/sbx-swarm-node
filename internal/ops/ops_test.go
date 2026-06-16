package ops

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
)

func newMgr(t *testing.T) *Manager {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return NewManager(st, ids.NewGen("n1"))
}

func TestOps_RunSetsDone(t *testing.T) {
	m := newMgr(t)
	op, existed, err := m.Start(context.Background(), "provision", "")
	require.NoError(t, err)
	require.False(t, existed)

	m.Run(op.ID, func() (string, error) { return "sb1", nil })
	require.Eventually(t, func() bool {
		got, _ := m.Get(op.ID)
		return got != nil && got.State == "done" && got.SandboxID == "sb1"
	}, time.Second, 10*time.Millisecond)
}

func TestOps_IdempotencyReturnsSameOp(t *testing.T) {
	m := newMgr(t)
	a, existedA, err := m.Start(context.Background(), "provision", "key-1")
	require.NoError(t, err)
	require.False(t, existedA)

	b, existedB, err := m.Start(context.Background(), "provision", "key-1")
	require.NoError(t, err)
	require.True(t, existedB)
	require.Equal(t, a.ID, b.ID) // same op for same idempotency key
}
