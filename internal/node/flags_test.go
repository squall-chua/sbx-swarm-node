package node

import (
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/squall-chua/sbx-swarm-node/internal/obs"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "node.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestNodeFlags_AbsentReadsFalse(t *testing.T) {
	log := obs.NewLogger("error", io.Discard)
	require.Equal(t, nodeFlags{}, loadNodeFlags(openTestStore(t), log))
}

func TestNodeFlags_RoundTrip(t *testing.T) {
	log := obs.NewLogger("error", io.Discard)
	st := openTestStore(t)
	saveNodeFlags(st, log, nodeFlags{Cordoned: true, Draining: true})
	require.Equal(t, nodeFlags{Cordoned: true, Draining: true}, loadNodeFlags(st, log))

	// Uncordon writes both back to false, not just the cordon.
	saveNodeFlags(st, log, nodeFlags{})
	require.Equal(t, nodeFlags{}, loadNodeFlags(st, log))
}

func TestNodeFlags_CorruptJSONReadsFalse(t *testing.T) {
	log := obs.NewLogger("error", io.Discard)
	st := openTestStore(t)
	// Write invalid JSON directly to the same bucket and key that saveNodeFlags uses.
	err := st.Put(flagsBucket, flagsKey, []byte("not valid json {"))
	require.NoError(t, err)
	// loadNodeFlags should fall back to all-false when JSON unmarshalling fails.
	require.Equal(t, nodeFlags{}, loadNodeFlags(st, log))
}
