package events

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBus_PublishAssignsMonotonicIDsAndBuffers(t *testing.T) {
	b := NewBus("node1", 8)

	e1 := b.Publish("sandbox.created", "sb1", map[string]string{"k": "v"})
	e2 := b.Publish("sandbox.stopped", "sb1", nil)
	require.Equal(t, uint64(1), e1.Seq)
	require.Equal(t, uint64(2), e2.Seq)
	require.Equal(t, "node1-1", e1.ID)

	// replay everything after seq 0
	all := b.Replay(Filter{}, 0)
	require.Len(t, all, 2)

	// filter by type
	created := b.Replay(Filter{Types: []string{"sandbox.created"}}, 0)
	require.Len(t, created, 1)
	require.Equal(t, "sandbox.created", created[0].Type)

	// filter by sandbox + since seq
	since := b.Replay(Filter{SandboxID: "sb1"}, 1)
	require.Len(t, since, 1)
	require.Equal(t, uint64(2), since[0].Seq)
}

func TestBus_SubscribeReceivesLiveEvents(t *testing.T) {
	b := NewBus("node1", 8)
	ch, cancel := b.Subscribe(Filter{Types: []string{"sandbox.created"}}, 0)
	defer cancel()

	b.Publish("sandbox.stopped", "sb1", nil) // filtered out
	b.Publish("sandbox.created", "sb2", nil) // delivered

	got := <-ch
	require.Equal(t, "sandbox.created", got.Type)
	require.Equal(t, "sb2", got.SandboxID)
}
