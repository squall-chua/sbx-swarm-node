// Package routing resolves which node owns a sandbox (by its self-routing id
// prefix, ADR-0002) and tracks node addresses + cordon state.
package routing

import (
	"strings"
	"sync"
)

type entry struct {
	addr     string
	cordoned bool
	pubkey   []byte
}

// Table is the in-memory node directory (rebuilt from gossip).
type Table struct {
	self string
	mu   sync.RWMutex
	m    map[string]entry
}

// NewTable returns a table for the local node id.
func NewTable(self string) *Table { return &Table{self: self, m: map[string]entry{}} }

// Upsert records a node's address, cordon flag, and (if non-empty) gossiped
// pubkey. An empty pubkey preserves any previously-pinned key, so meta-tier
// (UDP) updates do not clobber the bulk-tier (TCP) pubkey.
func (t *Table) Upsert(nodeID, addr string, cordoned bool, pubkey []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.m[nodeID]
	e.addr = addr
	e.cordoned = cordoned
	if len(pubkey) > 0 {
		e.pubkey = pubkey
	}
	t.m[nodeID] = e
}

// PubKey returns a node's gossiped pubkey, if known.
func (t *Table) PubKey(nodeID string) ([]byte, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, ok := t.m[nodeID]
	if !ok || len(e.pubkey) == 0 {
		return nil, false
	}
	return e.pubkey, true
}

// Remove drops a node (left/dead).
func (t *Table) Remove(nodeID string) { t.mu.Lock(); delete(t.m, nodeID); t.mu.Unlock() }

// Owner returns the node id that owns a sandbox/op id (its prefix).
func (t *Table) Owner(id string) (string, bool) {
	i := strings.IndexByte(id, '.')
	if i <= 0 {
		return "", false
	}
	return id[:i], true
}

// IsLocal reports whether the id is owned by this node.
func (t *Table) IsLocal(id string) bool {
	owner, ok := t.Owner(id)
	return ok && owner == t.self
}

// Addr returns a node's address.
func (t *Table) Addr(nodeID string) (string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, ok := t.m[nodeID]
	return e.addr, ok
}

// IsCordoned reports a node's cordon state.
func (t *Table) IsCordoned(nodeID string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.m[nodeID].cordoned
}

// Peers returns all known node ids except self.
func (t *Table) Peers() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []string
	for id := range t.m {
		if id != t.self {
			out = append(out, id)
		}
	}
	return out
}
