package apiserver

import (
	"context"
	"path/filepath"
	"testing"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
)

func TestInternalProvision_FloorsUnsizedSpec(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	mgr := sandbox.NewManager("n1", sandbox.NewFake(), st, ids.NewGen("n1"))
	mgr.SetCapacity(sandbox.NewCapacity(1, 1e9, 1e9)) // 1 core
	svc := NewInternalService(mgr, nil, nil)

	// An unsized spec (cpus=0) must be floored to >=1 core and reserved, so a
	// SECOND unsized create exceeds the 1-core limit and is NACKed. Without the
	// floor, cpus=0 reserves nothing and both would be accepted.
	r1, err := svc.Provision(context.Background(), &sbxv1.ProvisionRequest{Spec: &sbxv1.CreateSandboxRequest{}})
	require.NoError(t, err)
	require.True(t, r1.Accepted)

	r2, err := svc.Provision(context.Background(), &sbxv1.ProvisionRequest{Spec: &sbxv1.CreateSandboxRequest{}})
	require.NoError(t, err)
	require.False(t, r2.Accepted, "second unsized create must NACK once the floor is reserved")
}

func TestInternalProvision_AdmitsThenNacks(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	mgr := sandbox.NewManager("n1", sandbox.NewFake(), st, ids.NewGen("n1"))
	mgr.SetCapacity(sandbox.NewCapacity(2, 1e9, 1e9)) // 2 cores
	svc := NewInternalService(mgr, nil, nil)

	r1, err := svc.Provision(context.Background(), &sbxv1.ProvisionRequest{
		Spec: &sbxv1.CreateSandboxRequest{Cpus: 2, MemoryBytes: 1},
	})
	require.NoError(t, err)
	require.True(t, r1.Accepted)
	require.NotEmpty(t, r1.SandboxId)

	r2, err := svc.Provision(context.Background(), &sbxv1.ProvisionRequest{
		Spec: &sbxv1.CreateSandboxRequest{Cpus: 1, MemoryBytes: 1},
	})
	require.NoError(t, err)
	require.False(t, r2.Accepted)
	require.Equal(t, "no capacity", r2.Reason)
}

func TestInternalProvision_CordonedTargetNacks(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	mgr := sandbox.NewManager("n1", sandbox.NewFake(), st, ids.NewGen("n1"))
	mgr.SetCapacity(sandbox.NewCapacity(4, 1e9, 1e9)) // ample capacity
	svc := NewInternalService(mgr, nil, func() bool { return true })

	// A node cordoned after the entry node's snapshot must refuse a forwarded
	// provision (its own cordon recheck), even with capacity to spare.
	r, err := svc.Provision(context.Background(), &sbxv1.ProvisionRequest{
		Spec: &sbxv1.CreateSandboxRequest{Cpus: 1, MemoryBytes: 1},
	})
	require.NoError(t, err)
	require.False(t, r.Accepted)
	require.Equal(t, "cordoned", r.Reason)
}

func TestInternalProvision_DedupsByRequestID(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	mgr := sandbox.NewManager("n1", sandbox.NewFake(), st, ids.NewGen("n1"))
	mgr.SetCapacity(sandbox.NewCapacity(4, 1e9, 1e9))
	svc := NewInternalService(mgr, nil, nil)

	r1, err := svc.Provision(context.Background(), &sbxv1.ProvisionRequest{
		RequestId: "op-123", Spec: &sbxv1.CreateSandboxRequest{Cpus: 1, MemoryBytes: 1},
	})
	require.NoError(t, err)
	require.True(t, r1.Accepted)

	r2, err := svc.Provision(context.Background(), &sbxv1.ProvisionRequest{
		RequestId: "op-123", Spec: &sbxv1.CreateSandboxRequest{Cpus: 1, MemoryBytes: 1},
	})
	require.NoError(t, err)
	require.True(t, r2.Accepted)
	require.Equal(t, r1.SandboxId, r2.SandboxId, "same request_id must return the same sandbox")

	recs, err := mgr.List(context.Background())
	require.NoError(t, err)
	require.Len(t, recs, 1, "duplicate request_id must not create a second sandbox")
}

func TestInternalProvision_EmptyRequestIDDoesNotDedup(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	mgr := sandbox.NewManager("n1", sandbox.NewFake(), st, ids.NewGen("n1"))
	mgr.SetCapacity(sandbox.NewCapacity(4, 1e9, 1e9))
	svc := NewInternalService(mgr, nil, nil)

	for i := 0; i < 2; i++ {
		_, err := svc.Provision(context.Background(), &sbxv1.ProvisionRequest{
			Spec: &sbxv1.CreateSandboxRequest{Cpus: 1, MemoryBytes: 1},
		})
		require.NoError(t, err)
	}
	recs, err := mgr.List(context.Background())
	require.NoError(t, err)
	require.Len(t, recs, 2, "empty request_id must not dedup (back-compat)")
}
