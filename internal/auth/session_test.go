package auth

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSession_RoundTrip(t *testing.T) {
	s := NewSigner([]byte("secret-key-32-bytes-long-aaaaaaa"))
	tok := s.Mint("admin", time.Now().Add(time.Hour))

	role, err := s.Verify(tok)
	require.NoError(t, err)
	require.Equal(t, "admin", role)
}

func TestSession_RejectsExpiredAndTampered(t *testing.T) {
	s := NewSigner([]byte("secret-key-32-bytes-long-aaaaaaa"))

	expired := s.Mint("admin", time.Now().Add(-time.Minute))
	_, err := s.Verify(expired)
	require.Error(t, err)

	tok := s.Mint("read-only", time.Now().Add(time.Hour))
	_, err = s.Verify(tok + "x") // tamper
	require.Error(t, err)
}

func TestDeriveSessionKey_SwarmWideVsStandalone(t *testing.T) {
	// Same cluster secret on two nodes -> identical key -> cross-node verify works.
	k1 := DeriveSessionKey("cluster-secret-xyz", []byte("node-A-seed"))
	k2 := DeriveSessionKey("cluster-secret-xyz", []byte("node-B-seed"))
	require.Equal(t, k1, k2)
	require.Len(t, k1, 32)

	tokA := NewSigner(k1).Mint("admin", time.Now().Add(time.Hour))
	role, err := NewSigner(k2).Verify(tokA) // node B verifies node A's token
	require.NoError(t, err)
	require.Equal(t, "admin", role)

	// Standalone (no cluster secret) falls back to the per-node seed.
	s1 := DeriveSessionKey("", []byte("node-A-seed"))
	s2 := DeriveSessionKey("", []byte("node-B-seed"))
	require.NotEqual(t, s1, s2)
}
