package sandbox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFake_StatsAndBlocked(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	_, _ = f.Create(ctx, CreateSpec{Name: "s1"})

	u, err := f.Stats(ctx, "s1")
	require.NoError(t, err)
	require.Equal(t, 2, u.Cores)

	f.SetBlocked([]BlockedHost{{Host: "evil.example", VMName: "s1"}})
	bl, err := f.BlockedEgress(ctx)
	require.NoError(t, err)
	require.Len(t, bl, 1)
}
