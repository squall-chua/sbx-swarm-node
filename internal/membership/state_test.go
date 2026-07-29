package membership

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNodeState_BulkRoundTripSchedulingFields(t *testing.T) {
	in := NodeState{
		NodeID: "n1", ProtocolVersion: ProtocolVersion,
		Workspaces:  []string{"repo-foo"},
		Templates:   []string{"base:1"},
		LimitDiskGB: 100, AllocDiskGB: 12,
	}
	out, err := DecodeBulk(in.EncodeBulk())
	require.NoError(t, err)
	require.Equal(t, []string{"repo-foo"}, out.Workspaces)
	require.Equal(t, []string{"base:1"}, out.Templates)
	require.Equal(t, 100.0, out.LimitDiskGB)
	require.Equal(t, 12.0, out.AllocDiskGB)
}

func TestNodeState_MetaTinyAndBulkRoundTrip(t *testing.T) {
	ns := NodeState{
		NodeID: "n1", Addr: "10.0.0.1:8443", Cordoned: true, StateVersion: 7, ProtocolVersion: 1,
		PubKey: []byte("pk"), Capabilities: []string{"clone", "stats"},
		OwnedSandboxIDs: []string{"n1.aaa", "n1.bbb"}, SwarmID: "swarm-A",
	}

	meta := ns.EncodeMeta()
	require.LessOrEqual(t, len(meta), 512) // NodeMeta budget (ADR-0005)
	gotMeta, err := DecodeMeta(meta)
	require.NoError(t, err)
	require.Equal(t, "n1", gotMeta.NodeID)
	require.Equal(t, uint64(7), gotMeta.StateVersion)

	bulk := ns.EncodeBulk()
	gotBulk, err := DecodeBulk(bulk)
	require.NoError(t, err)
	require.Equal(t, []string{"n1.aaa", "n1.bbb"}, gotBulk.OwnedSandboxIDs)
	require.Equal(t, []string{"clone", "stats"}, gotBulk.Capabilities)
}

// TestBulk_CarriesNodeName proves a node's display name survives a bulk
// round-trip, so a peer's name shows on the console instead of rendering blank.
func TestBulk_CarriesNodeName(t *testing.T) {
	in := NodeState{NodeID: "n1", NodeName: "worker-1"}
	out, err := DecodeBulk(in.EncodeBulk())
	require.NoError(t, err)
	require.Equal(t, "worker-1", out.NodeName)
}

// TestBulk_MissingNodeNameStillDecodes proves an older peer's bulk state
// (encoded before NodeName existed) still decodes -- the field is additive and
// omitempty, so a payload without it just yields a zero-value name, not an error.
func TestBulk_MissingNodeNameStillDecodes(t *testing.T) {
	old := []byte(`{"id":"n1"}`) // no "node_name" key, simulating a pre-upgrade peer
	out, err := DecodeBulk(old)
	require.NoError(t, err)
	require.Equal(t, "n1", out.NodeID)
	require.Empty(t, out.NodeName)
}

func TestBulk_CarriesKits(t *testing.T) {
	in := NodeState{NodeID: "n1", Kits: []string{"extras", "tools"}}
	out, err := DecodeBulk(in.EncodeBulk())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, want := out.Kits, []string{"extras", "tools"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}
