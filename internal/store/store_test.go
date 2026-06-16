package store

import (
	"encoding/binary"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
)

func TestOpen_CreatesBucketsAndSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.db")

	s, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	require.NoError(t, s.DB().View(func(tx *bolt.Tx) error {
		for _, b := range []string{"meta", "sandboxes", "operations", "idempotency", "blocked_egress", "audit"} {
			require.NotNil(t, tx.Bucket([]byte(b)), "bucket %s", b)
		}
		return nil
	}))
}

func TestOpen_ReopenSucceeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.db")
	s1, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, s1.Close())

	s2, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, s2.Close())
}

func TestOpen_DowngradeGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.db")
	s, err := Open(path)
	require.NoError(t, err)

	// Forge a future schema version on disk.
	require.NoError(t, s.DB().Update(func(tx *bolt.Tx) error {
		future := make([]byte, 8)
		binary.BigEndian.PutUint64(future, 999)
		return tx.Bucket([]byte("meta")).Put([]byte("schema_version"), future)
	}))
	require.NoError(t, s.Close())

	_, err = Open(path)
	require.ErrorContains(t, err, "newer")
}
