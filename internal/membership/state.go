package membership

import "encoding/json"

// NodeState is a node's full advertised state. It is split: small routing
// fields ride NodeMeta (UDP); the rest rides TCP push/pull (ADR-0005).
type NodeState struct {
	// meta (tiny, UDP)
	NodeID          string `json:"id"`
	Addr            string `json:"a"`  // gRPC/REST address for routing
	Cordoned        bool   `json:"c"`
	StateVersion    uint64 `json:"v"`
	ProtocolVersion uint32 `json:"p"`
	// bulk (TCP push/pull)
	SwarmID         string            `json:"swarm_id,omitempty"`
	PubKey          []byte            `json:"pubkey,omitempty"`
	Capabilities    []string          `json:"caps,omitempty"`
	OwnedSandboxIDs []string          `json:"owned,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	LimitCPU        float64           `json:"limit_cpu,omitempty"`
	LimitMemKB      float64           `json:"limit_mem_kb,omitempty"`
	AllocCPU        float64           `json:"alloc_cpu,omitempty"`
	AllocMemKB      float64           `json:"alloc_mem_kb,omitempty"`
}

type metaWire struct {
	NodeID, Addr    string
	Cordoned        bool
	StateVersion    uint64
	ProtocolVersion uint32
}

// EncodeMeta serializes only the tiny routing fields for NodeMeta.
func (n NodeState) EncodeMeta() []byte {
	b, _ := json.Marshal(metaWire{n.NodeID, n.Addr, n.Cordoned, n.StateVersion, n.ProtocolVersion})
	return b
}

// DecodeMeta parses NodeMeta into a partial NodeState (routing fields only).
func DecodeMeta(b []byte) (NodeState, error) {
	var m metaWire
	if err := json.Unmarshal(b, &m); err != nil {
		return NodeState{}, err
	}
	return NodeState{NodeID: m.NodeID, Addr: m.Addr, Cordoned: m.Cordoned, StateVersion: m.StateVersion, ProtocolVersion: m.ProtocolVersion}, nil
}

// EncodeBulk/DecodeBulk serialize the full state for TCP push/pull.
func (n NodeState) EncodeBulk() []byte { b, _ := json.Marshal(n); return b }
func DecodeBulk(b []byte) (NodeState, error) {
	var n NodeState
	err := json.Unmarshal(b, &n)
	return n, err
}
