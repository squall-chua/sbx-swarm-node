package apiserver

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/git"
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

func TestResolveStrategy_AcceptsLeastActualLoad(t *testing.T) {
	got, err := resolveStrategy("least-actual-load", "")
	require.NoError(t, err)
	require.Equal(t, "least-actual-load", got)
}

func TestRequestFromSpec_CarriesNodeAffinity(t *testing.T) {
	spec := &sbxv1.CreateSandboxRequest{
		Cpus: 1, MemoryBytes: 1,
		NodeAffinity:     map[string]string{"zone": "eu"},
		NodeAntiAffinity: map[string]string{"gpu": "true"},
	}
	req := requestFromSpec(spec, "least-loaded", "r1")
	require.Equal(t, map[string]string{"zone": "eu"}, req.Affinity)
	require.Equal(t, map[string]string{"gpu": "true"}, req.AntiAffinity)
}

func TestToProto_GitFields(t *testing.T) {
	rec := &sandbox.Record{ID: "n1.x", Status: "running", Spec: sandbox.CreateSpec{Branch: "agent/x"}}
	p := toProto(rec)
	require.Equal(t, "agent/x", p.Branch)
	require.Empty(t, p.LastPublish)

	rec.LastPublish = time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)
	require.Equal(t, "2026-06-18T09:00:00Z", toProto(rec).LastPublish)
}

func TestPublishSandbox_AllowPushGate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	// Build a service over a git-backed workspace with AllowPush=false.
	root := t.TempDir()
	base := filepath.Join(root, "base.git")
	out, err := exec.Command("git", "init", "--bare", base).CombinedOutput()
	require.NoError(t, err, string(out))
	ws := git.New(git.Spec{Name: "repo", Base: base, Remote: "origin", AllowPush: false, Allowlist: []string{"git"}})

	mgr := newTestManager(t)
	rec, err := mgr.AdmitAndCreate(context.Background(), sandbox.CreateSpec{
		Agent: "shell", Clone: true, Branch: "agent/x", Workspaces: []sandbox.WorkspaceMount{{Name: "repo"}},
	})
	require.NoError(t, err)

	svc := NewSandboxService(mgr, newTestOps(t))
	svc.SetGit(map[string]*git.Workspace{"repo": ws})

	err = svc.doPublish(context.Background(), rec.ID, "")
	require.Equal(t, codes.FailedPrecondition, status.Code(err)) // allow_push=false
}

func TestStopSandbox_AutoPublishesThenStops(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	upstream := filepath.Join(root, "up.git")
	base := filepath.Join(root, "base.git")
	sbx := filepath.Join(root, "sbx")
	for _, c := range [][]string{
		{"git", "init", "--bare", upstream},
		{"git", "clone", upstream, sbx},
	} {
		out, err := exec.Command(c[0], c[1:]...).CombinedOutput()
		require.NoError(t, err, string(out))
	}
	run := func(dir string, a ...string) {
		out, err := func() ([]byte, error) { c := exec.Command("git", a...); c.Dir = dir; return c.CombinedOutput() }()
		require.NoError(t, err, string(out))
	}
	run(sbx, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "--allow-empty", "-m", "init")
	run(sbx, "push", "origin", "HEAD:main")
	out, err := exec.Command("git", "clone", "--bare", upstream, base).CombinedOutput()
	require.NoError(t, err, string(out))
	run(sbx, "checkout", "-b", "agent/x")
	run(sbx, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "--allow-empty", "-m", "work")

	ws := git.New(git.Spec{
		Name: "repo", Base: base, Remote: "origin", DefaultBranch: "main", AllowPush: true,
		PreSteps:     [][]string{{"git", "fetch", "{remote}", "+refs/heads/*:refs/heads/*"}},
		PublishSteps: [][]string{{"git", "fetch", "{sandbox_remote}", "+refs/heads/{branch}:refs/heads/{branch}"}, {"git", "push", "{remote}", "{branch}"}},
		Allowlist:    []string{"git"},
	})

	mgr := newTestManager(t)
	// The fake backend names the sandbox by spec.Name; force BackendName == "fake" so
	// {sandbox_remote} == "sandbox-fake". (newTestManager uses the fake backend; the
	// record's BackendName equals its swarm id. Override the remote name in the test
	// by registering the base remote under "sandbox-<backendName>".)
	rec, err := mgr.AdmitAndCreate(context.Background(), sandbox.CreateSpec{
		Agent: "shell", Clone: true, Branch: "agent/x", Workspaces: []sandbox.WorkspaceMount{{Name: "repo"}},
	})
	require.NoError(t, err)
	out, err = func() ([]byte, error) { c := exec.Command("git", "remote", "add", "sandbox-"+rec.BackendName, sbx); c.Dir = base; return c.CombinedOutput() }()
	require.NoError(t, err, string(out))

	svc := NewSandboxService(mgr, newTestOps(t))
	svc.SetGit(map[string]*git.Workspace{"repo": ws})

	_, err = svc.StopSandbox(context.Background(), &sbxv1.IdRequest{Id: rec.ID})
	require.NoError(t, err)

	// upstream received agent/x (publish ran before stop), and the sandbox is stopped.
	bo, _ := func() ([]byte, error) { c := exec.Command("git", "branch", "--list", "agent/x"); c.Dir = upstream; return c.CombinedOutput() }()
	require.Contains(t, string(bo), "agent/x")
	got, err := mgr.Get(context.Background(), rec.ID)
	require.NoError(t, err)
	require.Equal(t, "stopped", got.Status)
}
