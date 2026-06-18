package apiserver

import (
	"context"
	"time"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/audit"
	"github.com/squall-chua/sbx-swarm-node/internal/events"
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
}

// SetGit wires git-backed workspaces (by name) for the publish path.
func (s *SandboxService) SetGit(ws map[string]*git.Workspace) { s.gitWS = ws }

// SetAudit wires the audit log for git operations.
func (s *SandboxService) SetAudit(a *audit.Log) { s.audit = a }

// SetEvents wires the event publisher for publish success/failure signals.
func (s *SandboxService) SetEvents(p events.Publisher) { s.events = p }

// WithPlacement wires placement (coordinator) + sizing defaults.
func (s *SandboxService) WithPlacement(place PlaceFunc, defaultStrategy string, defaults sandbox.Resources) {
	s.place = place
	s.defaultStrategy = defaultStrategy
	s.defaultResources = defaults
}

const (
	floorCPUCores    int32 = 1
	floorMemoryBytes int64 = 512 << 20 // 512 MiB
	floorDiskGB            = 1.0
)

// effectiveSpec returns a copy of r with each unset resource filled from the
// configured default, else the built-in floor (no untracked sandboxes).
// ponytail: floor approximates the daemon's hidden default; source it from the
// daemon once the SDK exposes it.
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
	return sandbox.CreateSpec{
		Agent: r.Agent, Template: r.Template, CPUs: int(r.Cpus),
		MemoryBytes: r.MemoryBytes, DiskGB: r.DiskGb, Clone: r.Clone, Branch: r.Branch, Workspaces: ws, Env: r.Env,
	}
}

func toProto(rec *sandbox.Record) *sbxv1.Sandbox {
	ports := make([]*sbxv1.Port, 0, len(rec.Ports))
	for _, p := range rec.Ports {
		ports = append(ports, &sbxv1.Port{ContainerPort: int32(p.ContainerPort), HostPort: int32(p.HostPort)})
	}
	var lastPub string
	if !rec.LastPublish.IsZero() {
		lastPub = rec.LastPublish.UTC().Format(time.RFC3339)
	}
	return &sbxv1.Sandbox{
		Id: rec.ID, OwnerNode: rec.OwnerNode, Status: rec.Status, Ports: ports, Labels: rec.Labels,
		Branch: rec.Spec.Branch, LastPublish: lastPub,
	}
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
	op, existed, err := s.ops.Start(ctx, "provision", idempotencyKey(ctx))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
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

func (s *SandboxService) StopSandbox(ctx context.Context, r *sbxv1.IdRequest) (*sbxv1.Sandbox, error) {
	if err := s.mgr.Stop(ctx, r.Id); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return s.GetSandbox(ctx, &sbxv1.GetSandboxRequest{Id: r.Id})
}

func (s *SandboxService) Exec(ctx context.Context, r *sbxv1.ExecRequest) (*sbxv1.ExecResponse, error) {
	name, err := s.mgr.Resolve(ctx, r.Id)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	res, err := s.mgr.Backend().Exec(ctx, name, r.Cmd, sandbox.ExecOpts{Workdir: r.Workdir, Env: r.Env})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &sbxv1.ExecResponse{ExitCode: int32(res.ExitCode), Stdout: res.Stdout, Stderr: res.Stderr}, nil
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
	s.ops.Run(op.ID, func() (string, error) {
		did, derr := s.mgr.Backend().ExecDetached(context.Background(), name, cmd, opts)
		if derr != nil {
			return "", derr
		}
		for { // poll to completion (M1c: simple loop; M1d streams progress)
			st, perr := s.mgr.Backend().PollDetached(context.Background(), name, did)
			if perr != nil {
				return "", perr
			}
			if st.Done {
				if st.ExitCode != 0 {
					return sbID, status.Errorf(codes.Internal, "agent run exited %d", st.ExitCode)
				}
				return sbID, nil
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

// doPublish runs the publish pipeline for a sandbox's git-backed workspace and
// audits/emits the outcome.
func (s *SandboxService) doPublish(ctx context.Context, sandboxID, reqBranch string) error {
	rec, err := s.mgr.Get(ctx, sandboxID)
	if err == sandbox.ErrNotFound {
		return status.Error(codes.NotFound, "sandbox not found")
	}
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	if len(rec.Spec.Workspaces) != 1 {
		return status.Error(codes.FailedPrecondition, "sandbox is not clone-mode")
	}
	ws := s.gitWS[rec.Spec.Workspaces[0].Name]
	if ws == nil {
		return status.Error(codes.FailedPrecondition, "workspace is not git-backed")
	}
	if !ws.AllowPush() {
		return status.Error(codes.FailedPrecondition, "workspace does not allow push")
	}
	branch := reqBranch
	if branch == "" {
		branch = rec.Spec.Branch
	}
	if branch == "" {
		return status.Error(codes.FailedPrecondition, "no branch to publish")
	}
	if rec.Status != "running" {
		return status.Error(codes.FailedPrecondition, "sandbox not running; cannot reach sandbox-"+rec.BackendName)
	}

	perr := ws.Publish(ctx, branch, "sandbox-"+rec.BackendName)
	s.auditPublish(ws.Name(), branch, perr)
	if perr != nil {
		s.emit("sandbox.publish_failed", sandboxID, map[string]string{"branch": branch})
		return status.Errorf(codes.Internal, "publish: %v", perr)
	}
	s.emit("sandbox.published", sandboxID, map[string]string{"branch": branch})
	_ = s.mgr.SetLastPublish(ctx, sandboxID, time.Now())
	return nil
}

func (s *SandboxService) auditPublish(workspace, branch string, err error) {
	if s.audit == nil {
		return
	}
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	_ = s.audit.Record(audit.Entry{Action: "git.publish", Target: workspace + "@" + branch, Outcome: outcome})
}

func (s *SandboxService) emit(eventType, sandboxID string, payload map[string]string) {
	if s.events != nil {
		s.events.Publish(eventType, sandboxID, payload)
	}
}

// PublishSandbox starts an async git-publish operation.
func (s *SandboxService) PublishSandbox(ctx context.Context, r *sbxv1.PublishSandboxRequest) (*sbxv1.Operation, error) {
	op, _, err := s.ops.Start(ctx, "git-publish", idempotencyKey(ctx))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	id, branch := r.Id, r.Branch
	s.ops.Run(op.ID, func() (string, error) { return id, s.doPublish(context.Background(), id, branch) })
	return opProto(op), nil
}
