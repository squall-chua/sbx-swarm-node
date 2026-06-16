package apiserver

import (
	"context"
	"time"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/ops"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// SandboxService implements sbxv1.SandboxServiceServer over the sandbox Manager.
type SandboxService struct {
	sbxv1.UnimplementedSandboxServiceServer
	mgr *sandbox.Manager
	ops *ops.Manager
	obs ObserveDeps
}

// NewSandboxService builds the service.
func NewSandboxService(mgr *sandbox.Manager, opsM *ops.Manager) *SandboxService {
	return &SandboxService{mgr: mgr, ops: opsM}
}

func toSpec(r *sbxv1.CreateSandboxRequest) sandbox.CreateSpec {
	ws := make([]sandbox.WorkspaceMount, 0, len(r.Workspaces))
	for _, w := range r.Workspaces {
		ws = append(ws, sandbox.WorkspaceMount{Name: w.Name, ReadOnly: w.ReadOnly})
	}
	return sandbox.CreateSpec{
		Agent: r.Agent, Template: r.Template, CPUs: int(r.Cpus),
		MemoryBytes: r.MemoryBytes, Clone: r.Clone, Workspaces: ws, Env: r.Env,
	}
}

func toProto(rec *sandbox.Record) *sbxv1.Sandbox {
	ports := make([]*sbxv1.Port, 0, len(rec.Ports))
	for _, p := range rec.Ports {
		ports = append(ports, &sbxv1.Port{ContainerPort: int32(p.ContainerPort), HostPort: int32(p.HostPort)})
	}
	return &sbxv1.Sandbox{Id: rec.ID, OwnerNode: rec.OwnerNode, Status: rec.Status, Ports: ports, Labels: rec.Labels}
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
	op, existed, err := s.ops.Start(ctx, "provision", idempotencyKey(ctx))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if existed {
		return opProto(op), nil
	}
	spec := toSpec(r)
	s.ops.Run(op.ID, func() (string, error) {
		rec, cerr := s.mgr.Create(context.Background(), spec)
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
