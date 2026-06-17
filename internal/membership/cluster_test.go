package membership

import (
	"io"
	"log/slog"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/routing"
	"github.com/stretchr/testify/require"
)

// newTestDelegate builds a delegate over a minimal Cluster (no live memberlist),
// sufficient to exercise MergeRemoteState's gating/merge logic directly.
func newTestDelegate(self string, si *SwarmIdentity) (*delegate, *Cluster, *routing.Table) {
	tbl := routing.NewTable(self)
	c := &Cluster{
		local:      NodeState{NodeID: self, ProtocolVersion: ProtocolVersion},
		peerStates: map[string]NodeState{},
		tbl:        tbl,
		si:         si,
		revoked:    map[string]struct{}{},
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return &delegate{c: c}, c, tbl
}

func TestMergeRemoteState_SkipsIncompatibleProtocol(t *testing.T) {
	d, c, tbl := newTestDelegate("nA", &SwarmIdentity{SwarmID: "swarm-A", Mode: ModeRejoin})

	remote := NodeState{
		NodeID:          "nB",
		Addr:            "10.0.0.2:8443",
		ProtocolVersion: ProtocolVersion + 1, // incompatible
		SwarmID:         "swarm-A",
	}
	d.MergeRemoteState(remote.EncodeBulk(), false)

	// Skipped: not tracked, not routed.
	require.Empty(t, c.PeerStates())
	_, ok := tbl.Addr("nB")
	require.False(t, ok, "incompatible-protocol peer must not be added to routing")
}

func TestMergeRemoteState_MergesCompatiblePeer(t *testing.T) {
	d, c, tbl := newTestDelegate("nA", &SwarmIdentity{SwarmID: "swarm-A", Mode: ModeRejoin})

	remote := NodeState{
		NodeID:          "nB",
		Addr:            "10.0.0.2:8443",
		ProtocolVersion: ProtocolVersion,
		SwarmID:         "swarm-A",
	}
	d.MergeRemoteState(remote.EncodeBulk(), false)

	require.Len(t, c.PeerStates(), 1)
	addr, ok := tbl.Addr("nB")
	require.True(t, ok)
	require.Equal(t, "10.0.0.2:8443", addr)
}

func TestMergeRemoteState_PendingAdoptsAndExitsPending(t *testing.T) {
	si := &SwarmIdentity{Mode: ModePendingJoin} // empty SwarmID, pending
	d, c, _ := newTestDelegate("nA", si)
	// Adopt writes swarm.json via persist; point it at a temp path.
	c.siPath = t.TempDir() + "/swarm.json"

	remote := NodeState{
		NodeID:          "nB",
		Addr:            "10.0.0.2:8443",
		ProtocolVersion: ProtocolVersion,
		SwarmID:         "swarm-A",
		SwarmName:       "prod",
	}
	d.MergeRemoteState(remote.EncodeBulk(), true)

	require.Equal(t, "swarm-A", c.si.SwarmID)
	require.Equal(t, "prod", c.si.SwarmName, "Adopt should record the peer's swarm NAME, not its node id")
	require.Equal(t, ModeRejoin, c.si.Mode, "after adopting, the node must leave pending-join")
}

func TestMergeRemoteState_SkipsTrueSwarmMismatch(t *testing.T) {
	d, c, tbl := newTestDelegate("nA", &SwarmIdentity{SwarmID: "swarm-A", Mode: ModeRejoin})

	remote := NodeState{
		NodeID:          "nB",
		Addr:            "10.0.0.2:8443",
		ProtocolVersion: ProtocolVersion,
		SwarmID:         "swarm-B", // true mismatch
	}
	d.MergeRemoteState(remote.EncodeBulk(), false)

	require.Empty(t, c.PeerStates())
	_, ok := tbl.Addr("nB")
	require.False(t, ok)
}

func TestUpdateLocalLoad_SetsUtilAndBumpsVersionOnce(t *testing.T) {
	_, c, _ := newTestDelegate("n1", nil)
	before := c.LocalNodeState().StateVersion
	c.UpdateLocalLoad(2, 1024, 5, 0.4, 0.6)
	ns := c.LocalNodeState()
	require.Equal(t, 2.0, ns.AllocCPU)
	require.Equal(t, 1024.0, ns.AllocMemKB)
	require.Equal(t, 5.0, ns.AllocDiskGB)
	require.Equal(t, 0.4, ns.ActualCPU)
	require.Equal(t, 0.6, ns.ActualMem)
	require.Equal(t, before+1, ns.StateVersion, "one combined re-advertise")
}
