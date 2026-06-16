package routing

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTable_OwnerByPrefixAndAddr(t *testing.T) {
	tbl := NewTable("self")
	tbl.Upsert("self", "127.0.0.1:1", false)
	tbl.Upsert("n2", "127.0.0.1:2", false)

	owner, ok := tbl.Owner("n2.01ABC")
	require.True(t, ok)
	require.Equal(t, "n2", owner)
	require.False(t, tbl.IsLocal("n2.01ABC"))
	require.True(t, tbl.IsLocal("self.01XYZ"))

	addr, ok := tbl.Addr("n2")
	require.True(t, ok)
	require.Equal(t, "127.0.0.1:2", addr)

	// cordoned nodes are excluded from scheduling candidates
	tbl.Upsert("n2", "127.0.0.1:2", true)
	require.True(t, tbl.IsCordoned("n2"))
}
