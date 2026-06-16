package audit

import (
	"fmt"
	"path/filepath"
	"sync"
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

func TestAudit_ConcurrentRecordNoLostEntries(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	a := New(st, func() int64 { return 1 })

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			require.NoError(t, a.Record(Entry{Actor: "admin", Action: "secret.set", Target: fmt.Sprintf("host-%d", i), Outcome: "ok"}))
		}(i)
	}
	wg.Wait()

	all, err := a.List()
	require.NoError(t, err)
	require.Len(t, all, n, "no audit entries should be lost under concurrency")

	seen := map[int64]bool{}
	for _, e := range all {
		require.False(t, seen[e.Seq], "duplicate Seq %d", e.Seq)
		seen[e.Seq] = true
	}
	require.Len(t, seen, n, "all Seqs must be distinct")
}
