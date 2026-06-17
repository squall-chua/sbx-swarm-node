package apiserver

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/ops"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newSandboxSvc(t *testing.T) *SandboxService {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	gen := ids.NewGen("n1")
	mgr := sandbox.NewManager("n1", sandbox.NewFake(), st, gen)
	return NewSandboxService(mgr, ops.NewManager(st, gen))
}

func TestSandboxService_CreateThenGetList(t *testing.T) {
	svc := newSandboxSvc(t)
	ctx := context.Background()

	op, err := svc.CreateSandbox(ctx, &sbxv1.CreateSandboxRequest{Cpus: 1, MemoryBytes: 1 << 30})
	require.NoError(t, err)
	require.NotEmpty(t, op.Id)

	// provision runs async; wait until the op carries a sandbox id
	var sbID string
	require.Eventually(t, func() bool {
		got, _ := svc.ops.Get(op.Id)
		if got != nil && got.State == "done" {
			sbID = got.SandboxID
			return true
		}
		return false
	}, time.Second, 10*time.Millisecond)

	got, err := svc.GetSandbox(ctx, &sbxv1.GetSandboxRequest{Id: sbID})
	require.NoError(t, err)
	require.Equal(t, "running", got.Status)

	list, err := svc.ListSandboxes(ctx, &sbxv1.ListSandboxesRequest{})
	require.NoError(t, err)
	require.Len(t, list.Sandboxes, 1)
}

func TestSandboxService_Exec(t *testing.T) {
	svc := newSandboxSvc(t)
	ctx := context.Background()
	// create synchronously via the manager for a direct id
	rec, err := svc.mgr.Create(ctx, sandbox.CreateSpec{})
	require.NoError(t, err)

	res, err := svc.Exec(ctx, &sbxv1.ExecRequest{Id: rec.ID, Cmd: []string{"echo", "hi"}})
	require.NoError(t, err)
	require.Equal(t, int32(0), res.ExitCode)
}

func TestCreateSandbox_RejectsBadStrategy(t *testing.T) {
	svc := newSandboxSvc(t)
	_, err := svc.CreateSandbox(context.Background(), &sbxv1.CreateSandboxRequest{Strategy: "bogus"})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestEffectiveSizing_FillsUnsized(t *testing.T) {
	defaults := sandbox.Resources{CPUCores: 2, MemoryBytes: 1024, DiskGB: 3}
	got := effectiveSpec(&sbxv1.CreateSandboxRequest{}, defaults)
	require.Equal(t, int32(2), got.Cpus)
	require.Equal(t, int64(1024), got.MemoryBytes)
	require.Equal(t, 3.0, got.DiskGb)

	got = effectiveSpec(&sbxv1.CreateSandboxRequest{Cpus: 8, MemoryBytes: 4096, DiskGb: 9}, defaults)
	require.Equal(t, int32(8), got.Cpus)
	require.Equal(t, int64(4096), got.MemoryBytes)
	require.Equal(t, 9.0, got.DiskGb)
}

func TestEffectiveSizing_BuiltinFloorWhenNoDefault(t *testing.T) {
	got := effectiveSpec(&sbxv1.CreateSandboxRequest{}, sandbox.Resources{})
	require.Equal(t, floorCPUCores, got.Cpus)
	require.Equal(t, floorMemoryBytes, got.MemoryBytes)
	require.Equal(t, floorDiskGB, got.DiskGb)
}
