package routing

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTable_OwnerByPrefixAndAddr(t *testing.T) {
	tbl := NewTable("self")
	tbl.Upsert("self", "127.0.0.1:1", nil)
	tbl.Upsert("n2", "127.0.0.1:2", nil)

	owner, ok := tbl.Owner("n2.01ABC")
	require.True(t, ok)
	require.Equal(t, "n2", owner)
	require.False(t, tbl.IsLocal("n2.01ABC"))
	require.True(t, tbl.IsLocal("self.01XYZ"))

	addr, ok := tbl.Addr("n2")
	require.True(t, ok)
	require.Equal(t, "127.0.0.1:2", addr)
}

func TestTable_PubKey_PreservedOnMetaUpsert(t *testing.T) {
	tbl := NewTable("self")
	// bulk upsert sets the pubkey
	tbl.Upsert("nB", "10.0.0.2:8443", []byte("PUBKEY"))
	pk, ok := tbl.PubKey("nB")
	require.True(t, ok)
	require.Equal(t, []byte("PUBKEY"), pk)

	// a later meta upsert (empty pubkey) must NOT clobber it
	tbl.Upsert("nB", "10.0.0.2:8443", nil)
	pk, ok = tbl.PubKey("nB")
	require.True(t, ok)
	require.Equal(t, []byte("PUBKEY"), pk)

	_, ok = tbl.PubKey("unknown")
	require.False(t, ok)
}
