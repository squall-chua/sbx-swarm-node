package membership

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/routing"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
)

// newRevCluster builds a bare Cluster (no live memberlist; ml nil so UpdateNode
// no-ops) backed by a temp store — enough to exercise the revoked union.
func newRevCluster(t *testing.T, self string) *Cluster {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return &Cluster{
		local:      NodeState{NodeID: self, ProtocolVersion: ProtocolVersion},
		peerStates: map[string]NodeState{},
		tbl:        routing.NewTable(self),
		st:         st,
		revoked:    map[string]struct{}{},
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestRevoke_AddsAndAdvertises(t *testing.T) {
	c := newRevCluster(t, "nA")
	before := c.LocalNodeState().StateVersion
	require.NoError(t, c.Revoke("nB"))
	require.True(t, c.IsRevoked("nB"))
	require.Equal(t, []string{"nB"}, c.RevokedList())
	require.Equal(t, []string{"nB"}, c.LocalNodeState().Revoked, "revoked set is advertised")
	require.Equal(t, before+1, c.LocalNodeState().StateVersion)
}

func TestRevoke_RejectsSelfAndEmpty(t *testing.T) {
	c := newRevCluster(t, "nA")
	require.Error(t, c.Revoke("nA"))
	require.Error(t, c.Revoke(""))
	require.Empty(t, c.RevokedList())
}

func TestRevoke_IdempotentNoSecondBump(t *testing.T) {
	c := newRevCluster(t, "nA")
	require.NoError(t, c.Revoke("nB"))
	v := c.LocalNodeState().StateVersion
	require.NoError(t, c.Revoke("nB"))
	require.Equal(t, v, c.LocalNodeState().StateVersion, "re-revoking must not bump version")
	require.Len(t, c.RevokedList(), 1)
}

func TestRevoke_PersistsAndReloads(t *testing.T) {
	c := newRevCluster(t, "nA")
	require.NoError(t, c.Revoke("nB"))
	require.NoError(t, c.Revoke("nC"))

	// Simulate restart: a fresh Cluster over the SAME store re-seeds the union.
	c2 := &Cluster{
		local:      NodeState{NodeID: "nA", ProtocolVersion: ProtocolVersion},
		peerStates: map[string]NodeState{},
		tbl:        routing.NewTable("nA"),
		st:         c.st,
		revoked:    map[string]struct{}{},
		log:        c.log,
	}
	c2.loadRevoked()
	require.True(t, c2.IsRevoked("nB"))
	require.True(t, c2.IsRevoked("nC"))
	require.Equal(t, []string{"nB", "nC"}, c2.LocalNodeState().Revoked)
}

func TestMergeRemoteState_FoldsRemoteRevoked(t *testing.T) {
	c := newRevCluster(t, "nA")
	c.si = &SwarmIdentity{SwarmID: "swarm-A", Mode: ModeRejoin}
	d := &delegate{c: c}

	remote := NodeState{
		NodeID:          "nB",
		Addr:            "10.0.0.2:8443",
		ProtocolVersion: ProtocolVersion,
		SwarmID:         "swarm-A",
		Revoked:         []string{"nX"}, // B has revoked nX
	}
	d.MergeRemoteState(remote.EncodeBulk(), false)

	require.True(t, c.IsRevoked("nX"), "a peer's revocation is folded into our union")
	raw, ok, _ := c.st.Get(revokedBucket, "nX")
	require.True(t, ok)
	require.Equal(t, []byte{1}, raw, "folded revocation is persisted")
}
