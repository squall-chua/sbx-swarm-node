package audit

import (
	"path/filepath"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
)

func TestAudit_AppendAndList(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	a := New(st, func() int64 { return 1 })
	require.NoError(t, a.Record(Entry{Actor: "admin", Action: "policy.deny", Target: "evil.example", Outcome: "ok"}))

	all, err := a.List()
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Equal(t, "policy.deny", all[0].Action)
}
