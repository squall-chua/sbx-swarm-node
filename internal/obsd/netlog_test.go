package obsd

import (
	"context"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/stretchr/testify/require"
)

func TestNetLog_AccumulatesDistinctPairs(t *testing.T) {
	f := sandbox.NewFake()
	c := NewNetLogCollector(f, func(vm string) (string, bool) { return "sbid-" + vm, true })

	f.SetBlocked([]sandbox.BlockedHost{{Host: "a.com", VMName: "s1"}})
	require.NoError(t, c.PollOnce(context.Background()))
	require.NoError(t, c.PollOnce(context.Background())) // same pair again

	pairs := c.ForSandbox("sbid-s1")
	require.Len(t, pairs, 1) // deduped
	require.Equal(t, "a.com", pairs[0].Host)
	require.Equal(t, 1, c.DistinctCount())
}
