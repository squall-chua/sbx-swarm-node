//go:build integration

package membership_test

import (
	"context"
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

// startSchedNode builds+starts a node, allowing per-node config customization
// (workspaces, provision limits). Mirrors startNode but with a customize hook.
func startSchedNode(t *testing.T, listenAddr, gossipAddr string, seeds []string, customize func(*config.Config)) *node.Node {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ListenAddr = listenAddr
	cfg.GossipAddr = gossipAddr
	cfg.ClusterSecret = "integration-test-secret"
	cfg.Join = seeds
	cfg.APIKeys = []config.APIKey{{Key: "adm", Role: "admin"}, {Key: "ro", Role: "read-only"}}
	if customize != nil {
		customize(cfg)
	}
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

// listSandboxIDs returns the ids of sandboxes owned locally by n.
func listSandboxIDs(t *testing.T, client *http.Client, n *node.Node) []string {
	t.Helper()
	resp := authedGet(t, client, fmt.Sprintf("https://%s/v1/sandboxes", n.Addr()), "adm")
	defer resp.Body.Close()
	var body struct {
		Sandboxes []struct {
			ID string `json:"id"`
		} `json:"sandboxes"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	ids := make([]string, 0, len(body.Sandboxes))
	for _, s := range body.Sandboxes {
		ids = append(ids, s.ID)
	}
	return ids
}

// postCreate POSTs a create request body to n's REST API and returns the status code.
func postCreate(t *testing.T, client *http.Client, n *node.Node, jsonBody string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("https://%s/v1/sandboxes", n.Addr()), strings.NewReader(jsonBody))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer adm")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	return resp.StatusCode
}

// TestScheduling_WorkspaceTargetedLandsOnOwner: a create on entry node A that
// requires a workspace only B advertises must be forwarded and created on B
// (B's prefix), never locally on A.
func TestScheduling_WorkspaceTargetedLandsOnOwner(t *testing.T) {
	nodeA := startSchedNode(t, "127.0.0.1:19843", "127.0.0.1:17996", nil, func(c *config.Config) {
		c.ProvisionLimits.CPUCores = 4 // A has NO workspaces
	})
	nodeB := startSchedNode(t, "127.0.0.1:19844", "127.0.0.1:17997", []string{"127.0.0.1:17996"}, func(c *config.Config) {
		c.ProvisionLimits.CPUCores = 4
		c.Workspaces = []config.WorkspaceConfig{{Name: "repo-only-b", HostPath: t.TempDir()}}
	})

	waitForPeer(t, nodeA, nodeB.NodeID(), 10*time.Second)
	// Wait until A actually sees B advertise the workspace (bulk gossip).
	require.Eventually(t, func() bool {
		for _, p := range nodeA.Cluster().PeerStates() {
			if p.NodeID == nodeB.NodeID() {
				for _, w := range p.Workspaces {
					if w == "repo-only-b" {
						return true
					}
				}
			}
		}
		return false
	}, 10*time.Second, 200*time.Millisecond, "A never saw B advertise workspace repo-only-b")

	client := tlsClient()
	require.Equal(t, http.StatusOK, postCreate(t, client, nodeA, `{"workspaces":[{"name":"repo-only-b"}]}`))

	// The sandbox must appear on B (forwarded Provision), with B's self-routing prefix.
	require.Eventually(t, func() bool {
		ids := listSandboxIDs(t, client, nodeB)
		return len(ids) == 1 && strings.HasPrefix(ids[0], nodeB.NodeID()+".")
	}, 10*time.Second, 200*time.Millisecond, "sandbox never landed on B")
	// And it must NOT have been created locally on A.
	require.Empty(t, listSandboxIDs(t, client, nodeA), "A must not create the workspace-targeted sandbox locally")
}

// TestScheduling_OverCapacityCreatesNothing: a create exceeding BOTH nodes'
// limits must be admitted nowhere — no sandbox on either node.
func TestScheduling_OverCapacityCreatesNothing(t *testing.T) {
	nodeA := startSchedNode(t, "127.0.0.1:19845", "127.0.0.1:17998", nil, func(c *config.Config) {
		c.ProvisionLimits.CPUCores = 2
	})
	nodeB := startSchedNode(t, "127.0.0.1:19846", "127.0.0.1:17999", []string{"127.0.0.1:17998"}, func(c *config.Config) {
		c.ProvisionLimits.CPUCores = 2
	})

	waitForPeer(t, nodeA, nodeB.NodeID(), 10*time.Second)

	client := tlsClient()
	require.Equal(t, http.StatusOK, postCreate(t, client, nodeA, `{"cpus":100}`)) // op accepted; async placement finds no node

	// Give the async op time to run and fail; assert nothing was created anywhere.
	time.Sleep(2 * time.Second)
	require.Empty(t, listSandboxIDs(t, client, nodeA), "A must not create an over-capacity sandbox")
	require.Empty(t, listSandboxIDs(t, client, nodeB), "B must not create an over-capacity sandbox")
}
