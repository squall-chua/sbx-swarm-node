package membership

import (
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/squall-chua/sbx-swarm-node/internal/config"
	"github.com/squall-chua/sbx-swarm-node/internal/routing"
)

// Cluster wraps hashicorp/memberlist with swarm-aware delegate logic.
// It handles:
//   - NodeMeta (tiny UDP): routing fields via EncodeMeta/DecodeMeta.
//   - Push/Pull (TCP): full state via EncodeBulk/DecodeBulk.
//   - Delta broadcasts via TransmitLimitedQueue (wired but empty in v1;
//     state propagates through push/pull).
//   - Encrypted gossip via sha256(ClusterSecret) as AES-256 key (ADR-0004).
type Cluster struct {
	mu         sync.RWMutex
	local      NodeState
	peerStates map[string]NodeState // nodeID → latest received bulk state
	ml         *memberlist.Memberlist
	bcast      *memberlist.TransmitLimitedQueue
	tbl        *routing.Table
	si         *SwarmIdentity
	siPath     string // path to persist swarm.json on Adopt
	onNodeDead func(nodeID string)
	log        *slog.Logger
	shutdown   bool // guards Leave/Shutdown idempotency (memberlist.Leave panics after Shutdown)
}

// NewCluster constructs a Cluster. It does NOT join; call Join separately.
func NewCluster(
	cfg *config.Config,
	local NodeState,
	tbl *routing.Table,
	si *SwarmIdentity,
	siPath string,
	onNodeDead func(string),
	log *slog.Logger,
) (*Cluster, error) {
	c := &Cluster{
		local:      local,
		peerStates: map[string]NodeState{},
		tbl:        tbl,
		si:         si,
		siPath:     siPath,
		onNodeDead: onNodeDead,
		log:        log,
	}

	mlCfg := memberlist.DefaultLANConfig()
	mlCfg.Name = local.NodeID
	mlCfg.Logger = nil // suppress memberlist's default logger to avoid interleave

	// Parse GossipAddr into bind host/port.
	host, portStr, err := net.SplitHostPort(cfg.GossipAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid gossip_addr %q: %w", cfg.GossipAddr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("invalid gossip_addr port %q: %w", portStr, err)
	}
	mlCfg.BindAddr = host
	if mlCfg.BindAddr == "" {
		mlCfg.BindAddr = "0.0.0.0"
	}
	mlCfg.BindPort = port
	mlCfg.AdvertisePort = port

	// Encrypted gossip: sha256(secret) → 32-byte AES-256 key (ADR-0004).
	if cfg.ClusterSecret != "" {
		key := sha256.Sum256([]byte(cfg.ClusterSecret))
		keyring, err := memberlist.NewKeyring(nil, key[:])
		if err != nil {
			return nil, fmt.Errorf("memberlist keyring: %w", err)
		}
		mlCfg.Keyring = keyring
	}

	mlCfg.Delegate = &delegate{c: c}
	mlCfg.Events = &eventDelegate{c: c}

	ml, err := memberlist.Create(mlCfg)
	if err != nil {
		return nil, fmt.Errorf("memberlist create: %w", err)
	}
	c.ml = ml
	c.bcast = &memberlist.TransmitLimitedQueue{
		NumNodes:       func() int { return ml.NumMembers() },
		RetransmitMult: mlCfg.RetransmitMult,
	}

	return c, nil
}

// Join contacts the given seed addresses and merges into the cluster.
func (c *Cluster) Join(seeds []string) (int, error) {
	return c.ml.Join(seeds)
}

// Leave gracefully notifies peers before shutting down. It is idempotent: once
// the cluster has been shut down, Leave is a no-op (memberlist.Leave panics if
// called after Shutdown).
func (c *Cluster) Leave(timeout time.Duration) error {
	c.mu.Lock()
	if c.shutdown {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()
	return c.ml.Leave(timeout)
}

// Shutdown terminates the memberlist transport. It is idempotent.
func (c *Cluster) Shutdown() error {
	c.mu.Lock()
	if c.shutdown {
		c.mu.Unlock()
		return nil
	}
	c.shutdown = true
	c.mu.Unlock()
	return c.ml.Shutdown()
}

// LocalNodeState returns the current local NodeState snapshot.
func (c *Cluster) LocalNodeState() NodeState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.local
}

// SetCordoned updates the local node's cordon flag and re-advertises via
// memberlist.UpdateNode so peers receive the change in the next gossip round.
func (c *Cluster) SetCordoned(cordoned bool) {
	c.mu.Lock()
	c.local.Cordoned = cordoned
	c.local.StateVersion++
	ml := c.ml
	c.mu.Unlock()
	c.tbl.Upsert(c.local.NodeID, c.local.Addr, cordoned, c.local.PubKey)
	if ml != nil {
		_ = ml.UpdateNode(5 * time.Second)
	}
}

// PeerStates returns a snapshot of all known peer bulk states.
func (c *Cluster) PeerStates() []NodeState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]NodeState, 0, len(c.peerStates))
	for _, ns := range c.peerStates {
		out = append(out, ns)
	}
	return out
}

// localState returns the current local NodeState (caller must NOT hold mu).
func (c *Cluster) localState() NodeState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.local
}

// UpdateLocalSandboxIDs replaces OwnedSandboxIDs, bumps StateVersion, and
// re-advertises so peers pick up the change. The owned-id set rides bulk
// push/pull; UpdateNode promptly propagates the bumped StateVersion in meta and
// triggers a gossip round. Safe to call before memberlist is created (no-op
// re-advertise when ml is nil).
func (c *Cluster) UpdateLocalSandboxIDs(ids []string) {
	c.mu.Lock()
	c.local.OwnedSandboxIDs = ids
	c.local.StateVersion++
	ml := c.ml
	c.mu.Unlock()
	if ml != nil {
		_ = ml.UpdateNode(5 * time.Second)
	}
}

// UpdateLocalAlloc refreshes the gossiped allocation snapshot and re-advertises.
func (c *Cluster) UpdateLocalAlloc(cpu, memKB, diskGB float64) {
	c.mu.Lock()
	c.local.AllocCPU = cpu
	c.local.AllocMemKB = memKB
	c.local.AllocDiskGB = diskGB
	c.local.StateVersion++
	ml := c.ml
	c.mu.Unlock()
	if ml != nil {
		_ = ml.UpdateNode(5 * time.Second)
	}
}

// --- delegate (push/pull + broadcasts) ---

type delegate struct{ c *Cluster }

// NodeMeta returns tiny routing fields for UDP gossip (ADR-0005 meta tier).
func (d *delegate) NodeMeta(limit int) []byte {
	b := d.c.localState().EncodeMeta()
	if len(b) > limit {
		// Meta too large: clear OwnedSandboxIDs (they ride bulk push/pull anyway).
		d.c.log.Warn("NodeMeta exceeds limit; trimming owned sandbox ids",
			"size", len(b), "limit", limit)
		s := d.c.localState()
		s.OwnedSandboxIDs = nil
		b = s.EncodeMeta()
	}
	return b
}

// NotifyMsg handles incoming delta broadcast messages. In v1 broadcasts are
// unused (state rides push/pull), so we discard inbound messages silently.
func (d *delegate) NotifyMsg([]byte) {}

// GetBroadcasts returns pending delta messages. In v1 we send none.
func (d *delegate) GetBroadcasts(overhead, limit int) [][]byte {
	return d.c.bcast.GetBroadcasts(overhead, limit)
}

// LocalState returns the full bulk state for TCP push/pull.
func (d *delegate) LocalState(join bool) []byte {
	return d.c.localState().EncodeBulk()
}

// MergeRemoteState merges a peer's bulk state received via push/pull.
func (d *delegate) MergeRemoteState(buf []byte, join bool) {
	remote, err := DecodeBulk(buf)
	if err != nil {
		d.c.log.Warn("MergeRemoteState: failed to decode bulk", "err", err)
		return
	}

	// Protocol-version gating (ADR-0009): for v1 require an exact match. Skip
	// (do not Upsert / track) peers on an incompatible protocol version.
	if remote.ProtocolVersion != ProtocolVersion {
		d.c.log.Warn("MergeRemoteState: incompatible protocol version; skipping peer",
			"local_proto", ProtocolVersion, "remote_proto", remote.ProtocolVersion, "peer", remote.NodeID)
		return
	}

	localID := func() string {
		d.c.mu.RLock()
		defer d.c.mu.RUnlock()
		return d.c.si.SwarmID
	}()

	// A true mismatch is two distinct non-empty swarm ids under the same secret
	// (ADR-0001). An empty remote id is a pending-join peer that will adopt our
	// id, so GuardJoin accepts it and we proceed with the merge. On a true
	// mismatch we skip merging this peer only — we do NOT tear down the local
	// node from inside this delegate callback (a single rogue peer must never
	// evict an otherwise-healthy node).
	if err := GuardJoin(localID, remote.SwarmID); err != nil {
		d.c.log.Warn("MergeRemoteState: swarm id mismatch; skipping peer",
			"local_swarm", localID, "remote_swarm", remote.SwarmID, "peer", remote.NodeID)
		return
	}

	// Update peer map.
	d.c.mu.Lock()
	d.c.peerStates[remote.NodeID] = remote
	isPending := d.c.si.Mode == ModePendingJoin
	d.c.mu.Unlock()

	// Update routing table.
	d.c.tbl.Upsert(remote.NodeID, remote.Addr, remote.Cordoned, remote.PubKey)

	// Adopt swarm id if we are still pending-join and remote has one.
	if isPending && remote.SwarmID != "" {
		if err := d.c.si.Adopt(d.c.siPath, remote.SwarmID, remote.SwarmName); err != nil {
			d.c.log.Warn("Adopt failed", "err", err)
		} else {
			d.c.mu.Lock()
			d.c.local.SwarmID = remote.SwarmID
			// Transition out of pending-join so we stop re-Adopting (re-writing
			// swarm.json) on every subsequent push/pull round.
			d.c.si.Mode = ModeRejoin
			d.c.mu.Unlock()
			d.c.log.Info("adopted swarm id from peer",
				"swarm_id", remote.SwarmID, "peer", remote.NodeID)
		}
	}
}

// --- eventDelegate (join/leave/update notifications) ---

type eventDelegate struct{ c *Cluster }

func (e *eventDelegate) NotifyJoin(node *memberlist.Node) {
	ns, err := DecodeMeta(node.Meta)
	if err != nil || ns.NodeID == "" || ns.Addr == "" {
		// Meta unavailable/empty: do NOT fall back to node.Address() — that is the
		// gossip port, not the API/routing addr. MergeRemoteState (bulk push/pull)
		// will supply the correct API addr shortly.
		e.c.log.Info("memberlist: node joined (awaiting bulk state for addr)", "name", node.Name)
		return
	}
	e.c.tbl.Upsert(ns.NodeID, ns.Addr, ns.Cordoned, ns.PubKey)
	e.c.log.Info("memberlist: node joined", "node_id", ns.NodeID, "addr", ns.Addr)
}

func (e *eventDelegate) NotifyUpdate(node *memberlist.Node) {
	ns, err := DecodeMeta(node.Meta)
	if err != nil || ns.NodeID == "" || ns.Addr == "" {
		e.c.log.Info("memberlist: node updated (awaiting bulk state for addr)", "name", node.Name)
		return
	}
	e.c.tbl.Upsert(ns.NodeID, ns.Addr, ns.Cordoned, ns.PubKey)
	e.c.log.Info("memberlist: node updated", "node_id", ns.NodeID, "cordoned", ns.Cordoned)
}

func (e *eventDelegate) NotifyLeave(node *memberlist.Node) {
	// node.Name is the memberlist name, which we set to NodeID.
	nodeID := node.Name
	e.c.tbl.Remove(nodeID)
	e.c.mu.Lock()
	delete(e.c.peerStates, nodeID)
	e.c.mu.Unlock()
	e.c.log.Info("memberlist: node left/dead", "node_id", nodeID)
	if e.c.onNodeDead != nil {
		e.c.onNodeDead(nodeID)
	}
}
