package coordinator

import (
	"context"
	"errors"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/scheduler"
	"github.com/stretchr/testify/require"
)

func TestCoordinator_TriesInOrderAndRetriesOnNack(t *testing.T) {
	cands := []scheduler.Candidate{
		{NodeID: "C", LimitCPU: 10, LimitMem: 10, LimitDisk: 10},
		{NodeID: "A", LimitCPU: 10, AllocCPU: 5, LimitMem: 10, LimitDisk: 10},
	}
	var attempts []string
	attempt := func(_ context.Context, nodeID string) (string, error) {
		attempts = append(attempts, nodeID)
		if nodeID == "C" {
			return "", ErrNack // C rejects (admission)
		}
		return nodeID + ".sb", nil
	}
	co := New(func() []scheduler.Candidate { return cands })
	sbID, err := co.Provision(context.Background(), scheduler.Request{CPU: 1, Mem: 1, Disk: 1, Strategy: "least-loaded", RequestID: "r"}, attempt)
	require.NoError(t, err)
	require.Equal(t, "A.sb", sbID)
	require.Equal(t, []string{"C", "A"}, attempts) // C first (lighter), retried A
}

func TestCoordinator_AllNack(t *testing.T) {
	co := New(func() []scheduler.Candidate {
		return []scheduler.Candidate{{NodeID: "A", LimitCPU: 10, LimitMem: 10, LimitDisk: 10}}
	})
	_, err := co.Provision(context.Background(),
		scheduler.Request{CPU: 1, Mem: 1, Disk: 1, RequestID: "r"},
		func(context.Context, string) (string, error) { return "", ErrNack })
	require.True(t, errors.Is(err, ErrNoCapacity))
}

func TestCoordinator_NoEligibleNode(t *testing.T) {
	co := New(func() []scheduler.Candidate { return nil })
	_, err := co.Provision(context.Background(),
		scheduler.Request{CPU: 1, Mem: 1, Disk: 1, RequestID: "r"},
		func(context.Context, string) (string, error) { return "x", nil })
	require.True(t, errors.Is(err, scheduler.ErrNoEligibleNode))
}

func TestCoordinator_HardErrorSurfaces(t *testing.T) {
	boom := errors.New("transport down")
	co := New(func() []scheduler.Candidate {
		return []scheduler.Candidate{{NodeID: "A", LimitCPU: 10, LimitMem: 10, LimitDisk: 10}}
	})
	_, err := co.Provision(context.Background(),
		scheduler.Request{CPU: 1, Mem: 1, Disk: 1, RequestID: "r"},
		func(context.Context, string) (string, error) { return "", boom })
	require.ErrorIs(t, err, boom)
}
