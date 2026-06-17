//go:build integration

package membership_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRevocation_PropagatesOverGossip: node A revokes B's id; the revocation
// must reach B's own denylist via gossip (B folds A's gossiped Revoked set into
// its union). Proven by polling B's /v1/node/revoked until B's id appears.
func TestRevocation_PropagatesOverGossip(t *testing.T) {
	nodeA := startNode(t, "127.0.0.1:19847", "127.0.0.1:17990", nil)
	nodeB := startNode(t, "127.0.0.1:19848", "127.0.0.1:17991", []string{"127.0.0.1:17990"})
	waitForPeer(t, nodeA, nodeB.NodeID(), 10*time.Second)
	waitForPeer(t, nodeB, nodeA.NodeID(), 10*time.Second)

	client := tlsClient()

	// A revokes B (admin bearer).
	body := fmt.Sprintf(`{"node_id":%q}`, nodeB.NodeID())
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("https://%s/v1/node/revoke", nodeA.Addr()), strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer adm")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Poll B's denylist until B's own id propagates in.
	revokedURL := fmt.Sprintf("https://%s/v1/node/revoked", nodeB.Addr())
	require.Eventually(t, func() bool {
		greq, _ := http.NewRequest(http.MethodGet, revokedURL, nil)
		greq.Header.Set("Authorization", "Bearer adm")
		gresp, gerr := client.Do(greq)
		if gerr != nil {
			return false
		}
		defer gresp.Body.Close()
		var out struct {
			NodeIds []string `json:"node_ids"`
		}
		if json.NewDecoder(gresp.Body).Decode(&out) != nil {
			return false
		}
		for _, id := range out.NodeIds {
			if id == nodeB.NodeID() {
				return true
			}
		}
		return false
	}, 30*time.Second, 500*time.Millisecond, "B must learn its own revocation via gossip")
}
