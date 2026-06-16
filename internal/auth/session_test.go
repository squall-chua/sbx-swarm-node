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
