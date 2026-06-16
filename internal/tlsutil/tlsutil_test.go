package tlsutil

import (
	"crypto/ed25519"
	crand "crypto/rand"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadOrGenerate_GeneratesUsableCert(t *testing.T) {
	dir := t.TempDir()
	cert, err := LoadOrGenerate("", "", dir)
	require.NoError(t, err)
	require.NotNil(t, cert.PrivateKey)
	require.NotEmpty(t, cert.Certificate)

	// Regenerated load from the now-persisted files returns a cert too.
	again, err := LoadOrGenerate(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), dir)
	require.NoError(t, err)
	require.NotEmpty(t, again.Certificate)
}

func TestGenerateForKey_LeafPubkeyMatchesNodeKey(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(crand.Reader)
	require.NoError(t, err)
	cert, err := GenerateForKey(priv)
	require.NoError(t, err)

	leafPub, err := LeafPublicKey(cert)
	require.NoError(t, err)
	require.True(t, ed25519.PublicKey(leafPub).Equal(pub))

	// PinnedVerify accepts the matching pubkey, rejects a different one.
	verify := PinnedVerify(pub)
	require.NoError(t, verify(cert.Certificate, nil))

	otherPub, _, _ := ed25519.GenerateKey(crand.Reader)
	require.Error(t, PinnedVerify(otherPub)(cert.Certificate, nil))
}
