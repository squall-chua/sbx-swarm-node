package ids

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGen_FormatAndUniqueness(t *testing.T) {
	g := NewGen("node123")

	a := g.Sandbox()
	b := g.Sandbox()
	require.NotEqual(t, a, b) // monotonic ULIDs differ

	owner, ok := Owner(a)
	require.True(t, ok)
	require.Equal(t, "node123", owner)

	op := g.Op()
	o2, ok := Owner(op)
	require.True(t, ok)
	require.Equal(t, "node123", o2)
}

func TestOwner_Invalid(t *testing.T) {
	_, ok := Owner("noprefix")
	require.False(t, ok)
	_, ok = Owner("")
	require.False(t, ok)
	_, ok = Owner("trailingdot.")
	require.False(t, ok)
}
