package membership

import (
	"errors"
	"sort"
	"time"
)

// revokedBucket persists the grow-only denylist of revoked node ids (ADR-0013).
const revokedBucket = "revoked"

// Revoke adds nodeID to this node's denylist, persists it, and re-advertises so
// the revocation propagates over gossip. Grow-only and permanent (ADR-0013): a
// revoked node returns only by generating a new key. Rejects the empty id and
// self-revocation (which would brick this node's own node-auth to peers).
func (c *Cluster) Revoke(nodeID string) error {
	if nodeID == "" {
		return errors.New("revoke: empty node id")
	}
	c.mu.Lock()
	if nodeID == c.local.NodeID {
		c.mu.Unlock()
		return errors.New("revoke: cannot revoke self")
	}
	grew := c.addRevokedLocked(nodeID)
	ml := c.ml
	c.mu.Unlock()
	if grew && ml != nil {
		_ = ml.UpdateNode(5 * time.Second)
	}
	return nil
}

// IsRevoked reports whether nodeID is on the denylist. Wired as the nodekey
// `denied` predicate so a revoked node's node-auth is rejected swarm-wide.
func (c *Cluster) IsRevoked(nodeID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.revoked[nodeID]
	return ok
}

// RevokedList returns the sorted denylist snapshot.
func (c *Cluster) RevokedList() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return sortedKeys(c.revoked)
}

// addRevokedLocked folds ids into the union, persisting new ones and refreshing
// the advertised NodeState. Returns whether the union grew. Caller MUST hold
// c.mu (write); if it returns true, call ml.UpdateNode after unlocking.
func (c *Cluster) addRevokedLocked(ids ...string) bool {
	grew := false
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := c.revoked[id]; ok {
			continue
		}
		c.revoked[id] = struct{}{}
		if c.st != nil {
			_ = c.st.Put(revokedBucket, id, []byte{1})
		}
		grew = true
	}
	if grew {
		c.local.Revoked = sortedKeys(c.revoked)
		c.local.StateVersion++
	}
	return grew
}

// loadRevoked seeds the in-memory union from the store at construction so a
// restarted node keeps (and re-advertises) what it has revoked or learned.
func (c *Cluster) loadRevoked() {
	if c.st == nil {
		return
	}
	_ = c.st.ForEach(revokedBucket, func(k, _ []byte) error {
		c.revoked[string(k)] = struct{}{}
		return nil
	})
	if len(c.revoked) > 0 {
		c.local.Revoked = sortedKeys(c.revoked)
	}
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
