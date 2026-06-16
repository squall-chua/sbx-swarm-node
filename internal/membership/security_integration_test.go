//go:build integration

package membership_test

import (
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// postStatus issues a bodyless POST with a bearer key and returns the status.
func postStatus(t *testing.T, client *http.Client, url, key string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	return resp.StatusCode
}

// TestSecurity_ForwardedMutationRoleGate: a mutating call (StopSandbox) for a
// B-owned sandbox, issued at non-owner A, is gated at the owner — read-only is
// rejected (403, relayed by A), admin is allowed (200).
func TestSecurity_ForwardedMutationRoleGate(t *testing.T) {
	nodeA := startNode(t, "127.0.0.1:19743", "127.0.0.1:17986", nil)
	nodeB := startNode(t, "127.0.0.1:19744", "127.0.0.1:17987", []string{"127.0.0.1:17986"})
	waitForPeer(t, nodeA, nodeB.NodeID(), 10*time.Second)

	client := tlsClient()
	sbxID := createSandboxOnB(t, client, nodeB)

	stopURL := fmt.Sprintf("https://%s/v1/sandboxes/%s/stop", nodeA.Addr(), sbxID)
	require.Equal(t, http.StatusForbidden, postStatus(t, client, stopURL, "ro"),
		"read-only must be rejected on a forwarded mutation")
	require.Equal(t, http.StatusOK, postStatus(t, client, stopURL, "adm"),
		"admin must be allowed on a forwarded mutation")
}

// TestSecurity_CrossNodeBrowserSession: a session cookie minted on A authorizes a
// forwarded read on B (swarm-wide session key, ADR-0010).
func TestSecurity_CrossNodeBrowserSession(t *testing.T) {
	nodeA := startNode(t, "127.0.0.1:19745", "127.0.0.1:17988", nil)
	nodeB := startNode(t, "127.0.0.1:19746", "127.0.0.1:17989", []string{"127.0.0.1:17988"})
	waitForPeer(t, nodeA, nodeB.NodeID(), 10*time.Second)

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	client := tlsClient()
	client.Jar = jar

	// Mint a session on A by exchanging the admin bearer.
	sessURL := fmt.Sprintf("https://%s/v1/auth/session", nodeA.Addr())
	req, _ := http.NewRequest(http.MethodPost, sessURL, nil)
	req.Header.Set("Authorization", "Bearer adm")
	resp, err := client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	// Create a B-owned sandbox (bearer), then fetch it via A using ONLY the cookie.
	sbxID := createSandboxOnB(t, tlsClient(), nodeB)
	getURL := fmt.Sprintf("https://%s/v1/sandboxes/%s", nodeA.Addr(), sbxID)
	getReq, _ := http.NewRequest(http.MethodGet, getURL, nil) // no Authorization; cookie jar carries the session
	getResp, err := client.Do(getReq)
	require.NoError(t, err)
	defer getResp.Body.Close()
	require.Equal(t, http.StatusOK, getResp.StatusCode,
		"a session minted on A must authorize a forwarded read on B (swarm-wide session key)")
}
