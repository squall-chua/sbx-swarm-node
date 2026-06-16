# sbx-swarm-node M3 — Network Policy + Secrets Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development or superpowers:executing-plans. Checkbox steps.
>
> **Forward-looking:** depends on M1 (`Backend`, `Manager`, `apiserver`, `store`, auth). Swarm-wide fan-out of a default policy is **M4**; M3 is per-node-daemon.

**Goal:** Manage egress policy (structured `policy.List`, Allow/Deny/SetDefault/Profiles) and proxy-injected secrets (SetCustom/List/Remove) per node, with the cross-cutting **sensitive-data rule** (spec §11): secret + `env` values are write-only — never logged, never persisted, never gossiped, masked in output, TLS only.

**Architecture:** Extend `sandbox.Backend` with policy/secret methods mapping to `policy.*`/`secret.*`. A `PolicyService` (gRPC + gateway) exposes them; secret responses are always masked. Every mutating policy/secret call writes an `audit` record (durable; never contains secret values).

**Tech Stack:** Go 1.23, M1 stack.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/sandbox/backend.go` | add `PolicyRule`, `Secrets`, policy/secret methods |
| `internal/sandbox/fake.go` | fake policy/secret store |
| `internal/sandbox/sdkbackend.go` | map to `policy.*` / `secret.*` |
| `internal/audit/audit.go` | append-only audit writer over `store` |
| `proto/sbxswarm/v1/policy.proto` | `PolicyService` |
| `internal/apiserver/policyservice.go` | handlers (mask secrets, write audit) |
| `internal/node/node.go` | register `PolicyService` |

---

## Task 1: Backend policy + secret methods

**Files:** `internal/sandbox/backend.go`, `fake.go`, test `internal/sandbox/policy_fake_test.go`

- [x] **Step 1: Failing test**

```go
package sandbox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFake_PolicyAndSecrets(t *testing.T) {
	f := NewFake()
	ctx := context.Background()

	require.NoError(t, f.PolicyDeny(ctx, "", "evil.example"))
	rules, err := f.PolicyList(ctx, "")
	require.NoError(t, err)
	require.Len(t, rules, 1)
	require.Equal(t, "deny", rules[0].Decision)

	require.NoError(t, f.SecretSet(ctx, "s1", CustomSecret{Host: "api.x", Env: "TOKEN", Value: "shh"}))
	secs, err := f.SecretList(ctx, "s1")
	require.NoError(t, err)
	require.Len(t, secs.Custom, 1)
	require.Equal(t, "api.x", secs.Custom[0].Host)
	require.Empty(t, secs.Custom[0].Value) // backend never returns values
}
```

- [x] **Step 2: Run → FAIL**: `go test ./internal/sandbox/ -run TestFake_PolicyAndSecrets -v`

- [x] **Step 3: Add to `backend.go`**

```go
// PolicyRule mirrors a structured row from policy.List (SDK v0.1.2).
type PolicyRule struct {
	Provenance string `json:"provenance"`
	AppliesTo  string `json:"applies_to"`
	Rule       string `json:"rule"`
	Type       string `json:"type"`
	Decision   string `json:"decision"` // allow|deny
	Resources  string `json:"resources"`
}

// CustomSecret is a proxy-injected credential. Value is write-only and never
// returned by reads.
type CustomSecret struct {
	Host  string `json:"host"`
	Env   string `json:"env"`
	Value string `json:"value,omitempty"`
}

// StoredSecret is a non-custom secret entry (name only).
type StoredSecret struct {
	Name string `json:"name"`
}

// Secrets is the structured secret inventory (values always masked).
type Secrets struct {
	Stored []StoredSecret `json:"stored"`
	Custom []CustomSecret `json:"custom"`
}
```

Add to `Backend`:

```go
	PolicyAllow(ctx context.Context, scope, host string) error
	PolicyDeny(ctx context.Context, scope, host string) error
	PolicySetDefault(ctx context.Context, profile string) error
	PolicyRemoveRule(ctx context.Context, scope string) error
	PolicyReset(ctx context.Context) error
	PolicyList(ctx context.Context, scope string) ([]PolicyRule, error)
	PolicyProfiles(ctx context.Context) ([]string, error)
	SecretSet(ctx context.Context, scope string, s CustomSecret) error
	SecretList(ctx context.Context, scope string) (Secrets, error) // values masked
	SecretRemove(ctx context.Context, scope, host string) error
```

- [x] **Step 4: Add to `fake.go`** (in-memory; `SecretList` returns entries with empty `Value`)

```go
// add to Fake struct: rules []PolicyRule; secrets map[string][]CustomSecret

func (f *Fake) PolicyAllow(_ context.Context, _, host string) error {
	f.mu.Lock(); defer f.mu.Unlock()
	f.rules = append(f.rules, PolicyRule{Rule: host, Decision: "allow"}); return nil
}
func (f *Fake) PolicyDeny(_ context.Context, _, host string) error {
	f.mu.Lock(); defer f.mu.Unlock()
	f.rules = append(f.rules, PolicyRule{Rule: host, Decision: "deny"}); return nil
}
func (f *Fake) PolicySetDefault(context.Context, string) error { return nil }
func (f *Fake) PolicyRemoveRule(context.Context, string) error { return nil }
func (f *Fake) PolicyReset(context.Context) error             { f.mu.Lock(); f.rules = nil; f.mu.Unlock(); return nil }
func (f *Fake) PolicyList(_ context.Context, _ string) ([]PolicyRule, error) {
	f.mu.Lock(); defer f.mu.Unlock(); return append([]PolicyRule(nil), f.rules...), nil
}
func (f *Fake) PolicyProfiles(context.Context) ([]string, error) { return []string{"allow-all", "balanced", "deny-all"}, nil }

func (f *Fake) SecretSet(_ context.Context, scope string, s CustomSecret) error {
	f.mu.Lock(); defer f.mu.Unlock()
	if f.secrets == nil { f.secrets = map[string][]CustomSecret{} }
	f.secrets[scope] = append(f.secrets[scope], s); return nil
}
func (f *Fake) SecretList(_ context.Context, scope string) (Secrets, error) {
	f.mu.Lock(); defer f.mu.Unlock()
	var out Secrets
	for _, s := range f.secrets[scope] {
		out.Custom = append(out.Custom, CustomSecret{Host: s.Host, Env: s.Env}) // value masked
	}
	return out, nil
}
func (f *Fake) SecretRemove(_ context.Context, scope, host string) error {
	f.mu.Lock(); defer f.mu.Unlock()
	kept := f.secrets[scope][:0]
	for _, s := range f.secrets[scope] { if s.Host != host { kept = append(kept, s) } }
	f.secrets[scope] = kept; return nil
}
```

- [x] **Step 5: Run → PASS, commit**

```bash
go test ./internal/sandbox/ -v
git add internal/sandbox/ && git commit -m "feat(sandbox): Backend policy/secret methods + fake (masked secrets)"
```

> SDK adapter: map to `policy.Allow/Deny/SetDefault/RemoveRule/Reset/List/Profiles` and `secret.SetCustom/List/Remove`. For `PolicyList` handle `client.ErrUnexpectedFormat` by falling back to `ListRaw` and returning a single synthetic rule with `Type:"raw"` + the text (and log a warning). `SecretList` maps `*secret.Secrets` to `Secrets` with values stripped. Keep `var _ Backend` green.

---

## Task 2: Audit log

**Files:** `internal/audit/audit.go`, test `internal/audit/audit_test.go`

- [x] **Step 1: Failing test**

```go
package audit

import (
	"path/filepath"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
)

func TestAudit_AppendAndList(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	a := New(st, func() int64 { return 1 })
	require.NoError(t, a.Record(Entry{Actor: "admin", Action: "policy.deny", Target: "evil.example", Outcome: "ok"}))

	all, err := a.List()
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Equal(t, "policy.deny", all[0].Action)
}
```

- [x] **Step 2: Run → FAIL**: `go test ./internal/audit/ -v`

- [x] **Step 3: Implement `audit.go`**

```go
// Package audit is the durable, append-only record of credentialed/sensitive
// actions. It never stores secret values (spec §11/§15).
package audit

import (
	"encoding/binary"
	"encoding/json"
	"fmt"

	"github.com/squall-chua/sbx-swarm-node/internal/store"
)

const bucket = "audit"

// Entry is one audited action.
type Entry struct {
	Seq     int64  `json:"seq"`
	Actor   string `json:"actor"`   // role or key id, never the secret
	Action  string `json:"action"`  // e.g. policy.deny, secret.set
	Target  string `json:"target"`  // host/scope, never a value
	Outcome string `json:"outcome"` // ok|error
	TSUnix  int64  `json:"ts_unix"`
}

// Log appends and lists audit entries.
type Log struct {
	store *store.Store
	now   func() int64
}

// New builds an audit log. now returns a unix timestamp (injected for tests).
func New(st *store.Store, now func() int64) *Log { return &Log{store: st, now: now} }

// Record appends an entry with a monotonic seq (the bbolt key, big-endian).
func (l *Log) Record(e Entry) error {
	e.TSUnix = l.now()
	// next seq = current count + 1 (simple; audit is append-only)
	cur, err := l.List()
	if err != nil {
		return err
	}
	e.Seq = int64(len(cur)) + 1
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, uint64(e.Seq))
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return l.store.Put(bucket, string(key), raw)
}

// List returns all entries in seq order.
func (l *Log) List() ([]Entry, error) {
	var out []Entry
	err := l.store.ForEach(bucket, func(_, v []byte) error {
		var e Entry
		if err := json.Unmarshal(v, &e); err != nil {
			return fmt.Errorf("decode audit entry: %w", err)
		}
		out = append(out, e)
		return nil
	})
	return out, err
}
```

- [x] **Step 4: Run → PASS, commit**

```bash
go test ./internal/audit/ -v
git add internal/audit/ && git commit -m "feat(audit): durable append-only audit log"
```

---

## Task 3: PolicyService (proto + handlers, masking + audit) + wiring

**Files:** `proto/sbxswarm/v1/policy.proto`, `internal/apiserver/policyservice.go`, `internal/node/node.go`

- [x] **Step 1: Write `policy.proto` + regenerate**

```proto
syntax = "proto3";
package sbxswarm.v1;
import "google/api/annotations.proto";
option go_package = "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1;sbxswarmv1";

service PolicyService {
  rpc ListPolicy(ScopeRequest) returns (ListPolicyResponse) {
    option (google.api.http) = {get: "/v1/sandboxes/{scope}/policy"};
  }
  rpc SetPolicy(SetPolicyRequest) returns (Empty) {
    option (google.api.http) = {put: "/v1/sandboxes/{scope}/policy" body: "*"};
  }
  rpc ListSecrets(ScopeRequest) returns (ListSecretsResponse) {
    option (google.api.http) = {get: "/v1/sandboxes/{scope}/secrets"};
  }
  rpc SetSecret(SetSecretRequest) returns (Empty) {
    option (google.api.http) = {put: "/v1/sandboxes/{scope}/secrets" body: "*"};
  }
  rpc DeleteSecret(DeleteSecretRequest) returns (Empty) {
    option (google.api.http) = {delete: "/v1/sandboxes/{scope}/secrets/{host}"};
  }
}

message Empty {}
message ScopeRequest { string scope = 1; } // "" => node-global, sandbox id => per-sandbox

message PolicyRuleMsg { string provenance = 1; string applies_to = 2; string rule = 3; string type = 4; string decision = 5; string resources = 6; }
message ListPolicyResponse { repeated PolicyRuleMsg rules = 1; }
message SetPolicyRequest { string scope = 1; string decision = 2; string host = 3; } // decision: allow|deny

message SecretMsg { string host = 1; string env = 2; } // never a value
message ListSecretsResponse { repeated SecretMsg custom = 1; repeated string stored = 2; }
message SetSecretRequest { string scope = 1; string host = 2; string env = 3; string value = 4; } // value write-only
message DeleteSecretRequest { string scope = 1; string host = 2; }
```

Run: `buf generate && go build ./...`

- [x] **Step 2: Implement handlers (TDD: test SetSecret then ListSecrets returns no value; SetPolicy writes audit)**

```go
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

// PolicyService exposes per-scope policy + secret management. scope "" = node
// global; otherwise a sandbox id, resolved to its backend name.
type PolicyService struct {
	sbxv1.UnimplementedPolicyServiceServer
	mgr   *sandbox.Manager
	audit *audit.Log
}

// NewPolicyService builds the service.
func NewPolicyService(mgr *sandbox.Manager, a *audit.Log) *PolicyService { return &PolicyService{mgr: mgr, audit: a} }

func (s *PolicyService) scopeName(ctx context.Context, scope string) (string, error) {
	if scope == "" {
		return "", nil // global
	}
	return s.mgr.Resolve(ctx, scope)
}

func actor(ctx context.Context) string { r, _ := auth.RoleFromContext(ctx); return r }

func (s *PolicyService) SetPolicy(ctx context.Context, r *sbxv1.SetPolicyRequest) (*sbxv1.Empty, error) {
	name, err := s.scopeName(ctx, r.Scope)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	switch r.Decision {
	case "allow":
		err = s.mgr.Backend().PolicyAllow(ctx, name, r.Host)
	case "deny":
		err = s.mgr.Backend().PolicyDeny(ctx, name, r.Host)
	default:
		return nil, status.Error(codes.InvalidArgument, "decision must be allow|deny")
	}
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	_ = s.audit.Record(audit.Entry{Actor: actor(ctx), Action: "policy." + r.Decision, Target: r.Host, Outcome: outcome})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &sbxv1.Empty{}, nil
}

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
		out.Rules = append(out.Rules, &sbxv1.PolicyRuleMsg{Provenance: rr.Provenance, AppliesTo: rr.AppliesTo, Rule: rr.Rule, Type: rr.Type, Decision: rr.Decision, Resources: rr.Resources})
	}
	return out, nil
}

func (s *PolicyService) SetSecret(ctx context.Context, r *sbxv1.SetSecretRequest) (*sbxv1.Empty, error) {
	name, err := s.scopeName(ctx, r.Scope)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	serr := s.mgr.Backend().SecretSet(ctx, name, sandbox.CustomSecret{Host: r.Host, Env: r.Env, Value: r.Value})
	outcome := "ok"
	if serr != nil {
		outcome = "error"
	}
	// audit records host/env only — NEVER the value (spec §11).
	_ = s.audit.Record(audit.Entry{Actor: actor(ctx), Action: "secret.set", Target: r.Host, Outcome: outcome})
	if serr != nil {
		return nil, status.Error(codes.Internal, serr.Error())
	}
	return &sbxv1.Empty{}, nil
}

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
		out.Custom = append(out.Custom, &sbxv1.SecretMsg{Host: c.Host, Env: c.Env}) // no value, ever
	}
	for _, st := range secs.Stored {
		out.Stored = append(out.Stored, st.Name)
	}
	return out, nil
}

func (s *PolicyService) DeleteSecret(ctx context.Context, r *sbxv1.DeleteSecretRequest) (*sbxv1.Empty, error) {
	name, err := s.scopeName(ctx, r.Scope)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	derr := s.mgr.Backend().SecretRemove(ctx, name, r.Host)
	_ = s.audit.Record(audit.Entry{Actor: actor(ctx), Action: "secret.remove", Target: r.Host, Outcome: outcomeOf(derr)})
	if derr != nil {
		return nil, status.Error(codes.Internal, derr.Error())
	}
	return &sbxv1.Empty{}, nil
}

func outcomeOf(err error) string { if err != nil { return "error" }; return "ok" }
```

Write a test in `policyservice_test.go` constructing a `PolicyService` over a fake-backed manager + an audit log, asserting: `SetSecret` then `ListSecrets` returns the host but empty value; `SetPolicy("deny")` then `audit.List()` contains a `policy.deny` entry.

- [x] **Step 3: Register in `apiserver.Build` + `node.New`**

Add `Policy *PolicyService` to `apiserver.Options`; register on `grpcSrv` + gateway when set. In `node.New`, build `audit.New(st, func() int64 { return time.Now().Unix() })` and `apiserver.NewPolicyService(mgr, auditLog)`; pass via `Options`.

- [x] **Step 4: Run all tests + manual**

Run: `go test ./...`
Manual: `PUT /v1/sandboxes//policy {"decision":"deny","host":"evil.example"}` (global scope), then `GET /v1/sandboxes//policy` shows the rule; `PUT .../secrets` then `GET .../secrets` shows host/env but no value.

- [x] **Step 5: Commit**

```bash
git add proto/ internal/gen/ internal/apiserver/policyservice.go internal/apiserver/policyservice_test.go internal/node/
git commit -m "feat(policy): per-node policy + masked secrets API with audit"
```

---

## Self-Review

**Spec coverage (M3):** structured policy management (Allow/Deny/SetDefault/List→rules/Profiles, `ErrUnexpectedFormat`→`ListRaw` fallback) → Tasks 1,3 ✓; secret API (Set/List/Remove) **values write-only & masked** → Tasks 1,3 ✓; env-at-provision already carried in `CreateSpec.Env` (M1c) ✓; sensitive-data rule (no value logged/persisted/returned) → Tasks 1,2,3 ✓; audit log → Task 2 ✓. **Deferred:** swarm-wide `SetDefault` fan-out (M4); `Profiles`/`Reset`/`RemoveRule` RPCs (additive — backend methods exist, add thin handlers as needed).

**Placeholder scan:** No TBD/TODO. SDK adapter passthroughs guarded by `var _ Backend`. The "never log/persist value" rule is enforced concretely (audit records `Target` host only; `ListSecrets` strips `Value`).

**Type consistency:** `sandbox.PolicyRule`/`CustomSecret`/`Secrets`; backend methods match fake + handlers; `audit.New(*store.Store, func() int64).{Record,List}`; `apiserver.NewPolicyService(*sandbox.Manager, *audit.Log)`; proto `PolicyRuleMsg`/`SecretMsg` (no value field on read) match handlers.
