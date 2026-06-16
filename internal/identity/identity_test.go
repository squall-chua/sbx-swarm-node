package identity

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeriveNodeID_DeterministicAndFormatted(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize) // all-zero seed → fixed key
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)

	id1 := DeriveNodeID(pub)
	id2 := DeriveNodeID(pub)
	require.Equal(t, id1, id2)        // deterministic
	require.Len(t, id1, 16)           // 10 bytes base32-no-pad = 16 chars
	require.Equal(t, id1, strings.ToLower(id1)) // lowercase
}

func TestLoadOrCreate_PersistsAndReuses(t *testing.T) {
	dir := t.TempDir()

	a, err := LoadOrCreate(dir)
	require.NoError(t, err)
	require.NotEmpty(t, a.NodeID)

	info, err := os.Stat(filepath.Join(dir, "node.key"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	b, err := LoadOrCreate(dir) // second load reuses the key
	require.NoError(t, err)
	require.Equal(t, a.NodeID, b.NodeID)
}
