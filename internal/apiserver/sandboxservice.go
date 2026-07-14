package apiserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/audit"
	"github.com/squall-chua/sbx-swarm-node/internal/events"
	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/git"
	"github.com/squall-chua/sbx-swarm-node/internal/ops"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/squall-chua/sbx-swarm-node/internal/scheduler"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// PlaceFunc places a sized request and returns the created sandbox id. Injected
// by node.go (coordinator-backed); nil falls back to a local admit+create.
type PlaceFunc func(ctx context.Context, req scheduler.Request, spec *sbxv1.CreateSandboxRequest) (sandboxID string, err error)

// SandboxService implements sbxv1.SandboxServiceServer over the sandbox Manager.
type SandboxService struct {
	sbxv1.UnimplementedSandboxServiceServer
	mgr              *sandbox.Manager
	ops              *ops.Manager
	obs              ObserveDeps
	place            PlaceFunc
	defaultStrategy  string
	defaultResources sandbox.Resources
	gitWS            map[string]*git.Workspace
	audit            *audit.Log
	events           events.Publisher
	idleTimeout      time.Duration
	publishTimeout   time.Duration // 0 → defaultPublishTimeout; bounds the bundle publish
	bundleDir        string        // "" → "/tmp"; where the publish bundle is staged (host + container side)
	maxUploadBytes   int64         // 0 → defaultMaxUploadBytes; per-request upload ceiling
}

// SetGit wires git-backed workspaces (by name) for the publish path.
func (s *SandboxService) SetGit(ws map[string]*git.Workspace) { s.gitWS = ws }

// SetAudit wires the audit log for git operations.
func (s *SandboxService) SetAudit(a *audit.Log) { s.audit = a }

// SetEvents wires the event publisher for publish success/failure signals.
func (s *SandboxService) SetEvents(p events.Publisher) { s.events = p }

// SetIdleTimeout configures the idle-stop threshold. 0 disables both the reaper
// sweep and the agent-run keepalive throttle.
func (s *SandboxService) SetIdleTimeout(d time.Duration) { s.idleTimeout = d }

// SetMaxUploadBytes sets the per-request file-upload ceiling (0 → default).
func (s *SandboxService) SetMaxUploadBytes(n int64) { s.maxUploadBytes = n }

// WithPlacement wires placement (coordinator) + sizing defaults.
func (s *SandboxService) WithPlacement(place PlaceFunc, defaultStrategy string, defaults sandbox.Resources) {
	s.place = place
	s.defaultStrategy = defaultStrategy
	s.defaultResources = defaults
}

const (
	floorCPUCores    int32 = 1
	floorMemoryBytes int64 = 1 << 30 // 1 GiB — the sbx daemon rejects anything below this
	floorDiskGB            = 1.0
)

// effectiveSpec returns a copy of r with each unset resource filled from the
// configured default, else the built-in floor (no untracked sandboxes).
// ponytail: floor tracks the daemon's minimum (1 GiB memory); source it from
// the daemon once the SDK exposes it.
func effectiveSpec(r *sbxv1.CreateSandboxRequest, defaults sandbox.Resources) *sbxv1.CreateSandboxRequest {
	out := proto.Clone(r).(*sbxv1.CreateSandboxRequest)
	if out.Cpus <= 0 {
		if defaults.CPUCores > 0 {
			out.Cpus = int32(defaults.CPUCores)
		} else {
			out.Cpus = floorCPUCores
		}
	}
	if out.MemoryBytes <= 0 {
		if defaults.MemoryBytes > 0 {
			out.MemoryBytes = defaults.MemoryBytes
		} else {
			out.MemoryBytes = floorMemoryBytes
		}
	}
	if out.DiskGb <= 0 {
		if defaults.DiskGB > 0 {
			out.DiskGb = defaults.DiskGB
		} else {
			out.DiskGb = floorDiskGB
		}
	}
	return out
}

// resolveStrategy applies precedence request -> config default -> least-loaded
// and validates the result.
func resolveStrategy(reqStrategy, defaultStrategy string) (string, error) {
	s := reqStrategy
	if s == "" {
		s = defaultStrategy
	}
	if s == "" {
		s = "least-loaded"
	}
	switch s {
	case "least-loaded", "bin-pack", "spread", "least-actual-load":
		return s, nil
	default:
		return "", status.Errorf(codes.InvalidArgument, "unknown strategy %q", reqStrategy)
	}
}

// requestFromSpec builds the scheduler Request from a sized spec.
func requestFromSpec(spec *sbxv1.CreateSandboxRequest, strategy, requestID string) scheduler.Request {
	ws := make([]string, 0, len(spec.Workspaces))
	for _, w := range spec.Workspaces {
		ws = append(ws, w.Name)
	}
	var caps []string
	if spec.Clone {
		caps = append(caps, "clone") // ADR-0009 capability predicate
	}
	return scheduler.Request{
		CPU: float64(spec.Cpus), Mem: float64(spec.MemoryBytes) / 1024, Disk: spec.DiskGb,
		Workspaces: ws, Template: spec.Template, Capabilities: caps,
		Affinity: spec.NodeAffinity, AntiAffinity: spec.NodeAntiAffinity,
		Strategy: strategy, RequestID: requestID,
	}
}

// NewSandboxService builds the service.
func NewSandboxService(mgr *sandbox.Manager, opsM *ops.Manager) *SandboxService {
	return &SandboxService{mgr: mgr, ops: opsM}
}

// ToSpecForProvision maps a proto create request to a sandbox.CreateSpec.
func ToSpecForProvision(r *sbxv1.CreateSandboxRequest) sandbox.CreateSpec { return toSpec(r) }

func toSpec(r *sbxv1.CreateSandboxRequest) sandbox.CreateSpec {
	ws := make([]sandbox.WorkspaceMount, 0, len(r.Workspaces))
	for _, w := range r.Workspaces {
		ws = append(ws, sandbox.WorkspaceMount{Name: w.Name, ReadOnly: w.ReadOnly})
	}
	var reviewRef *sandbox.ReviewRef
	if rr := r.ReviewRef; rr != nil {
		reviewRef = &sandbox.ReviewRef{Workspace: rr.Workspace, ID: rr.Id}
	}
	return sandbox.CreateSpec{
		Agent: r.Agent, Template: r.Template, CPUs: int(r.Cpus),
		MemoryBytes: r.MemoryBytes, DiskGB: r.DiskGb, Clone: r.Clone, Branch: r.Branch, ReviewRef: reviewRef, Workspaces: ws, Env: r.Env, Labels: r.Labels,
		DisplayName: r.Name,
	}
}

func toProto(rec *sandbox.Record) *sbxv1.Sandbox {
	ports := make([]*sbxv1.Port, 0, len(rec.Ports))
	for _, p := range rec.Ports {
		ports = append(ports, &sbxv1.Port{ContainerPort: int32(p.ContainerPort), HostPort: int32(p.HostPort)})
	}
	ws := make([]*sbxv1.WorkspaceMount, 0, len(rec.Spec.Workspaces))
	for _, w := range rec.Spec.Workspaces {
		ws = append(ws, &sbxv1.WorkspaceMount{Name: w.Name, ReadOnly: w.ReadOnly})
	}
	var lastPub string
	if !rec.LastPublish.IsZero() {
		lastPub = rec.LastPublish.UTC().Format(time.RFC3339)
	}
	var createdAt string
	if !rec.CreatedAt.IsZero() {
		createdAt = rec.CreatedAt.UTC().Format(time.RFC3339)
	}
	return &sbxv1.Sandbox{
		Id: rec.ID, OwnerNode: rec.OwnerNode, Status: rec.Status, Ports: ports, Labels: rec.Labels,
		Branch: rec.Spec.Branch, LastPublish: lastPub, Agent: rec.Spec.Agent,
		Name: displayName(rec), Workspaces: ws, CreatedAt: createdAt,
		Cpus: int32(rec.Spec.CPUs), MemoryBytes: rec.Spec.MemoryBytes, DiskGb: rec.Spec.DiskGB,
	}
}

// displayName returns the human-readable sandbox name: the custom name if set,
// else one derived from agent + first workspace + a short id suffix (the routing
// id is never readable). Display only — not unique, not a key.
func displayName(rec *sandbox.Record) string {
	if rec.Spec.DisplayName != "" {
		return rec.Spec.DisplayName
	}
	parts := make([]string, 0, 3)
	if rec.Spec.Agent != "" {
		parts = append(parts, rec.Spec.Agent)
	}
	if len(rec.Spec.Workspaces) > 0 {
		parts = append(parts, rec.Spec.Workspaces[0].Name)
	}
	if i := strings.LastIndexByte(rec.ID, '.'); i >= 0 && i+1 < len(rec.ID) {
		u := rec.ID[i+1:]
		if len(u) > 6 {
			u = u[len(u)-6:]
		}
		parts = append(parts, strings.ToLower(u))
	}
	if len(parts) == 0 {
		return rec.ID
	}
	return strings.Join(parts, "-")
}

func opProto(op *ops.Operation) *sbxv1.Operation {
	return &sbxv1.Operation{Id: op.ID, Type: op.Type, State: op.State, SandboxId: op.SandboxID, Error: op.Error}
}

// idempotencyKey reads the Idempotency-Key from gRPC/gateway metadata.
func idempotencyKey(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get("idempotency-key"); len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

// CreateSandbox starts an async provision operation (idempotent).
func (s *SandboxService) CreateSandbox(ctx context.Context, r *sbxv1.CreateSandboxRequest) (*sbxv1.Operation, error) {
	strategy, err := resolveStrategy(r.Strategy, s.defaultStrategy)
	if err != nil {
		return nil, err
	}
	idemKey := idempotencyKey(ctx)
	op, existed, err := s.ops.Start(ctx, "provision", idemKey)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if existed && s.provisionSandboxGone(ctx, op) {
		// A keyed retry normally returns the original provision op. But if that
		// op's sandbox has been deleted or marked lost (e.g. force-deleted or
		// crashed under a still-live node), returning it strands a rebind on a
		// dead sandbox. Drop the stale mapping and provision fresh so the caller
		// gets a new sandbox under the same identity.
		if cerr := s.ops.ClearIdempotency(idemKey); cerr != nil {
			return nil, status.Error(codes.Internal, cerr.Error())
		}
		op, existed, err = s.ops.Start(ctx, "provision", idemKey)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	if existed {
		return opProto(op), nil
	}
	sized := effectiveSpec(r, s.defaultResources)
	req := requestFromSpec(sized, strategy, op.ID)
	s.ops.Run(op.ID, func() (string, error) {
		if s.place != nil {
			return s.place(context.Background(), req, sized)
		}
		// Fallback (no coordinator wired, e.g. unit tests): local admit+create.
		rec, cerr := s.mgr.AdmitAndCreate(context.Background(), toSpec(sized))
		if cerr != nil {
			return "", cerr
		}
		return rec.ID, nil
	})
	return opProto(op), nil
}

// provisionSandboxGone reports whether a completed provision op's sandbox no
// longer exists or has been marked lost — the signal that a same-idempotency-key
// retry must re-provision instead of returning the dead op. A pending/running op
// is a genuine in-flight create and is never treated as stale.
func (s *SandboxService) provisionSandboxGone(ctx context.Context, op *ops.Operation) bool {
	if op == nil || op.State != "done" || op.SandboxID == "" {
		return false
	}
	rec, err := s.mgr.Get(ctx, op.SandboxID)
	if errors.Is(err, sandbox.ErrNotFound) {
		return true
	}
	return err == nil && rec.Status == "lost"
}

func (s *SandboxService) GetSandbox(ctx context.Context, r *sbxv1.GetSandboxRequest) (*sbxv1.Sandbox, error) {
	rec, err := s.mgr.Get(ctx, r.Id)
	if err == sandbox.ErrNotFound {
		return nil, status.Error(codes.NotFound, "sandbox not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return toProto(rec), nil
}

func (s *SandboxService) ListSandboxes(ctx context.Context, r *sbxv1.ListSandboxesRequest) (*sbxv1.ListSandboxesResponse, error) {
	recs, err := s.mgr.List(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &sbxv1.ListSandboxesResponse{}
	for _, rec := range recs {
		if r.Status != "" && rec.Status != r.Status {
			continue
		}
		out.Sandboxes = append(out.Sandboxes, toProto(rec))
	}
	return out, nil
}

func (s *SandboxService) DeleteSandbox(ctx context.Context, r *sbxv1.DeleteSandboxRequest) (*sbxv1.Operation, error) {
	op, _, err := s.ops.Start(ctx, "remove", "")
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	id := r.Id
	s.ops.Run(op.ID, func() (string, error) { return id, s.mgr.Delete(context.Background(), id) })
	return opProto(op), nil
}

func (s *SandboxService) StartSandbox(ctx context.Context, r *sbxv1.IdRequest) (*sbxv1.Sandbox, error) {
	if err := s.mgr.Start(ctx, r.Id); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return s.GetSandbox(ctx, &sbxv1.GetSandboxRequest{Id: r.Id})
}

// KeepAlive records consumer Activity on a sandbox, resetting its idle clock.
func (s *SandboxService) KeepAlive(ctx context.Context, r *sbxv1.IdRequest) (*sbxv1.Sandbox, error) {
	if err := s.mgr.BumpActivity(ctx, r.Id); err == sandbox.ErrNotFound {
		return nil, status.Error(codes.NotFound, "sandbox not found")
	} else if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return s.GetSandbox(ctx, &sbxv1.GetSandboxRequest{Id: r.Id})
}

func (s *SandboxService) StopSandbox(ctx context.Context, r *sbxv1.IdRequest) (*sbxv1.Sandbox, error) {
	s.maybeAutoPublish(ctx, r.Id) // publish-then-stop: the sandbox-<name> fetch needs the live daemon
	if err := s.mgr.Stop(ctx, r.Id); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return s.GetSandbox(ctx, &sbxv1.GetSandboxRequest{Id: r.Id})
}

// maybeAutoPublish best-effort publishes the recorded branch of a clone-mode,
// push-allowed sandbox. Failures are audited + logged inside doPublish and do NOT
// block the caller (ADR: auto-publish is best-effort).
func (s *SandboxService) maybeAutoPublish(ctx context.Context, id string) {
	if s.gitWS == nil {
		return
	}
	rec, err := s.mgr.Get(ctx, id)
	if err != nil || len(rec.Spec.Workspaces) != 1 || !rec.Spec.Clone || rec.Spec.Branch == "" {
		return
	}
	ws := s.gitWS[rec.Spec.Workspaces[0].Name]
	if ws == nil || !ws.AllowPush() {
		return // not git-backed or pull-only: silent skip
	}
	// Attribute to the user when ctx carries one (synchronous StopSandbox); fall
	// back to "system" for background triggers (AgentRun success) whose ctx has
	// no principal.
	actor := principalFromContext(ctx).userRole
	if actor == "" {
		actor = "system"
	}
	if perr := s.doPublish(ctx, id, nil, actor); perr != nil {
		slog.Warn("auto-publish failed", "sandbox", id, "err", perr)
	}
}

// ReapIdle idle-stops every running, non-exempt sandbox past the idle timeout,
// auto-publishing git-backed ones first (publish-before-stop: the live daemon is
// needed for the sandbox-<name> fetch). now is a parameter for testability.
// Returns the number stopped. A publish failure does NOT skip the stop (parity
// with graceful StopSandbox).
func (s *SandboxService) ReapIdle(ctx context.Context, now time.Time) int {
	idle, err := s.mgr.IdleRunning(ctx, now, s.idleTimeout)
	if err != nil {
		slog.Warn("reaper: list idle failed", "err", err)
		return 0
	}
	n := 0
	for _, rec := range idle {
		s.maybeAutoPublish(ctx, rec.ID) // best-effort, before stop
		if serr := s.mgr.Stop(ctx, rec.ID); serr != nil {
			slog.Warn("reaper: stop failed", "sandbox", rec.ID, "err", serr)
			continue
		}
		slog.Info("idle-stopped sandbox", "sandbox", rec.ID, "idle", now.Sub(rec.LastActivity).String())
		n++
	}
	return n
}

func (s *SandboxService) Exec(ctx context.Context, r *sbxv1.ExecRequest) (*sbxv1.ExecResponse, error) {
	name, err := s.mgr.Resolve(ctx, r.Id)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	_ = s.mgr.BumpActivity(ctx, r.Id) // Exec is Activity
	res, err := s.mgr.Backend().Exec(ctx, name, r.Cmd, sandbox.ExecOpts{Workdir: r.Workdir, Env: r.Env})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &sbxv1.ExecResponse{ExitCode: int32(res.ExitCode), Stdout: res.Stdout, Stderr: res.Stderr}, nil
}

// WriteFiles lands a batch of files into a sandbox's VM over the reliable chunked
// request path (the same transport as the REST upload; see filescopy.go). It is
// the gRPC-only ingress the Agency uses to drop an Opencode harness before
// starting the agent. Each write is guarded against path traversal, staged and
// byte-verified, then optionally chmod'd. On any file's failure the whole call
// fails (Agency treats the harness as all-or-nothing).
func (s *SandboxService) WriteFiles(ctx context.Context, r *sbxv1.WriteFilesRequest) (*sbxv1.WriteFilesResponse, error) {
	name, err := s.mgr.Resolve(ctx, r.Id)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	b := s.mgr.Backend()
	actor := principalFromContext(ctx).userRole
	for _, fw := range r.Files {
		dest, err := resolveUploadDest(fw.Path)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "%s: %v", fw.Path, err)
		}
		werr := copyReaderToSandbox(ctx, b, name, bytes.NewReader(fw.Content), int64(len(fw.Content)), dest)
		if werr == nil && fw.Mode != 0 {
			_, werr = execChecked(ctx, b, name, "chmod", fmt.Sprintf("%o", fw.Mode), dest)
		}
		s.auditWrite(actor, dest, werr)
		if werr != nil {
			return nil, status.Errorf(codes.Internal, "write %s: %v", dest, werr)
		}
	}
	_ = s.mgr.BumpActivity(ctx, r.Id) // WriteFiles is Activity
	return &sbxv1.WriteFilesResponse{FilesWritten: int32(len(r.Files))}, nil
}

// auditWrite records one file.write per landed file, attributed to the gRPC
// principal (blank => "system" for a node-forwarded call).
func (s *SandboxService) auditWrite(actor, target string, err error) {
	if s.audit == nil {
		return
	}
	if actor == "" {
		actor = "system"
	}
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	_ = s.audit.Record(audit.Entry{Actor: actor, Action: "file.write", Target: target, Outcome: outcome})
}

func (s *SandboxService) AgentRun(ctx context.Context, r *sbxv1.AgentRunRequest) (*sbxv1.Operation, error) {
	name, err := s.mgr.Resolve(ctx, r.Id)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	op, _, err := s.ops.Start(ctx, "agent-run", idempotencyKey(ctx))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	cmd, opts := r.Cmd, sandbox.ExecOpts{Workdir: r.Workdir, Env: r.Env}
	sbID := r.Id
	publishOnSuccess := r.PublishOnSuccess
	s.ops.Run(op.ID, func() (string, error) {
		_ = s.mgr.BumpActivity(context.Background(), sbID) // run started = Activity
		did, derr := s.mgr.Backend().ExecDetached(context.Background(), name, cmd, opts)
		if derr != nil {
			return "", derr
		}
		lastTouch := time.Now()
		for { // poll to completion (M1c: simple loop; M1d streams progress)
			st, perr := s.mgr.Backend().PollDetached(context.Background(), name, did)
			if perr != nil {
				return "", perr
			}
			if st.Done {
				if st.ExitCode != 0 {
					return sbID, status.Errorf(codes.Internal, "agent run exited %d", st.ExitCode)
				}
				if publishOnSuccess {
					s.maybeAutoPublish(context.Background(), sbID) // best-effort
				}
				return sbID, nil
			}
			// Keep a long-running agent's sandbox alive: bump on a timeout/2 throttle
			// so it is never idle-stopped mid-run (skip when the reaper is disabled).
			if s.idleTimeout > 0 && time.Since(lastTouch) > s.idleTimeout/2 {
				_ = s.mgr.BumpActivity(context.Background(), sbID)
				lastTouch = time.Now()
			}
			time.Sleep(200 * time.Millisecond)
		}
	})
	return opProto(op), nil
}

func (s *SandboxService) PublishPort(ctx context.Context, r *sbxv1.PublishPortRequest) (*sbxv1.Port, error) {
	name, err := s.mgr.Resolve(ctx, r.Id)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	p, err := s.mgr.Backend().PublishPort(ctx, name, int(r.ContainerPort))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &sbxv1.Port{ContainerPort: int32(p.ContainerPort), HostPort: int32(p.HostPort)}, nil
}

func (s *SandboxService) UnpublishPort(ctx context.Context, r *sbxv1.UnpublishPortRequest) (*sbxv1.Empty, error) {
	name, err := s.mgr.Resolve(ctx, r.Id)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	if err := s.mgr.Backend().UnpublishPort(ctx, name, int(r.ContainerPort)); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &sbxv1.Empty{}, nil
}

func (s *SandboxService) ListPorts(ctx context.Context, r *sbxv1.IdRequest) (*sbxv1.ListPortsResponse, error) {
	name, err := s.mgr.Resolve(ctx, r.Id)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	ports, err := s.mgr.Backend().Ports(ctx, name)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &sbxv1.ListPortsResponse{}
	for _, p := range ports {
		out.Ports = append(out.Ports, &sbxv1.Port{ContainerPort: int32(p.ContainerPort), HostPort: int32(p.HostPort)})
	}
	return out, nil
}

// defaultPublishTimeout bounds the publish (bundle create + copy-out + fetch) so a
// wedged sandbox cannot block the RPC forever.
const defaultPublishTimeout = 2 * time.Minute

// agentHeadBranch returns the branch the agent's clone is currently on (its HEAD),
// execed inside the sandbox (exec defaults its cwd to the workspace).
func (s *SandboxService) agentHeadBranch(ctx context.Context, backendName string) (string, error) {
	res, err := s.mgr.Backend().Exec(ctx, backendName, []string{"git", "rev-parse", "--abbrev-ref", "HEAD"}, sandbox.ExecOpts{})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("git rev-parse: %s", strings.TrimSpace(string(res.Stderr)))
	}
	b := strings.TrimSpace(string(res.Stdout))
	if b == "" || b == "HEAD" {
		return "", fmt.Errorf("sandbox is in detached HEAD; specify a branch to publish")
	}
	return b, nil
}

// gitTarget resolves a clone-mode, git-backed sandbox to its record + workspace.
// It does NOT gate on status (the record can lag the daemon's own idle-stop).
func (s *SandboxService) gitTarget(ctx context.Context, sandboxID string) (*sandbox.Record, *git.Workspace, error) {
	rec, err := s.mgr.Get(ctx, sandboxID)
	if err == sandbox.ErrNotFound {
		return nil, nil, status.Error(codes.NotFound, "sandbox not found")
	}
	if err != nil {
		return nil, nil, status.Error(codes.Internal, err.Error())
	}
	if len(rec.Spec.Workspaces) != 1 {
		return nil, nil, status.Error(codes.FailedPrecondition, "sandbox is not clone-mode")
	}
	ws := s.gitWS[rec.Spec.Workspaces[0].Name]
	if ws == nil {
		return nil, nil, status.Error(codes.FailedPrecondition, "workspace is not git-backed")
	}
	return rec, ws, nil
}

// bundleBranches packs the given branches from the agent's clone into a git bundle
// and copies it to a host file, returning the host path + a cleanup. This replaces
// the in-container git-daemon: it needs only exec + file copy, so it works on any
// running sandbox regardless of whether the git-daemon process is alive (it only
// comes back on a full boot, not the exec-restart the daemon-API path does). The
// bundle is a valid git remote, so the configured publish steps fetch from it
// unchanged ({sandbox_remote} = the bundle path).
func (s *SandboxService) bundleBranches(ctx context.Context, backendName string, branches []string) (string, func(), error) {
	dir := s.bundleDir
	if dir == "" {
		dir = "/tmp"
	}
	hostf, err := os.CreateTemp(dir, "sbxpub-*.bundle")
	if err != nil {
		return "", nil, status.Errorf(codes.Internal, "bundle: stage file: %v", err)
	}
	_ = hostf.Close()
	hostPath := hostf.Name()
	// Distinct from hostPath so the cross-namespace CopyFrom is never a self-copy.
	containerPath := filepath.Join(dir, "in-"+filepath.Base(hostPath))
	cleanup := func() {
		_ = os.Remove(hostPath)
		_, _ = s.mgr.Backend().Exec(context.WithoutCancel(ctx), backendName, []string{"rm", "-f", containerPath}, sandbox.ExecOpts{})
	}

	create := append([]string{"git", "bundle", "create", containerPath}, branches...)
	res, err := s.mgr.Backend().Exec(ctx, backendName, create, sandbox.ExecOpts{})
	if err != nil {
		cleanup()
		return "", nil, status.Errorf(codes.Internal, "bundle: %v", err)
	}
	if res.ExitCode != 0 {
		cleanup()
		return "", nil, status.Errorf(codes.Internal, "bundle: git exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	if err := s.mgr.Backend().CopyFrom(ctx, backendName, containerPath, hostPath); err != nil {
		cleanup()
		return "", nil, status.Errorf(codes.Internal, "bundle: copy out: %v", err)
	}
	return hostPath, cleanup, nil
}

func (s *SandboxService) doPublish(ctx context.Context, sandboxID string, reqBranches []string, actor string) error {
	rec, ws, err := s.gitTarget(ctx, sandboxID)
	if err != nil {
		return err
	}
	if !ws.AllowPush() {
		return status.Error(codes.FailedPrecondition, "workspace does not allow push")
	}

	to := s.publishTimeout
	if to <= 0 {
		to = defaultPublishTimeout
	}
	pubCtx, cancel := context.WithTimeout(ctx, to)
	defer cancel()

	// No explicit selection → the branch the agent actually worked on (clone HEAD),
	// not the stale provision-time metadata.
	branches := reqBranches
	if len(branches) == 0 {
		b, err := s.agentHeadBranch(pubCtx, rec.BackendName)
		if err != nil {
			return status.Errorf(codes.FailedPrecondition, "determine branch: %v", err)
		}
		branches = []string{b}
	}

	bundlePath, cleanup, err := s.bundleBranches(pubCtx, rec.BackendName, branches)
	if err != nil {
		s.auditPublish(ws.Name(), branches[0], actor, err)
		s.emit("sandbox.publish_failed", sandboxID, map[string]string{"branch": branches[0]})
		return status.Errorf(codes.Internal, "publish: %v", err)
	}
	defer cleanup()

	// Run the configured publish pipeline once per selected branch.
	var firstErr error
	for _, branch := range branches {
		perr := ws.Publish(pubCtx, branch, bundlePath)
		s.auditPublish(ws.Name(), branch, actor, perr)
		if perr != nil {
			s.emit("sandbox.publish_failed", sandboxID, map[string]string{"branch": branch})
			if firstErr == nil {
				firstErr = perr
			}
			continue
		}
		s.emit("sandbox.published", sandboxID, map[string]string{"branch": branch})
	}
	if firstErr != nil {
		return status.Errorf(codes.Internal, "publish: %v", firstErr)
	}
	_ = s.mgr.SetLastPublish(pubCtx, sandboxID, time.Now())
	return nil
}

// ListBranches lists the agent's branches for the publish-selection UI, read
// straight from the clone with `git for-each-ref`. The exec auto-starts an
// idle-stopped sandbox; reading on-disk refs needs no in-container git-daemon
// (which, unlike publish, only comes up on a full boot — not an exec-restart).
func (s *SandboxService) ListBranches(ctx context.Context, r *sbxv1.IdRequest) (*sbxv1.ListBranchesResponse, error) {
	rec, _, err := s.gitTarget(ctx, r.Id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := s.mgr.Backend().Exec(ctx, rec.BackendName,
		[]string{"git", "for-each-ref", "--format=%(refname:short)", "refs/heads/"}, sandbox.ExecOpts{})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list branches: %v", err)
	}
	if res.ExitCode != 0 {
		return nil, status.Errorf(codes.Internal, "list branches: git exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	var branches []string
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		if b := strings.TrimSpace(line); b != "" {
			branches = append(branches, b)
		}
	}
	return &sbxv1.ListBranchesResponse{Branches: branches}, nil
}

func (s *SandboxService) auditPublish(workspace, branch, actor string, err error) {
	if s.audit == nil {
		return
	}
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	_ = s.audit.Record(audit.Entry{Actor: actor, Action: "git.publish", Target: workspace + "@" + branch, Outcome: outcome})
}

func (s *SandboxService) emit(eventType, sandboxID string, payload map[string]string) {
	if s.events != nil {
		s.events.Publish(eventType, sandboxID, payload)
	}
}

func (s *SandboxService) ListOperations(_ context.Context, r *sbxv1.ListOperationsRequest) (*sbxv1.ListOperationsResponse, error) {
	list, err := s.ops.List(int(r.Limit))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &sbxv1.ListOperationsResponse{}
	for _, op := range list {
		out.Operations = append(out.Operations, &sbxv1.OperationSummary{
			Id: op.ID, Type: op.Type, State: op.State, SandboxId: op.SandboxID,
			Error:     op.Error,
			CreatedAt: op.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt: op.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// PublishSandbox starts an async git-publish operation.
func (s *SandboxService) PublishSandbox(ctx context.Context, r *sbxv1.PublishSandboxRequest) (*sbxv1.Operation, error) {
	op, _, err := s.ops.Start(ctx, "git-publish", idempotencyKey(ctx))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	id := r.Id
	branches := r.Branches // multi-select; falls back to single Branch, then agent HEAD
	if len(branches) == 0 && r.Branch != "" {
		branches = []string{r.Branch}
	}
	act := principalFromContext(ctx).userRole // capture before going async (background ctx has no principal)
	s.ops.Run(op.ID, func() (string, error) { return id, s.doPublish(context.Background(), id, branches, act) })
	return opProto(op), nil
}
