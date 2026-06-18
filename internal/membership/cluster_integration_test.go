//go:build integration

package membership_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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

// createSandboxOnB POSTs a sandbox on the owner node, then polls its sandbox
// list for the REAL sandbox id (CreateSandbox returns an async Operation, not
// the sandbox itself). It returns the "<nodeID>.<ulid>" sandbox id.
func createSandboxOnB(t *testing.T, client *http.Client, owner *node.Node) string {
	t.Helper()
	createURL := fmt.Sprintf("https://%s/v1/sandboxes", owner.Addr())
	req, err := http.NewRequest(http.MethodPost, createURL, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer adm")
	req.Header.Set("Content-Type", "application/json")
	createResp, err := client.Do(req)
	require.NoError(t, err)
	defer createResp.Body.Close()
	require.Equal(t, http.StatusOK, createResp.StatusCode)

	// Poll the owner's own list for the created sandbox id.
	listURL := fmt.Sprintf("https://%s/v1/sandboxes", owner.Addr())
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp := authedGet(t, client, listURL, "adm")
		var body struct {
			Sandboxes []struct {
				ID        string `json:"id"`
				OwnerNode string `json:"owner_node"`
			} `json:"sandboxes"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if len(body.Sandboxes) > 0 && body.Sandboxes[0].ID != "" {
			require.Equal(t, owner.NodeID(), body.Sandboxes[0].OwnerNode)
			return body.Sandboxes[0].ID
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("timeout: sandbox never appeared in owner's list")
	return ""
}

// TestCluster_ForwardSandboxRequest creates a sandbox on node B and verifies it
// is reachable via node A's REST API: A must reverse-proxy GET
// /v1/sandboxes/{id} to B (the owner) and return 200 with B's sandbox data.
func TestCluster_ForwardSandboxRequest(t *testing.T) {
	nodeA := startNode(t, "127.0.0.1:19543", "127.0.0.1:17956", nil)
	nodeB := startNode(t, "127.0.0.1:19544", "127.0.0.1:17957", []string{"127.0.0.1:17956"})

	// Wait for gossip to converge so A knows B's API address.
	waitForPeer(t, nodeA, nodeB.NodeID(), 10*time.Second)

	client := tlsClient()
	sbxID := createSandboxOnB(t, client, nodeB)
	require.Contains(t, sbxID, nodeB.NodeID()+".", "sandbox id must be self-routing under B")

	// Fetch the sandbox from the NON-OWNER (A). A must forward to B and return
	// 200 with B's data — NOT a local 404.
	getURL := fmt.Sprintf("https://%s/v1/sandboxes/%s", nodeA.Addr(), sbxID)
	resp := authedGet(t, client, getURL, "adm")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "non-owner A must forward GET to owner B")

	var got struct {
		ID        string `json:"id"`
		OwnerNode string `json:"owner_node"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, sbxID, got.ID)
	require.Equal(t, nodeB.NodeID(), got.OwnerNode, "forwarded response must carry B's ownership")
}

// TestCluster_ForwardLogsSSE verifies that a logs SSE stream for a B-owned
// sandbox, requested on non-owner A, is relayed from B (C2).
func TestCluster_ForwardLogsSSE(t *testing.T) {
	nodeA := startNode(t, "127.0.0.1:19553", "127.0.0.1:17976", nil)
	nodeB := startNode(t, "127.0.0.1:19554", "127.0.0.1:17977", []string{"127.0.0.1:17976"})

	waitForPeer(t, nodeA, nodeB.NodeID(), 10*time.Second)

	client := tlsClient()
	sbxID := createSandboxOnB(t, client, nodeB)

	// Request logs SSE on A (non-owner) — must be reverse-proxied to B.
	logsURL := fmt.Sprintf("https://%s/v1/sandboxes/%s/logs", nodeA.Addr(), sbxID)
	req, err := http.NewRequest(http.MethodGet, logsURL, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer adm")
	req.Header.Set("Accept", "text/event-stream")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := client.Do(req.WithContext(ctx))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")

	// Read until we see a relayed log data frame from B's fake backend.
	buf := make([]byte, 256)
	deadline := time.Now().Add(4 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			got += string(buf[:n])
			if strings.Contains(got, "data:") {
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	require.Contains(t, got, "data:", "expected an SSE log frame relayed from B, got %q", got)
}

// TestCluster_PublishForwardsToOwner verifies that a PublishSandbox request
// sent to the NON-OWNER node is forwarded to the owner and returns an Operation.
// The op's background doPublish will fail because the fake backend has no
// git-backed workspace — that is EXPECTED. This test validates cross-node
// forwarding + op creation, not git transport.
func TestCluster_PublishForwardsToOwner(t *testing.T) {
	nodeA := startNode(t, "127.0.0.1:19545", "127.0.0.1:17958", nil)
	nodeB := startNode(t, "127.0.0.1:19546", "127.0.0.1:17959", []string{"127.0.0.1:17958"})

	waitForPeer(t, nodeA, nodeB.NodeID(), 10*time.Second)

	client := tlsClient()
	// Create the sandbox on B (B is the owner).
	sbxID := createSandboxOnB(t, client, nodeB)
	require.Contains(t, sbxID, nodeB.NodeID()+".", "sandbox id must be self-routing under B")

	// POST publish to NON-OWNER A — A must forward to owner B and return an Operation.
	publishURL := fmt.Sprintf("https://%s/v1/sandboxes/%s/git/publish", nodeA.Addr(), sbxID)
	req, err := http.NewRequest(http.MethodPost, publishURL, strings.NewReader("{}"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer adm")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var op struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&op))
	require.NotEmpty(t, op.ID)
	require.Equal(t, "git-publish", op.Type)
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
