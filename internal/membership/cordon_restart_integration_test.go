//go:build integration

package membership_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/config"
	"github.com/squall-chua/sbx-swarm-node/internal/node"
	"github.com/squall-chua/sbx-swarm-node/internal/obs"
	"github.com/stretchr/testify/require"
)

// startNodeWithDir is startNode but reuses a caller-given DataDir instead of a
// fresh t.TempDir(). Node identity (internal/identity) and the cordon/draining
// flags are both persisted under DataDir, so booting a fresh *node.Node on the
// same directory and address is a real restart, not a new node.
func startNodeWithDir(t *testing.T, dataDir, listenAddr, gossipAddr string, seeds []string) *node.Node {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = dataDir
	cfg.ListenAddr = listenAddr
	cfg.GossipAddr = gossipAddr
	cfg.ClusterSecret = "integration-test-secret"
	cfg.Join = seeds
	cfg.APIKeys = []config.APIKey{{Key: "adm", Role: "admin"}, {Key: "ro", Role: "read-only"}}

	log := obs.NewLogger("error", io.Discard)
	n, err := node.New(cfg, log, "test")
	require.NoError(t, err, "node.New for %s", listenAddr)
	require.NoError(t, n.Start(), "node.Start for %s", listenAddr)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = n.Stop(ctx)
	})
	return n
}

// forceSync triggers an immediate TCP push/pull between watcher and the peer
// at gossipAddr, instead of waiting on the periodic push/pull ticker
// (memberlist.DefaultLANConfig().PushPullInterval is 30s).
//
// Cordoned rides in NodeMeta too, and NotifyUpdate (fired promptly by
// UpdateNode) fires almost immediately — but NotifyUpdate only upserts the
// routing table. PeerStates(), what buildCandidates/ListNodes actually read,
// is fed exclusively by MergeRemoteState, i.e. a full push/pull round. Without
// a nudge, waitForCordoned is racing a 30s timer: it is genuinely
// nondeterministic (observed both a ~40s pass and a 45s-timeout failure across
// runs), so a longer fixed timeout only narrows the odds of flaking, it does
// not remove them.
//
// (*Cluster).Join always performs a pushPullNode against every given address,
// even one that is already a member (see memberlist.Memberlist.Join) — this is
// the exact mechanism production code already relies on for the same "bulk
// state must propagate promptly" problem: Cluster.Revoke's pushPullPeers
// (internal/membership/revocation.go) calls ml.Join per live peer for
// precisely this reason. Reusing the exported Join here (rather than adding a
// test-only export of the unexported pushPullPeers) makes the propagation
// deterministic: Join blocks until the exchange completes, so by the time it
// returns, watcher's peerStates already reflects the peer's current state.
func forceSync(t *testing.T, watcher *node.Node, gossipAddr string) {
	t.Helper()
	n, err := watcher.Cluster().Join([]string{gossipAddr})
	require.NoError(t, err, "forced push/pull with %s", gossipAddr)
	require.Greater(t, n, 0, "forced push/pull with %s contacted no host", gossipAddr)
}

// waitForCordoned polls watcher's gossiped peer view until it sees peerID's
// Cordoned flag match want, or fails the test on timeout. Call forceSync
// first — this is a short safety-net poll for the (already-applied) result,
// not the thing doing the waiting.
func waitForCordoned(t *testing.T, watcher *node.Node, peerID string, want bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, p := range watcher.Cluster().PeerStates() {
			if p.NodeID == peerID && p.Cordoned == want {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timeout: peer %s cordoned=%v not observed within %s", peerID, want, timeout)
}

// selfCordoned reads n's own row from its GET /v1/nodes and returns its cordoned
// flag — the same field a swarm console or operator tool would read.
func selfCordoned(t *testing.T, client *http.Client, n *node.Node) bool {
	t.Helper()
	resp := authedGet(t, client, fmt.Sprintf("https://%s/v1/nodes", n.Addr()), "adm")
	defer resp.Body.Close()
	var out struct {
		Nodes []struct {
			NodeID   string `json:"node_id"`
			Cordoned bool   `json:"cordoned"`
		} `json:"nodes"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	for _, nd := range out.Nodes {
		if nd.NodeID == n.NodeID() {
			return nd.Cordoned
		}
	}
	t.Fatalf("self node_id %s not found in %s's own /v1/nodes", n.NodeID(), n.NodeID())
	return false
}

// createSandboxSwarmWide POSTs a create request (no target node) to postTo and
// polls postTo's own /v1/operations for the op to finish, returning the
// resulting sandbox id — which names its OWNING node, wherever the scheduler
// actually placed it. Unlike createSandboxOnB (cluster_integration_test.go),
// this does not assume postTo is the owner, so it cannot poll postTo's sandbox
// list; the operation's sandbox_id is the only place that survives a
// cross-node placement.
func createSandboxSwarmWide(t *testing.T, client *http.Client, postTo *node.Node) string {
	t.Helper()
	createURL := fmt.Sprintf("https://%s/v1/sandboxes", postTo.Addr())
	req, err := http.NewRequest(http.MethodPost, createURL, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer adm")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var op struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&op))
	require.NotEmpty(t, op.ID)

	opsURL := fmt.Sprintf("https://%s/v1/operations", postTo.Addr())
	var sbxID string
	require.Eventually(t, func() bool {
		resp := authedGet(t, client, opsURL, "adm")
		defer resp.Body.Close()
		var out struct {
			Operations []struct {
				ID        string `json:"id"`
				State     string `json:"state"`
				Error     string `json:"error"`
				SandboxID string `json:"sandbox_id"`
			} `json:"operations"`
		}
		if json.NewDecoder(resp.Body).Decode(&out) != nil {
			return false
		}
		for _, o := range out.Operations {
			if o.ID != op.ID {
				continue
			}
			if o.State == "error" {
				t.Fatalf("provision op %s failed: %s", o.ID, o.Error)
			}
			if o.State == "done" {
				sbxID = o.SandboxID
				return true
			}
		}
		return false
	}, 10*time.Second, 100*time.Millisecond, "provision op never reached a terminal state")
	require.NotEmpty(t, sbxID, "a done provision op must carry a sandbox id")
	return sbxID
}

// deleteSandboxOn issues a best-effort admin DELETE for id against owner. Used
// as cleanup for a sandbox this test placed on a node other than the one it
// posted the create request to.
func deleteSandboxOn(client *http.Client, owner *node.Node, id string) {
	req, err := http.NewRequest(http.MethodDelete, fmt.Sprintf("https://%s/v1/sandboxes/%s", owner.Addr(), id), nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer adm")
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

// TestCluster_CordonSurvivesRestart drives the ADR-0023 fix on a real two-node
// cluster (not the single-process boot tests in internal/node/node_test.go):
// cordon node A, restart it in place (same DataDir, same listen/gossip
// addresses, rejoining the same seed) and confirm the cordon holds on all three
// axes that matter — A reports itself cordoned, peer B (re-)observes A as
// cordoned via gossip, and a swarm-wide placement actually avoids the
// restarted A.
//
// The third assertion is the one that proves the bug is fixed (a NodeInfo flag
// that says "cordoned" but does not gate placement would be worthless). With
// only two nodes, "avoids A" is verified by asserting the sandbox lands on B —
// the only other candidate once A is filtered — since with A cordoned there is
// nowhere else for the scheduler to legally place it.
func TestCluster_CordonSurvivesRestart(t *testing.T) {
	dirA := t.TempDir()

	const (
		listenA = "127.0.0.1:19743"
		gossipA = "127.0.0.1:17986"
		listenB = "127.0.0.1:19744"
		gossipB = "127.0.0.1:17987"
	)

	// B is the stable seed; A (whose restart we are about to simulate) joins it.
	nodeB := startNode(t, listenB, gossipB, nil)
	nodeA := startNodeWithDir(t, dirA, listenA, gossipA, []string{gossipB})

	waitForPeer(t, nodeB, nodeA.NodeID(), 10*time.Second)
	waitForPeer(t, nodeA, nodeB.NodeID(), 10*time.Second)
	aID := nodeA.NodeID()

	client := tlsClient()

	// Cordon A through its own REST API.
	cordonReq, err := http.NewRequest(http.MethodPost, "https://"+nodeA.Addr()+"/v1/node/cordon", nil)
	require.NoError(t, err)
	cordonReq.Header.Set("Authorization", "Bearer adm")
	cordonResp, err := client.Do(cordonReq)
	require.NoError(t, err)
	cordonResp.Body.Close()
	require.Equal(t, http.StatusOK, cordonResp.StatusCode)

	// B must see A as cordoned before we tear A down, or the restart proves
	// nothing (we would not know whether B's later view came from before or
	// after the restart). Force the sync rather than wait on the periodic timer.
	forceSync(t, nodeB, gossipA)
	waitForCordoned(t, nodeB, aID, true, 5*time.Second)

	// Stop A, then start a brand-new *node.Node on the SAME DataDir and the
	// SAME addresses — the thing under test is that this counts as a restart.
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	require.NoError(t, nodeA.Stop(stopCtx))
	cancel()

	nodeA2 := startNodeWithDir(t, dirA, listenA, gossipA, []string{gossipB})
	require.Equal(t, aID, nodeA2.NodeID(), "a restart on the same DataDir must keep the same node identity")

	// 1. A reports itself cordoned, to itself, straight after boot.
	require.True(t, selfCordoned(t, client, nodeA2), "restarted node must report itself cordoned")

	// 2. B (re-)sees A as cordoned once gossip reconverges after the rejoin.
	// A's own rejoin (cfg.Join above) already triggers a push/pull with B, but
	// force one from B's side too so this assertion does not depend on which
	// side's join happened to win the race.
	waitForPeer(t, nodeB, aID, 10*time.Second)
	forceSync(t, nodeB, gossipA)
	waitForCordoned(t, nodeB, aID, true, 5*time.Second)

	// 3. A swarm-wide placement must not land on the restarted, still-cordoned
	// A. Post the create request to A itself (a cordoned node still answers
	// the API; it just cannot be scheduled onto) and confirm the scheduler
	// picked B instead.
	sbxID := createSandboxSwarmWide(t, client, nodeA2)
	t.Cleanup(func() { deleteSandboxOn(client, nodeB, sbxID) })
	require.Contains(t, sbxID, nodeB.NodeID()+".",
		"placement must have skipped the cordoned, restarted A and landed on B")
}
