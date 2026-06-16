package tlsutil

import (
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
