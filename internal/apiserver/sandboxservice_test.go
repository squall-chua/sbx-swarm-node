package apiserver

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/audit"
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
	// The daemon rejects sub-1-GiB memory; the floor must not regress below it.
	require.GreaterOrEqual(t, got.MemoryBytes, int64(1<<30))
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

func TestCreate_PersistsLabels(t *testing.T) {
	svc := newSandboxSvc(t)
	ctx := context.Background()
	rec, err := svc.mgr.Create(ctx, sandbox.CreateSpec{Labels: map[string]string{"idle-stop": "off"}})
	require.NoError(t, err)

	got, err := svc.GetSandbox(ctx, &sbxv1.GetSandboxRequest{Id: rec.ID})
	require.NoError(t, err)
	require.Equal(t, "off", got.Labels["idle-stop"], "labels persist through Create and toProto")

	spec := toSpec(&sbxv1.CreateSandboxRequest{Labels: map[string]string{"team": "eng"}})
	require.Equal(t, "eng", spec.Labels["team"], "toSpec carries request labels")
}

func TestToProto_GitFields(t *testing.T) {
	rec := &sandbox.Record{ID: "n1.x", Status: "running", Spec: sandbox.CreateSpec{Branch: "agent/x"}}
	p := toProto(rec)
	require.Equal(t, "agent/x", p.Branch)
	require.Empty(t, p.LastPublish)

	rec.LastPublish = time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)
	require.Equal(t, "2026-06-18T09:00:00Z", toProto(rec).LastPublish)

	require.Empty(t, toProto(rec).CreatedAt)
	rec.CreatedAt = time.Date(2026, 6, 18, 8, 0, 0, 0, time.UTC)
	require.Equal(t, "2026-06-18T08:00:00Z", toProto(rec).CreatedAt)

	rec.Spec.CPUs, rec.Spec.MemoryBytes, rec.Spec.DiskGB = 2, 1<<30, 10
	p2 := toProto(rec)
	require.Equal(t, int32(2), p2.Cpus)
	require.Equal(t, int64(1<<30), p2.MemoryBytes)
	require.Equal(t, 10.0, p2.DiskGb)
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

	err = svc.doPublish(context.Background(), rec.ID, nil, "admin")
	require.Equal(t, codes.FailedPrecondition, status.Code(err)) // allow_push=false
}

func TestPublishSandbox_AuditRecordsActor(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	base := filepath.Join(root, "base.git")
	out, err := exec.Command("git", "init", "--bare", base).CombinedOutput()
	require.NoError(t, err, string(out))
	// AllowPush + a fetch from the (default-fake, empty) bundle, so Publish fails —
	// but auditPublish runs before the error check, so the actor is still recorded.
	ws := git.New(git.Spec{
		Name: "repo", Base: base, Remote: "origin", AllowPush: true,
		PublishSteps: [][]string{{"git", "fetch", "{sandbox_remote}", "+refs/heads/{branch}:refs/heads/{branch}"}},
		Allowlist:    []string{"git"},
	})

	mgr := newTestManager(t)
	rec, err := mgr.AdmitAndCreate(context.Background(), sandbox.CreateSpec{
		Agent: "shell", Clone: true, Branch: "agent/x", Workspaces: []sandbox.WorkspaceMount{{Name: "repo"}},
	})
	require.NoError(t, err)

	st, err := store.Open(filepath.Join(root, "audit.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	al := audit.New(st, func() int64 { return 1 })

	svc := NewSandboxService(mgr, newTestOps(t))
	svc.SetGit(map[string]*git.Workspace{"repo": ws})
	svc.SetAudit(al)
	svc.publishTimeout = time.Second

	err = svc.doPublish(context.Background(), rec.ID, nil, "admin")
	require.Error(t, err) // publish fails: empty bundle, fetch errors

	entries, err := al.List()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "git.publish", entries[0].Action)
	require.Equal(t, "admin", entries[0].Actor)
	require.Equal(t, "error", entries[0].Outcome)
}

func TestExec_BumpsActivity(t *testing.T) {
	svc := newSandboxSvc(t)
	ctx := context.Background()
	rec, err := svc.mgr.Create(ctx, sandbox.CreateSpec{})
	require.NoError(t, err)
	before := rec.LastActivity

	_, err = svc.Exec(ctx, &sbxv1.ExecRequest{Id: rec.ID, Cmd: []string{"echo", "hi"}})
	require.NoError(t, err)

	got, err := svc.mgr.Get(ctx, rec.ID)
	require.NoError(t, err)
	require.True(t, got.LastActivity.After(before), "Exec bumps LastActivity")
}

func TestSetIdleTimeout(t *testing.T) {
	svc := newSandboxSvc(t)
	svc.SetIdleTimeout(15 * time.Minute)
	require.Equal(t, 15*time.Minute, svc.idleTimeout)
}

// upstreamHasBranch reports whether the upstream bare repo holds branch.
func upstreamHasBranch(t *testing.T, upstream, branch string) bool {
	t.Helper()
	c := exec.Command("git", "branch", "--list", branch)
	c.Dir = upstream
	out, err := c.CombinedOutput()
	require.NoError(t, err, string(out))
	return len(out) > 0
}

// gitBundleFixture mirrors gitPublishFixture but drives the bundle transport: the
// fake backend proxies the sandbox's git commands (rev-parse, for-each-ref, bundle
// create) to the real agent clone, and CopyFrom hands the bundle file back to the
// host — the way doPublish/ListBranches reach the clone. No git-daemon.
func gitPublishFixture(t *testing.T) (*SandboxService, *sandbox.Record, string, *audit.Log) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	upstream := filepath.Join(root, "up.git")
	const name = "repo"
	base := filepath.Join(root, "base", name)
	sbx := filepath.Join(root, "srv", name) // the agent's clone
	for _, c := range [][]string{
		{"git", "init", "--bare", upstream},
		{"git", "clone", upstream, sbx},
	} {
		out, err := exec.Command(c[0], c[1:]...).CombinedOutput()
		require.NoError(t, err, string(out))
	}
	run := func(dir string, a ...string) {
		c := exec.Command("git", a...)
		c.Dir = dir
		out, err := c.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	run(sbx, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "--allow-empty", "-m", "init")
	run(sbx, "push", "origin", "HEAD:main")
	out, err := exec.Command("git", "clone", "--bare", upstream, base).CombinedOutput()
	require.NoError(t, err, string(out))
	run(sbx, "checkout", "-b", "agent/x")
	run(sbx, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "--allow-empty", "-m", "work")

	ws := git.New(git.Spec{
		Name: name, Base: base, Remote: "origin", DefaultBranch: "main", AllowPush: true,
		PublishSteps: [][]string{{"git", "fetch", "{sandbox_remote}", "+refs/heads/{branch}:refs/heads/{branch}"}, {"git", "push", "{remote}", "{branch}"}},
		Allowlist:    []string{"git"},
	})

	mgr := newTestManager(t)
	fake := mgr.Backend().(*sandbox.Fake)
	fake.ExecFunc = func(_ string, cmd []string) (sandbox.ExecResult, error) {
		if len(cmd) == 0 || cmd[0] != "git" {
			return sandbox.ExecResult{ExitCode: 0}, nil // e.g. `rm -f` cleanup
		}
		out, err := exec.Command("git", append([]string{"-C", sbx}, cmd[1:]...)...).Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return sandbox.ExecResult{ExitCode: ee.ExitCode(), Stderr: ee.Stderr}, nil
			}
			return sandbox.ExecResult{ExitCode: 1, Stderr: []byte(err.Error())}, nil
		}
		return sandbox.ExecResult{ExitCode: 0, Stdout: out}, nil
	}
	fake.CopyFromFunc = func(_, remote, local string) error { return copyFile(remote, local) }
	rec, err := mgr.AdmitAndCreate(context.Background(), sandbox.CreateSpec{
		Agent: "shell", Clone: true, Branch: "agent/x", Workspaces: []sandbox.WorkspaceMount{{Name: name}},
	})
	require.NoError(t, err)

	ast, err := store.Open(filepath.Join(root, "audit.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = ast.Close() })
	al := audit.New(ast, func() int64 { return 1 })

	svc := NewSandboxService(mgr, newTestOps(t))
	svc.SetGit(map[string]*git.Workspace{name: ws})
	svc.SetAudit(al)
	svc.bundleDir = t.TempDir()
	return svc, rec, upstream, al
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// Publish transfers the agent's branch with a git bundle (exec + copy), no
// git-daemon, landing it in upstream.
func TestPublishSandbox_ViaBundle(t *testing.T) {
	svc, rec, upstream, _ := gitPublishFixture(t)
	svc.publishTimeout = 10 * time.Second
	require.NoError(t, svc.doPublish(context.Background(), rec.ID, nil, "admin"))
	require.True(t, upstreamHasBranch(t, upstream, "agent/x"), "bundle publish lands the branch upstream")
}

func TestListBranches_ReturnsAgentBranches(t *testing.T) {
	svc, rec, _, _ := gitPublishFixture(t)
	resp, err := svc.ListBranches(context.Background(), &sbxv1.IdRequest{Id: rec.ID})
	require.NoError(t, err)
	require.Contains(t, resp.Branches, "agent/x", "the agent's branch must be listed")
}

// The daemon idle-stops sandboxes on its own, so listing branches must work even
// when the container is down: it reads on-disk refs via exec (which auto-starts
// the sandbox), not the git-daemon (which only comes up on a full boot).
func TestListBranches_AutoStartsStoppedSandbox(t *testing.T) {
	svc, rec, _, _ := gitPublishFixture(t)
	fake := svc.mgr.Backend().(*sandbox.Fake)
	fake.ExecFunc = func(_ string, _ []string) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{ExitCode: 0, Stdout: []byte("agent/x\nmain\n")}, nil
	}
	require.NoError(t, fake.Stop(context.Background(), rec.BackendName))

	resp, err := svc.ListBranches(context.Background(), &sbxv1.IdRequest{Id: rec.ID})
	require.NoError(t, err)
	require.Equal(t, []string{"agent/x", "main"}, resp.Branches)

	bs, err := fake.Get(context.Background(), rec.BackendName)
	require.NoError(t, err)
	require.Equal(t, "running", bs.Status, "exec must auto-start a stopped sandbox")
}

func TestReapIdle_PublishesThenStops(t *testing.T) {
	svc, rec, upstream, _ := gitPublishFixture(t)
	svc.SetIdleTimeout(time.Hour)

	// Not yet idle: now == create time, elapsed ~0 < 1h.
	require.Equal(t, 0, svc.ReapIdle(context.Background(), time.Now()))
	got, err := svc.mgr.Get(context.Background(), rec.ID)
	require.NoError(t, err)
	require.Equal(t, "running", got.Status)

	// Idle: now far past the timeout → publish-then-stop.
	require.Equal(t, 1, svc.ReapIdle(context.Background(), time.Now().Add(2*time.Hour)))
	require.True(t, upstreamHasBranch(t, upstream, "agent/x"), "publish ran before stop")
	got, err = svc.mgr.Get(context.Background(), rec.ID)
	require.NoError(t, err)
	require.Equal(t, "stopped", got.Status)
}

func TestStopSandbox_AutoPublishesThenStops(t *testing.T) {
	svc, rec, upstream, al := gitPublishFixture(t)

	_, err := svc.StopSandbox(context.Background(), &sbxv1.IdRequest{Id: rec.ID})
	require.NoError(t, err)

	// upstream received agent/x (publish ran before stop), and the sandbox is stopped.
	require.True(t, upstreamHasBranch(t, upstream, "agent/x"))
	got, err := svc.mgr.Get(context.Background(), rec.ID)
	require.NoError(t, err)
	require.Equal(t, "stopped", got.Status)

	// auto-publish is system-initiated, so the audit actor is "system" (not empty).
	entries, err := al.List()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "git.publish", entries[0].Action)
	require.Equal(t, "system", entries[0].Actor)
	require.Equal(t, "ok", entries[0].Outcome)
}

func TestKeepAlive_BumpsAndNotFound(t *testing.T) {
	svc := newSandboxSvc(t)
	ctx := context.Background()
	rec, err := svc.mgr.Create(ctx, sandbox.CreateSpec{})
	require.NoError(t, err)
	before := rec.LastActivity

	sb, err := svc.KeepAlive(ctx, &sbxv1.IdRequest{Id: rec.ID})
	require.NoError(t, err)
	require.Equal(t, rec.ID, sb.Id)

	got, err := svc.mgr.Get(ctx, rec.ID)
	require.NoError(t, err)
	require.True(t, got.LastActivity.After(before), "KeepAlive bumps LastActivity")

	_, err = svc.KeepAlive(ctx, &sbxv1.IdRequest{Id: "n1.missing"})
	require.Equal(t, codes.NotFound, status.Code(err))
}

func TestSandboxService_ListOperations(t *testing.T) {
	svc := newSandboxSvc(t)
	// Create a sandbox to produce a "provision" operation through the service.
	_, err := svc.CreateSandbox(context.Background(), &sbxv1.CreateSandboxRequest{})
	require.NoError(t, err)

	resp, err := svc.ListOperations(context.Background(), &sbxv1.ListOperationsRequest{})
	require.NoError(t, err)
	require.NotEmpty(t, resp.Operations)
	require.Equal(t, "provision", resp.Operations[0].Type)
	require.NotEmpty(t, resp.Operations[0].CreatedAt)
}
