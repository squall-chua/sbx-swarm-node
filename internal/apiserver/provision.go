package apiserver

import (
	"context"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
)

// InternalService handles node->node RPCs (provision admission). Authorized by
// node identity (ADR-0011); never exposed over REST.
type InternalService struct {
	sbxv1.UnimplementedInternalServiceServer
	mgr *sandbox.Manager
}

// NewInternalService builds the internal service.
func NewInternalService(mgr *sandbox.Manager) *InternalService { return &InternalService{mgr: mgr} }

// Provision performs target-authoritative admission against real local capacity,
// then creates. A capacity miss returns accepted=false (the coordinator's NACK).
func (s *InternalService) Provision(ctx context.Context, r *sbxv1.ProvisionRequest) (*sbxv1.ProvisionReply, error) {
	in := r.Spec
	if in == nil {
		in = &sbxv1.CreateSandboxRequest{}
	}
	// Defensive: re-apply the built-in floor at the node->node trust boundary so a
	// peer that sends an unsized spec cannot bypass capacity accounting (ADR-0011).
	spec := toSpec(effectiveSpec(in, sandbox.Resources{}))
	rec, err := s.mgr.AdmitAndCreate(ctx, spec)
	if err == sandbox.ErrNoCapacity {
		return &sbxv1.ProvisionReply{Accepted: false, Reason: "no capacity"}, nil
	}
	if err != nil {
		return nil, err
	}
	return &sbxv1.ProvisionReply{Accepted: true, SandboxId: rec.ID}, nil
}
