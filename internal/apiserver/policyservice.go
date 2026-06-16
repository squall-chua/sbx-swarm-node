package apiserver

import (
	"context"

	"github.com/squall-chua/sbx-swarm-node/internal/audit"
	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/auth"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// PolicyService exposes per-scope policy and secret management. scope "" is
// node-global; a non-empty scope is a sandbox swarm ID resolved to a backend name.
type PolicyService struct {
	sbxv1.UnimplementedPolicyServiceServer
	mgr   *sandbox.Manager
	audit *audit.Log
}

// NewPolicyService builds the service.
func NewPolicyService(mgr *sandbox.Manager, a *audit.Log) *PolicyService {
	return &PolicyService{mgr: mgr, audit: a}
}

// scopeName resolves a swarm scope to a backend name. "" is returned as-is
// (node-global means no per-sandbox scoping at the SDK layer).
func (s *PolicyService) scopeName(ctx context.Context, scope string) (string, error) {
	if scope == "" {
		return "", nil
	}
	return s.mgr.Resolve(ctx, scope)
}

// actor returns the authenticated role from context, or "" if unauthenticated.
func actor(ctx context.Context) string {
	r, _ := auth.RoleFromContext(ctx)
	return r
}

func outcomeOf(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

// SetPolicy adds an allow or deny network rule for a host within a scope.
func (s *PolicyService) SetPolicy(ctx context.Context, r *sbxv1.SetPolicyRequest) (*sbxv1.Empty, error) {
	name, err := s.scopeName(ctx, r.Scope)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	var opErr error
	switch r.Decision {
	case "allow":
		opErr = s.mgr.Backend().PolicyAllow(ctx, name, r.Host)
	case "deny":
		opErr = s.mgr.Backend().PolicyDeny(ctx, name, r.Host)
	default:
		return nil, status.Error(codes.InvalidArgument, "decision must be allow|deny")
	}
	// Audit: records actor/action/host and outcome. Never records a value (spec §11).
	_ = s.audit.Record(audit.Entry{
		Actor:   actor(ctx),
		Action:  "policy." + r.Decision,
		Target:  r.Host, // host name only
		Outcome: outcomeOf(opErr),
	})
	if opErr != nil {
		return nil, status.Error(codes.Internal, opErr.Error())
	}
	return &sbxv1.Empty{}, nil
}

// ListPolicy returns the current policy rules for a scope.
func (s *PolicyService) ListPolicy(ctx context.Context, r *sbxv1.ScopeRequest) (*sbxv1.ListPolicyResponse, error) {
	name, err := s.scopeName(ctx, r.Scope)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	rules, err := s.mgr.Backend().PolicyList(ctx, name)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &sbxv1.ListPolicyResponse{}
	for _, rr := range rules {
		out.Rules = append(out.Rules, &sbxv1.PolicyRuleMsg{
			Provenance: rr.Provenance,
			AppliesTo:  rr.AppliesTo,
			Rule:       rr.Rule,
			Type:       rr.Type,
			Decision:   rr.Decision,
			Resources:  rr.Resources,
		})
	}
	return out, nil
}

// SetSecret stores a custom proxy-injected secret. The value is passed through
// to the backend only and is never stored, logged, or returned (spec §11).
func (s *PolicyService) SetSecret(ctx context.Context, r *sbxv1.SetSecretRequest) (*sbxv1.Empty, error) {
	name, err := s.scopeName(ctx, r.Scope)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	serr := s.mgr.Backend().SecretSet(ctx, name, sandbox.CustomSecret{
		Host:  r.Host,
		Env:   r.Env,
		Value: r.Value, // consumed by backend only; not stored or logged here
	})
	// Audit: Target = host only. Value is intentionally absent (spec §11).
	_ = s.audit.Record(audit.Entry{
		Actor:   actor(ctx),
		Action:  "secret.set",
		Target:  r.Host, // host/scope identifier only, never the value
		Outcome: outcomeOf(serr),
	})
	if serr != nil {
		return nil, status.Error(codes.Internal, serr.Error())
	}
	return &sbxv1.Empty{}, nil
}

// ListSecrets returns the secret inventory. Values are never included in any
// response field (spec §11).
func (s *PolicyService) ListSecrets(ctx context.Context, r *sbxv1.ScopeRequest) (*sbxv1.ListSecretsResponse, error) {
	name, err := s.scopeName(ctx, r.Scope)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	secs, err := s.mgr.Backend().SecretList(ctx, name)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &sbxv1.ListSecretsResponse{}
	for _, c := range secs.Custom {
		// Value is intentionally omitted from SecretMsg (no value field exists on SecretMsg).
		out.Custom = append(out.Custom, &sbxv1.SecretMsg{Host: c.Host, Env: c.Env})
	}
	for _, st := range secs.Stored {
		out.Stored = append(out.Stored, st.Name)
	}
	return out, nil
}

// DeleteSecret removes a custom secret by host within a scope.
func (s *PolicyService) DeleteSecret(ctx context.Context, r *sbxv1.DeleteSecretRequest) (*sbxv1.Empty, error) {
	name, err := s.scopeName(ctx, r.Scope)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	derr := s.mgr.Backend().SecretRemove(ctx, name, r.Host)
	_ = s.audit.Record(audit.Entry{
		Actor:   actor(ctx),
		Action:  "secret.remove",
		Target:  r.Host,
		Outcome: outcomeOf(derr),
	})
	if derr != nil {
		return nil, status.Error(codes.Internal, derr.Error())
	}
	return &sbxv1.Empty{}, nil
}
