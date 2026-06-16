package obsd

import (
	"context"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/stretchr/testify/require"
)

func TestStatsCollector_PollComputesActualUtil(t *testing.T) {
	f := sandbox.NewFake()
	ctx := context.Background()
	_, _ = f.Create(ctx, sandbox.CreateSpec{Name: "s1"})

	c := NewStatsCollector(f, listFn(f), ProvisionLimit{CPU: 4, MemKB: 1 << 21}, 4)
	require.NoError(t, c.PollOnce(ctx))

	u, ok := c.Latest("s1")
	require.True(t, ok)
	require.Equal(t, 2, u.Cores)

	au := c.ActualUtil()
	require.InDelta(t, (10.0/100*2)/4, au.CPU, 0.001) // (cpu% * cores)/limit
}

// listFn adapts the fake's List to the names-only signature.
func listFn(f *sandbox.Fake) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		bs, err := f.List(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]string, len(bs))
		for i, b := range bs {
			out[i] = b.Name
		}
		return out, nil
	}
}
