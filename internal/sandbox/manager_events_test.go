package sandbox

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/events"
	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
)

func TestManager_EmitsLifecycleEvents(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	bus := events.NewBus("node1", 32)
	m := NewManager("node1", NewFake(), st, ids.NewGen("node1"))
	m.SetPublisher(bus)

	ctx := context.Background()
	rec, err := m.Create(ctx, CreateSpec{})
	require.NoError(t, err)
	require.NoError(t, m.Stop(ctx, rec.ID))

	got := bus.Replay(events.Filter{}, 0)
	types := []string{}
	for _, e := range got {
		types = append(types, e.Type)
	}
	require.Contains(t, types, "sandbox.created")
	require.Contains(t, types, "sandbox.stopped")
}
