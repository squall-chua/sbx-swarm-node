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

func TestInternalProvision_AdmitsThenNacks(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	mgr := sandbox.NewManager("n1", sandbox.NewFake(), st, ids.NewGen("n1"))
	mgr.SetCapacity(sandbox.NewCapacity(2, 1e9, 1e9)) // 2 cores
	svc := NewInternalService(mgr)

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
