package nodekey

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/identity"
	"github.com/stretchr/testify/require"
)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return pub, priv, identity.DeriveNodeID(pub)
}

func TestSignVerify_RoundTrip(t *testing.T) {
	pub, priv, callerID := newKey(t)
	now := time.Unix(1_700_000_000, 0)
	tok := Sign(priv, callerID, "target-node", now)

	resolve := func(id string) ([]byte, bool) {
		if id == callerID {
			return pub, true
		}
		return nil, false
	}
	got, err := Verify(tok, "target-node", resolve, now.Add(5*time.Second), 30*time.Second, nil)
	require.NoError(t, err)
	require.Equal(t, callerID, got)
}

func TestVerify_WrongAudienceRejected(t *testing.T) {
	pub, priv, callerID := newKey(t)
	now := time.Unix(1_700_000_000, 0)
	tok := Sign(priv, callerID, "node-B", now)
	resolve := func(string) ([]byte, bool) { return pub, true }
	_, err := Verify(tok, "node-C", resolve, now, 30*time.Second, nil)
	require.ErrorContains(t, err, "audience")
}

func TestVerify_StaleRejected(t *testing.T) {
	pub, priv, callerID := newKey(t)
	now := time.Unix(1_700_000_000, 0)
	tok := Sign(priv, callerID, "t", now)
	resolve := func(string) ([]byte, bool) { return pub, true }
	_, err := Verify(tok, "t", resolve, now.Add(40*time.Second), 30*time.Second, nil)
	require.ErrorContains(t, err, "stale")
}

func TestVerify_UnknownPeerRejected(t *testing.T) {
	_, priv, callerID := newKey(t)
	now := time.Unix(1_700_000_000, 0)
	tok := Sign(priv, callerID, "t", now)
	resolve := func(string) ([]byte, bool) { return nil, false }
	_, err := Verify(tok, "t", resolve, now, 30*time.Second, nil)
	require.ErrorContains(t, err, "unknown")
}

func TestVerify_ForgedSignatureRejected(t *testing.T) {
	pub, priv, callerID := newKey(t)
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Unix(1_700_000_000, 0)
	tok := Sign(priv, callerID, "t", now)
	resolve := func(string) ([]byte, bool) { return otherPub, true } // wrong pubkey
	_, err := Verify(tok, "t", resolve, now, 30*time.Second, nil)
	require.Error(t, err)
	_ = pub
}

func TestVerify_DenylistedRejected(t *testing.T) {
	pub, priv, callerID := newKey(t)
	now := time.Unix(1_700_000_000, 0)
	tok := Sign(priv, callerID, "t", now)
	resolve := func(string) ([]byte, bool) { return pub, true }
	denied := func(id string) bool { return id == callerID }
	_, err := Verify(tok, "t", resolve, now, 30*time.Second, denied)
	require.ErrorContains(t, err, "denied")
}
