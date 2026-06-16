package membership

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSwarmIdentity_MintPersistAdopt(t *testing.T) {
	dir := t.TempDir()

	// no persisted id, no seeds -> mint standalone
	si, err := LoadOrInit(filepath.Join(dir, "swarm.json"), nil)
	require.NoError(t, err)
	require.Equal(t, ModeStandalone, si.Mode)
	require.NotEmpty(t, si.SwarmID)

	// reload -> same id, rejoin mode if it had peers; here standalone persists id
	si2, err := LoadOrInit(filepath.Join(dir, "swarm.json"), nil)
	require.NoError(t, err)
	require.Equal(t, si.SwarmID, si2.SwarmID)
}

func TestSwarmIdentity_PendingJoinWhenSeedsButNoID(t *testing.T) {
	si, err := LoadOrInit(filepath.Join(t.TempDir(), "swarm.json"), []string{"10.0.0.1:7946"})
	require.NoError(t, err)
	require.Equal(t, ModePendingJoin, si.Mode) // never mint when seeds are configured
	require.Empty(t, si.SwarmID)
}

func TestSwarmIdentity_JoinGuardRejectsMismatch(t *testing.T) {
	require.Error(t, GuardJoin("swarm-A", "swarm-B"))  // different id, same secret -> refuse
	require.NoError(t, GuardJoin("swarm-A", "swarm-A"))
	require.NoError(t, GuardJoin("", "swarm-A"))        // pending-join adopts
}
