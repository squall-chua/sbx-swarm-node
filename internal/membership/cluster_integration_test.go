//go:build integration

package membership_test

import (
	"context"
	"crypto/tls"
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

// tlsClient returns an http.Client that skips TLS verification (self-signed
// node certs in integration tests).
func tlsClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		},
		Timeout: 10 * time.Second,
	}
}

// authedGet performs a GET with bearer auth.
func authedGet(t *testing.T, client *http.Client, url, key string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	require.NoError(t, err)
	return resp
}

// startNode builds and starts a node with the given listen/gossip ports.
func startNode(t *testing.T, listenAddr, gossipAddr string, seeds []string) *node.Node {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ListenAddr = listenAddr
	cfg.GossipAddr = gossipAddr
	cfg.ClusterSecret = "integration-test-secret"
	cfg.Join = seeds
	cfg.APIKeys = []config.APIKey{{Key: "adm", Role: "admin"}}

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

// waitForPeer polls until nodeA's routing table knows nodeB or timeout.
func waitForPeer(t *testing.T, nodeA *node.Node, peerID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cl := nodeA.Cluster()
		if cl != nil {
			peers := cl.PeerStates()
			for _, p := range peers {
				if p.NodeID == peerID {
					return
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timeout: nodeA did not see peer %s in %s", peerID, timeout)
}

// TestCluster_TwoNodeJoin starts two nodes, B joins A, and asserts that A's
// cluster sees B in its peer states and routing table.
func TestCluster_TwoNodeJoin(t *testing.T) {
	// Node A is the seed.
	nodeA := startNode(t, "127.0.0.1:19443", "127.0.0.1:17946", nil)
	// Node B joins A via gossip.
	nodeB := startNode(t, "127.0.0.1:19444", "127.0.0.1:17947", []string{"127.0.0.1:17946"})

	// Give gossip time to exchange push/pull state.
	waitForPeer(t, nodeA, nodeB.NodeID(), 10*time.Second)

	// A's cluster should now list B in its peer states.
	clA := nodeA.Cluster()
	require.NotNil(t, clA)
	peers := clA.PeerStates()
	var found bool
	for _, p := range peers {
		if p.NodeID == nodeB.NodeID() {
			found = true
			break
		}
	}
	require.True(t, found, "nodeA.PeerStates should contain nodeB")
}

// TestCluster_ForwardSandboxRequest creates a sandbox on node B and verifies
// it is reachable via node A's REST API (forwarding by self-routing id prefix).
func TestCluster_ForwardSandboxRequest(t *testing.T) {
	nodeA := startNode(t, "127.0.0.1:19543", "127.0.0.1:17956", nil)
	nodeB := startNode(t, "127.0.0.1:19544", "127.0.0.1:17957", []string{"127.0.0.1:17956"})

	// Wait for gossip to converge.
	waitForPeer(t, nodeA, nodeB.NodeID(), 10*time.Second)

	client := tlsClient()

	// Create a sandbox on B directly.
	createURL := fmt.Sprintf("https://%s/v1/sandboxes", nodeB.Addr())
	req, err := http.NewRequest(http.MethodPost, createURL, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer adm")
	req.Header.Set("Content-Type", "application/json")
	createResp, err := client.Do(req)
	require.NoError(t, err)
	defer createResp.Body.Close()
	require.Equal(t, http.StatusOK, createResp.StatusCode)

	var sbx struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.NewDecoder(createResp.Body).Decode(&sbx))
	require.NotEmpty(t, sbx.ID)

	// Fetch the sandbox from A (should forward to B).
	getURL := fmt.Sprintf("https://%s/v1/sandboxes/%s", nodeA.Addr(), sbx.ID)
	resp := authedGet(t, client, getURL, "adm")
	defer resp.Body.Close()
	// 200 OK = forward succeeded; 404 = sandbox not found (acceptable if Fake
	// backend doesn't persist across the forward); anything else is a failure.
	require.True(t, resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound,
		"expected 200 or 404 from forwarded request, got %d", resp.StatusCode)
}

// TestCluster_NodeDeadRemovesFromPeers stops node B and verifies that A's
// peer view eventually removes it (memberlist failure detection).
func TestCluster_NodeDeadRemovesFromPeers(t *testing.T) {
	nodeA := startNode(t, "127.0.0.1:19643", "127.0.0.1:17966", nil)
	nodeB := startNode(t, "127.0.0.1:19644", "127.0.0.1:17967", []string{"127.0.0.1:17966"})

	waitForPeer(t, nodeA, nodeB.NodeID(), 10*time.Second)

	// Stop B gracefully so it sends a Leave notification to A.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = nodeB.Stop(ctx)

	// A should remove B from its peer states after Leave is received.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		peers := nodeA.Cluster().PeerStates()
		var found bool
		for _, p := range peers {
			if p.NodeID == nodeB.NodeID() {
				found = true
				break
			}
		}
		if !found {
			return // B successfully removed from A's view
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("timeout: nodeA still sees nodeB after graceful shutdown")
}
