# Security Follow-on (Node Trust + Role-Gate) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the node safe for untrusted-network deployment: gate mutating RPCs to admin, authenticate the native-gRPC port, and authenticate/pin node↔node traffic with Ed25519 node keys (ADR-0004) — all on one branch.

**Architecture:** Route the in-process grpc-gateway through an in-memory loopback gRPC connection so a single interceptor chain (`authn → authz → forward`) covers REST and native gRPC. A gateway metadata annotator bridges the HTTP-authenticated role across the loopback as a signed `x-sbx-authz` token. Peers authenticate with audience-bound Ed25519 PerRPC tokens and pin each other's TLS leaf cert to the gossiped node pubkey. Session cookies become swarm-verifiable via a `cluster_secret`-derived signing key (ADR-0010).

**Tech Stack:** Go 1.25, gRPC + grpc-gateway v2.29, `crypto/ed25519`, `crypto/hkdf`, `google.golang.org/grpc/test/bufconn`, testify.

**Reconciliation notes (verified against current code):**
- Roles are exactly `"admin"` and `"read-only"` (`internal/config/config.go`).
- `auth.Signer.Mint(role string, expiry time.Time) string` / `Verify(token string) (string, error)`; HMAC over a key (`internal/auth/session.go`).
- `identity.Identity{PublicKey ed25519.PublicKey; PrivateKey ed25519.PrivateKey; NodeID string}`, `identity.DeriveNodeID(pub) string` (`internal/identity/identity.go`).
- `routing.Table` entry is `{addr string; cordoned bool}`; `Upsert(nodeID, addr string, cordoned bool)`, `Addr`, `IsLocal`, `Owner`, `Peers` (`internal/routing/table.go`). `Upsert` call sites: `cluster.go:149,262,294,304` (262 is bulk → has `PubKey`; 294/304 are meta → `PubKey` empty).
- `peer.Pool.Conn(addr string) (*grpc.ClientConn, error)`; options `WithCreds`, `WithContextDialer` (`internal/peer/client.go`). Callers: `forward.go:43`, `sse.go:129`.
- `membership.NodeState.PubKey []byte` exists on the bulk channel but is **unset** in node wiring; `NodeState.Addr/Cordoned/...` (`internal/membership/state.go`). Local `NodeState` built at `node.go:148`.
- gRPC full-method names (authoritative inventory): SandboxService `{CreateSandbox,GetSandbox,ListSandboxes,DeleteSandbox,StartSandbox,StopSandbox,Exec,AgentRun,PublishPort,ListPorts,GetStats,ListBlocked}`; NodeService `{GetNodeInfo,Cordon,Uncordon,Drain}`; PolicyService `{ListPolicy,SetPolicy,ListSecrets,SetSecret,DeleteSecret}`; EventService `{WatchEvents}` (stream).
- Existing test idiom: `apiserver.Build(Options)` served over real TLS on `127.0.0.1:0`; REST via `InsecureSkipVerify` http client; gRPC via `grpc.NewClient(addr, WithTransportCredentials(NewTLS(InsecureSkipVerify)))` (`internal/apiserver/server_test.go`). `keyMap` is a test `KeyStore`; `testSigner()` = `auth.NewSigner([]byte("test-secret"))`.
- `apiserver.Build` builds `grpcSrv` with a single `grpc.UnaryInterceptor(Forward...)`; gateway registered in-process via `RegisterXHandlerServer` (`internal/apiserver/server.go:40-124`). Generated loopback variants `RegisterXHandler(ctx, mux, *grpc.ClientConn)` exist.

**Conventions:** Run `go build ./... && go vet ./...` before each commit. Run `-race` on concurrent packages (`peer`, `apiserver`, `membership`, `routing`). Never log cert bytes, node keys, or tokens (secrets invariant). Each task ends green.

---

### Task 1: `nodekey` package — audience-bound Ed25519 PerRPC token

**Files:**
- Create: `internal/nodekey/nodekey.go`
- Test: `internal/nodekey/nodekey_test.go`

- [x] **Step 1: Write the failing test**

```go
package nodekey

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/identity"
	"github.com/stretchr/testify/require"
)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return pub, priv, identity.DeriveNodeID(pub)
}

func TestSignVerify_RoundTrip(t *testing.T) {
	pub, priv, callerID := newKey(t)
	now := time.Unix(1_700_000_000, 0)
	tok := Sign(priv, callerID, "target-node", now)

	resolve := func(id string) ([]byte, bool) {
		if id == callerID {
			return pub, true
		}
		return nil, false
	}
	got, err := Verify(tok, "target-node", resolve, now.Add(5*time.Second), 30*time.Second, nil)
	require.NoError(t, err)
	require.Equal(t, callerID, got)
}

func TestVerify_WrongAudienceRejected(t *testing.T) {
	pub, priv, callerID := newKey(t)
	now := time.Unix(1_700_000_000, 0)
	tok := Sign(priv, callerID, "node-B", now)
	resolve := func(string) ([]byte, bool) { return pub, true }
	_, err := Verify(tok, "node-C", resolve, now, 30*time.Second, nil)
	require.ErrorContains(t, err, "audience")
}

func TestVerify_StaleRejected(t *testing.T) {
	pub, priv, callerID := newKey(t)
	now := time.Unix(1_700_000_000, 0)
	tok := Sign(priv, callerID, "t", now)
	resolve := func(string) ([]byte, bool) { return pub, true }
	_, err := Verify(tok, "t", resolve, now.Add(40*time.Second), 30*time.Second, nil)
	require.ErrorContains(t, err, "stale")
}

func TestVerify_UnknownPeerRejected(t *testing.T) {
	_, priv, callerID := newKey(t)
	now := time.Unix(1_700_000_000, 0)
	tok := Sign(priv, callerID, "t", now)
	resolve := func(string) ([]byte, bool) { return nil, false }
	_, err := Verify(tok, "t", resolve, now, 30*time.Second, nil)
	require.ErrorContains(t, err, "unknown")
}

func TestVerify_ForgedSignatureRejected(t *testing.T) {
	pub, priv, callerID := newKey(t)
	otherPub, _, _ := newKey(t)
	now := time.Unix(1_700_000_000, 0)
	tok := Sign(priv, callerID, "t", now)
	resolve := func(string) ([]byte, bool) { return otherPub, true } // wrong pubkey
	_, err := Verify(tok, "t", resolve, now, 30*time.Second, nil)
	require.Error(t, err)
	_ = pub
}

func TestVerify_DenylistedRejected(t *testing.T) {
	pub, priv, callerID := newKey(t)
	now := time.Unix(1_700_000_000, 0)
	tok := Sign(priv, callerID, "t", now)
	resolve := func(string) ([]byte, bool) { return pub, true }
	denied := func(id string) bool { return id == callerID }
	_, err := Verify(tok, "t", resolve, now, 30*time.Second, denied)
	require.ErrorContains(t, err, "denied")
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/nodekey/ -run TestSignVerify -v`
Expected: FAIL (package/functions undefined).

- [x] **Step 3: Write minimal implementation**

```go
// Package nodekey signs and verifies audience-bound Ed25519 tokens that
// authenticate one node to another (ADR-0004). A token binds the caller node id,
// the intended target node id (audience), and a timestamp, so it cannot be
// replayed to a third node or outside a short freshness window.
package nodekey

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/identity"
)

// MetadataKey is the gRPC metadata key carrying the token.
const MetadataKey = "x-sbx-node-auth"

func payload(callerID, targetID string, unix int64) string {
	return callerID + "|" + targetID + "|" + strconv.FormatInt(unix, 10)
}

// Sign returns a token of the form "<caller>.<target>.<unix>.<base64(sig)>".
func Sign(priv ed25519.PrivateKey, callerID, targetID string, now time.Time) string {
	unix := now.Unix()
	sig := ed25519.Sign(priv, []byte(payload(callerID, targetID, unix)))
	return callerID + "." + targetID + "." + strconv.FormatInt(unix, 10) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
}

// Verify parses and validates a token. pubkeyFor resolves a caller node id to its
// gossiped pubkey (the TOFU pin); denied (nil ok) reports revoked node ids.
func Verify(
	token, expectedTarget string,
	pubkeyFor func(nodeID string) ([]byte, bool),
	now time.Time,
	skew time.Duration,
	denied func(nodeID string) bool,
) (callerID string, err error) {
	parts := strings.Split(token, ".")
	if len(parts) != 4 {
		return "", errors.New("nodekey: malformed token")
	}
	callerID, targetID, unixStr, sigB64 := parts[0], parts[1], parts[2], parts[3]
	if targetID != expectedTarget {
		return "", fmt.Errorf("nodekey: wrong audience %q", targetID)
	}
	if denied != nil && denied(callerID) {
		return "", fmt.Errorf("nodekey: node %s is denied", callerID)
	}
	pub, ok := pubkeyFor(callerID)
	if !ok {
		return "", fmt.Errorf("nodekey: unknown peer %s", callerID)
	}
	if len(pub) != ed25519.PublicKeySize || identity.DeriveNodeID(pub) != callerID {
		return "", errors.New("nodekey: pubkey does not match node id")
	}
	unix, perr := strconv.ParseInt(unixStr, 10, 64)
	if perr != nil {
		return "", fmt.Errorf("nodekey: bad timestamp: %w", perr)
	}
	if d := now.Sub(time.Unix(unix, 0)); d > skew || d < -skew {
		return "", errors.New("nodekey: stale token")
	}
	sig, derr := base64.RawURLEncoding.DecodeString(sigB64)
	if derr != nil {
		return "", fmt.Errorf("nodekey: bad signature encoding: %w", derr)
	}
	if !ed25519.Verify(pub, []byte(payload(callerID, targetID, unix)), sig) {
		return "", errors.New("nodekey: signature verification failed")
	}
	return callerID, nil
}
```

- [x] **Step 4: Run test to verify it passes**

Run: `go test ./internal/nodekey/ -v`
Expected: PASS (all cases).

- [x] **Step 5: Commit**

```bash
git add internal/nodekey/
git commit -m "feat(nodekey): audience-bound Ed25519 PerRPC token (ADR-0004)"
```

---

### Task 2: Swarm-wide session signing key (ADR-0010)

**Files:**
- Modify: `internal/auth/session.go` (add `DeriveSessionKey`)
- Test: `internal/auth/session_test.go` (append)

- [x] **Step 1: Write the failing test** (append to `internal/auth/session_test.go`)

```go
func TestDeriveSessionKey_SwarmWideVsStandalone(t *testing.T) {
	// Same cluster secret on two nodes -> identical key -> cross-node verify works.
	k1 := auth.DeriveSessionKey("cluster-secret-xyz", []byte("node-A-seed"))
	k2 := auth.DeriveSessionKey("cluster-secret-xyz", []byte("node-B-seed"))
	require.Equal(t, k1, k2)
	require.Len(t, k1, 32)

	tokA := auth.NewSigner(k1).Mint("admin", time.Now().Add(time.Hour))
	role, err := auth.NewSigner(k2).Verify(tokA) // node B verifies node A's token
	require.NoError(t, err)
	require.Equal(t, "admin", role)

	// Standalone (no cluster secret) falls back to the per-node seed.
	s1 := auth.DeriveSessionKey("", []byte("node-A-seed"))
	s2 := auth.DeriveSessionKey("", []byte("node-B-seed"))
	require.NotEqual(t, s1, s2)
}
```

Add imports `"time"` and `"github.com/squall-chua/sbx-swarm-node/internal/auth"` if the test file is `package auth_test`; if it is `package auth`, drop the `auth.` qualifiers and the import. (Check the existing header of `session_test.go` and match it.)

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/auth/ -run TestDeriveSessionKey -v`
Expected: FAIL (`DeriveSessionKey` undefined).

- [x] **Step 3: Write minimal implementation** (append to `internal/auth/session.go`, add imports `crypto/sha256`, `crypto/hkdf`)

```go
// DeriveSessionKey returns the HMAC key used to sign session/x-sbx-authz tokens.
// In a swarm it is derived from the cluster secret so a token minted by any node
// verifies on every node (ADR-0010); a standalone node (empty clusterSecret)
// uses its per-node seed.
func DeriveSessionKey(clusterSecret string, nodeSeed []byte) []byte {
	if clusterSecret == "" {
		return nodeSeed
	}
	key, err := hkdf.Key(sha256.New, []byte(clusterSecret), nil, "sbx-session-v1", 32)
	if err != nil {
		// HKDF only errors on absurd key lengths; 32 is always valid.
		panic("auth: hkdf derive session key: " + err.Error())
	}
	return key
}
```

- [x] **Step 4: Run test to verify it passes**

Run: `go test ./internal/auth/ -v`
Expected: PASS.

- [x] **Step 5: Commit**

```bash
git add internal/auth/session.go internal/auth/session_test.go
git commit -m "feat(auth): cluster-secret-derived swarm-wide session key (ADR-0010)"
```

---

### Task 3: Authorization — method classification + interceptors (drift-guarded)

**Files:**
- Create: `internal/apiserver/authz.go`
- Test: `internal/apiserver/authz_test.go`

- [x] **Step 1: Write the failing test**

```go
package apiserver

import (
	"context"
	"testing"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestAuthorize_MutationRequiresAdmin(t *testing.T) {
	// read-only user cannot mutate
	err := authorize("/sbxswarm.v1.SandboxService/CreateSandbox", principal{userRole: "read-only"})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	// admin user can mutate
	require.NoError(t, authorize("/sbxswarm.v1.SandboxService/CreateSandbox", principal{userRole: "admin"}))
	// node-only principal cannot mutate
	err = authorize("/sbxswarm.v1.PolicyService/SetSecret", principal{node: true})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}

func TestAuthorize_ReadsAndWatchAllowAnyAuthenticated(t *testing.T) {
	require.NoError(t, authorize("/sbxswarm.v1.SandboxService/ListSandboxes", principal{userRole: "read-only"}))
	require.NoError(t, authorize("/sbxswarm.v1.EventService/WatchEvents", principal{node: true}))
	// unauthenticated principal is rejected even for reads
	err := authorize("/sbxswarm.v1.SandboxService/ListSandboxes", principal{})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}

// Drift guard: every registered method must be classified.
func TestAuthz_AllMethodsClassified(t *testing.T) {
	descs := []grpc.ServiceDesc{
		sbxv1.SandboxService_ServiceDesc,
		sbxv1.NodeService_ServiceDesc,
		sbxv1.PolicyService_ServiceDesc,
		sbxv1.EventService_ServiceDesc,
	}
	for _, d := range descs {
		names := []string{}
		for _, m := range d.Methods {
			names = append(names, m.MethodName)
		}
		for _, s := range d.Streams {
			names = append(names, s.StreamName)
		}
		for _, n := range names {
			full := "/" + d.ServiceName + "/" + n
			require.True(t, classified(full), "method %s is not classified (add to mutating or reads)", full)
		}
	}
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/apiserver/ -run TestAuthz -v`
Expected: FAIL (`authorize`, `principal`, `classified` undefined).

- [x] **Step 3: Write minimal implementation**

```go
package apiserver

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// principal is the authenticated identity attached by the authn interceptor.
type principal struct {
	userRole string // "admin" | "read-only" | "" (none)
	node     bool   // authenticated as a swarm peer via node-key
}

func (p principal) authenticated() bool { return p.userRole != "" || p.node }

// mutatingMethods require an admin USER role. A node-only principal can never
// authorize a mutation by itself; forwarded mutations carry the user's admin
// credential alongside the node-key. Keep in sync with the proto — the drift
// guard test (TestAuthz_AllMethodsClassified) fails if a new method is unlisted.
var mutatingMethods = map[string]bool{
	"/sbxswarm.v1.SandboxService/CreateSandbox": true,
	"/sbxswarm.v1.SandboxService/DeleteSandbox": true,
	"/sbxswarm.v1.SandboxService/StartSandbox":  true,
	"/sbxswarm.v1.SandboxService/StopSandbox":   true,
	"/sbxswarm.v1.SandboxService/Exec":          true,
	"/sbxswarm.v1.SandboxService/AgentRun":      true,
	"/sbxswarm.v1.SandboxService/PublishPort":   true,
	"/sbxswarm.v1.PolicyService/SetPolicy":      true,
	"/sbxswarm.v1.PolicyService/SetSecret":      true,
	"/sbxswarm.v1.PolicyService/DeleteSecret":   true,
	"/sbxswarm.v1.NodeService/Cordon":           true,
	"/sbxswarm.v1.NodeService/Uncordon":         true,
	"/sbxswarm.v1.NodeService/Drain":            true,
}

// readMethods are explicitly read/internal (any authenticated principal).
var readMethods = map[string]bool{
	"/sbxswarm.v1.SandboxService/GetSandbox":    true,
	"/sbxswarm.v1.SandboxService/ListSandboxes": true,
	"/sbxswarm.v1.SandboxService/ListPorts":     true,
	"/sbxswarm.v1.SandboxService/GetStats":      true,
	"/sbxswarm.v1.SandboxService/ListBlocked":   true,
	"/sbxswarm.v1.NodeService/GetNodeInfo":      true,
	"/sbxswarm.v1.PolicyService/ListPolicy":     true,
	"/sbxswarm.v1.PolicyService/ListSecrets":    true,
	"/sbxswarm.v1.EventService/WatchEvents":     true,
}

func classified(fullMethod string) bool {
	return mutatingMethods[fullMethod] || readMethods[fullMethod]
}

// authorize enforces the 2-bucket policy. Unknown methods fail closed (treated as
// mutating: require admin) so an unclassified new RPC is never silently open.
func authorize(fullMethod string, p principal) error {
	if !p.authenticated() {
		return status.Error(codes.Unauthenticated, "authentication required")
	}
	if readMethods[fullMethod] {
		return nil
	}
	if p.userRole == "admin" {
		return nil
	}
	return status.Errorf(codes.PermissionDenied, "method %s requires admin", fullMethod)
}

// authzUnaryInterceptor enforces authorize() using the principal placed in ctx by
// the authn interceptor.
func authzUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := authorize(info.FullMethod, principalFromContext(ctx)); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// authzStreamInterceptor is the streaming counterpart.
func authzStreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := authorize(info.FullMethod, principalFromContext(ss.Context())); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}
```

Note: `principalFromContext` is defined in Task 4 (`authn.go`, same package). This task does not compile/run the interceptor functions in isolation — but `authorize`/`classified`/`principal` (used by the tests) are self-contained. To keep this task green before Task 4, add a temporary stub at the bottom of `authz.go` and delete it in Task 4 Step 3:

```go
// TEMP: replaced by authn.go in Task 4.
func principalFromContext(ctx context.Context) principal { return principal{} }
```

- [x] **Step 4: Run test to verify it passes**

Run: `go test ./internal/apiserver/ -run TestAuthz -v && go test ./internal/apiserver/ -run TestAuthorize -v`
Expected: PASS. Also `go build ./...` succeeds.

- [x] **Step 5: Commit**

```bash
git add internal/apiserver/authz.go internal/apiserver/authz_test.go
git commit -m "feat(apiserver): 2-bucket gRPC authz interceptor + drift guard"
```

---

### Task 4: Authentication interceptor (unary + stream)

**Files:**
- Create: `internal/apiserver/authn.go`
- Modify: `internal/apiserver/authz.go` (remove the TEMP `principalFromContext` stub)
- Test: `internal/apiserver/authn_test.go`

- [x] **Step 1: Write the failing test**

```go
package apiserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/auth"
	"github.com/squall-chua/sbx-swarm-node/internal/identity"
	"github.com/squall-chua/sbx-swarm-node/internal/nodekey"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestAuthenticate_SignedAuthzToken(t *testing.T) {
	signer := auth.NewSigner([]byte("k"))
	a := newAuthenticator(authnDeps{
		Signer: signer, Keys: keyMap{"adm": "admin"}, LocalNodeID: "self",
	})
	tok := signer.Mint("read-only", time.Now().Add(time.Minute))
	md := metadata.Pairs("x-sbx-authz", tok)
	ctx := metadata.NewIncomingContext(context.Background(), md)
	p, err := a.authenticate(ctx)
	require.NoError(t, err)
	require.Equal(t, "read-only", p.userRole)
}

func TestAuthenticate_BearerAPIKey(t *testing.T) {
	a := newAuthenticator(authnDeps{Signer: auth.NewSigner([]byte("k")), Keys: keyMap{"adm": "admin"}, LocalNodeID: "self"})
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer adm"))
	p, err := a.authenticate(ctx)
	require.NoError(t, err)
	require.Equal(t, "admin", p.userRole)
}

func TestAuthenticate_NodeKey(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	callerID := identity.DeriveNodeID(pub)
	a := newAuthenticator(authnDeps{
		Signer: auth.NewSigner([]byte("k")), Keys: keyMap{}, LocalNodeID: "self",
		PubKeyFor: func(id string) ([]byte, bool) {
			if id == callerID {
				return pub, true
			}
			return nil, false
		},
	})
	tok := nodekey.Sign(priv, callerID, "self", time.Now())
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(nodekey.MetadataKey, tok))
	p, err := a.authenticate(ctx)
	require.NoError(t, err)
	require.True(t, p.node)
	require.Empty(t, p.userRole)
}

func TestAuthenticate_NoCredsRejected(t *testing.T) {
	a := newAuthenticator(authnDeps{Signer: auth.NewSigner([]byte("k")), Keys: keyMap{}, LocalNodeID: "self"})
	_, err := a.authenticate(context.Background())
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/apiserver/ -run TestAuthenticate -v`
Expected: FAIL (`newAuthenticator`, `authnDeps` undefined).

- [x] **Step 3: Write minimal implementation** (create `authn.go`; then delete the TEMP stub from `authz.go`)

```go
package apiserver

import (
	"context"
	"strings"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/auth"
	"github.com/squall-chua/sbx-swarm-node/internal/nodekey"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type principalCtxKey struct{}

func principalFromContext(ctx context.Context) principal {
	p, _ := ctx.Value(principalCtxKey{}).(principal)
	return p
}

// authnDeps are the authenticator's collaborators. PubKeyFor/Denied are nil in
// standalone mode (no peers).
type authnDeps struct {
	Signer      *auth.Signer
	Keys        auth.KeyStore
	LocalNodeID string
	PubKeyFor   func(nodeID string) ([]byte, bool) // gossiped peer pubkeys
	Denied      func(nodeID string) bool
	Skew        time.Duration // 0 -> default 30s
}

type authenticator struct{ d authnDeps }

func newAuthenticator(d authnDeps) *authenticator {
	if d.Skew == 0 {
		d.Skew = 30 * time.Second
	}
	return &authenticator{d: d}
}

// authenticate resolves a principal in strict order: x-sbx-authz (signed role),
// authorization (api key), node-key. Returns Unauthenticated if none verify.
func (a *authenticator) authenticate(ctx context.Context) (principal, error) {
	md, _ := metadata.FromIncomingContext(ctx)

	if v := first(md, "x-sbx-authz"); v != "" {
		if role, err := a.d.Signer.Verify(v); err == nil {
			return principal{userRole: role}, nil
		}
	}
	if v := first(md, "authorization"); v != "" {
		if role, ok := a.d.Keys.RoleForKey(strings.TrimPrefix(v, "Bearer ")); ok {
			return principal{userRole: role}, nil
		}
	}
	if v := first(md, nodekey.MetadataKey); v != "" && a.d.PubKeyFor != nil {
		if _, err := nodekey.Verify(v, a.d.LocalNodeID, a.d.PubKeyFor, time.Now(), a.d.Skew, a.d.Denied); err == nil {
			return principal{node: true}, nil
		}
	}
	return principal{}, status.Error(codes.Unauthenticated, "authentication required")
}

func first(md metadata.MD, key string) string {
	if v := md.Get(key); len(v) > 0 {
		return v[0]
	}
	return ""
}

func (a *authenticator) unaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		p, err := a.authenticate(ctx)
		if err != nil {
			return nil, err
		}
		return handler(context.WithValue(ctx, principalCtxKey{}, p), req)
	}
}

func (a *authenticator) streamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		p, err := a.authenticate(ss.Context())
		if err != nil {
			return err
		}
		return handler(srv, &ctxStream{ServerStream: ss, ctx: context.WithValue(ss.Context(), principalCtxKey{}, p)})
	}
}

// ctxStream overrides Context() so downstream interceptors/handlers see the principal.
type ctxStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *ctxStream) Context() context.Context { return s.ctx }
```

Then **delete** the TEMP `principalFromContext` stub from `authz.go` (Task 3 Step 3).

- [x] **Step 4: Run test to verify it passes**

Run: `go test ./internal/apiserver/ -run TestAuthenticate -v && go test ./internal/apiserver/ -run TestAuthz -v`
Expected: PASS. `go build ./...` succeeds.

- [x] **Step 5: Commit**

```bash
git add internal/apiserver/authn.go internal/apiserver/authz.go internal/apiserver/authn_test.go
git commit -m "feat(apiserver): gRPC authn interceptor (x-sbx-authz / bearer / node-key)"
```

---

### Task 5: Bind TLS leaf cert to the node key + pin verifier

**Files:**
- Modify: `internal/tlsutil/tlsutil.go` (add `GenerateForKey`, `LeafPublicKey`, `PinnedVerify`)
- Test: `internal/tlsutil/tlsutil_test.go` (append)

- [x] **Step 1: Write the failing test** (append)

```go
func TestGenerateForKey_LeafPubkeyMatchesNodeKey(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(crand.Reader)
	require.NoError(t, err)
	cert, err := tlsutil.GenerateForKey(priv)
	require.NoError(t, err)

	leafPub, err := tlsutil.LeafPublicKey(cert)
	require.NoError(t, err)
	require.True(t, ed25519.PublicKey(leafPub).Equal(pub))

	// PinnedVerify accepts the matching pubkey, rejects a different one.
	verify := tlsutil.PinnedVerify(pub)
	require.NoError(t, verify(cert.Certificate, nil))

	otherPub, _, _ := ed25519.GenerateKey(crand.Reader)
	require.Error(t, tlsutil.PinnedVerify(otherPub)(cert.Certificate, nil))
}
```

Add imports to the test file: `"crypto/ed25519"`, `crand "crypto/rand"`, and `"github.com/squall-chua/sbx-swarm-node/internal/tlsutil"` if `package tlsutil_test` (else drop qualifiers). Match the existing test file's package clause.

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tlsutil/ -run TestGenerateForKey -v`
Expected: FAIL (functions undefined).

- [x] **Step 3: Write minimal implementation** (append to `tlsutil.go`; add imports `crypto/ed25519`, `crypto/x509`, `errors`)

```go
// GenerateForKey builds an in-memory self-signed Ed25519 leaf certificate whose
// key IS the node key, so peers can pin the TLS channel to the gossiped node
// pubkey (ADR-0004). The cert is deterministic-enough to regenerate each boot.
func GenerateForKey(priv ed25519.PrivateKey) (tls.Certificate, error) {
	pub := priv.Public().(ed25519.PublicKey)
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "sbx-swarm-node"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"localhost", "sbx-swarm-node"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, pub, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create node cert: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, nil
}

// LeafPublicKey returns the Ed25519 public key in a certificate's leaf.
func LeafPublicKey(cert tls.Certificate) (ed25519.PublicKey, error) {
	if len(cert.Certificate) == 0 {
		return nil, errors.New("tlsutil: empty certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("tlsutil: parse leaf: %w", err)
	}
	pub, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("tlsutil: leaf is not Ed25519")
	}
	return pub, nil
}

// PinnedVerify returns a tls.Config.VerifyPeerCertificate that requires the
// presented leaf's pubkey to equal expected. Pair with InsecureSkipVerify:true
// (default CA chain disabled; this pin is the real check).
func PinnedVerify(expected ed25519.PublicKey) func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("tlsutil: peer presented no certificate")
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("tlsutil: parse peer leaf: %w", err)
		}
		pub, ok := leaf.PublicKey.(ed25519.PublicKey)
		if !ok || !pub.Equal(expected) {
			return errors.New("tlsutil: peer certificate pin mismatch")
		}
		return nil
	}
}
```

- [x] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tlsutil/ -v`
Expected: PASS.

- [x] **Step 5: Commit**

```bash
git add internal/tlsutil/tlsutil.go internal/tlsutil/tlsutil_test.go
git commit -m "feat(tlsutil): node-key-bound leaf cert + peer pin verifier"
```

---

### Task 6: `routing.Table` carries the peer pubkey

**Files:**
- Modify: `internal/routing/table.go`
- Modify: `internal/membership/cluster.go` (4 `Upsert` call sites)
- Test: `internal/routing/table_test.go` (append)

- [x] **Step 1: Write the failing test** (append)

```go
func TestTable_PubKey_PreservedOnMetaUpsert(t *testing.T) {
	tbl := routing.NewTable("self")
	// bulk upsert sets the pubkey
	tbl.Upsert("nB", "10.0.0.2:8443", false, []byte("PUBKEY"))
	pk, ok := tbl.PubKey("nB")
	require.True(t, ok)
	require.Equal(t, []byte("PUBKEY"), pk)

	// a later meta upsert (empty pubkey) must NOT clobber it
	tbl.Upsert("nB", "10.0.0.2:8443", true, nil)
	pk, ok = tbl.PubKey("nB")
	require.True(t, ok)
	require.Equal(t, []byte("PUBKEY"), pk)
	require.True(t, tbl.IsCordoned("nB"))

	_, ok = tbl.PubKey("unknown")
	require.False(t, ok)
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/routing/ -run TestTable_PubKey -v`
Expected: FAIL (`Upsert` arity / `PubKey` undefined).

- [x] **Step 3: Write minimal implementation**

Edit `internal/routing/table.go`:

```go
type entry struct {
	addr     string
	cordoned bool
	pubkey   []byte
}
```

```go
// Upsert records a node's address, cordon flag, and (if non-empty) gossiped
// pubkey. An empty pubkey preserves any previously-pinned key, so meta-tier
// (UDP) updates do not clobber the bulk-tier (TCP) pubkey.
func (t *Table) Upsert(nodeID, addr string, cordoned bool, pubkey []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.m[nodeID]
	e.addr = addr
	e.cordoned = cordoned
	if len(pubkey) > 0 {
		e.pubkey = pubkey
	}
	t.m[nodeID] = e
}

// PubKey returns a node's gossiped pubkey, if known.
func (t *Table) PubKey(nodeID string) ([]byte, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, ok := t.m[nodeID]
	if !ok || len(e.pubkey) == 0 {
		return nil, false
	}
	return e.pubkey, true
}
```

Edit `internal/membership/cluster.go` — update all four `Upsert` calls to pass the pubkey (meta sites pass the empty `ns.PubKey`, which is preserved):
- `cluster.go:149` → `c.tbl.Upsert(c.local.NodeID, c.local.Addr, cordoned, c.local.PubKey)`
- `cluster.go:262` → `d.c.tbl.Upsert(remote.NodeID, remote.Addr, remote.Cordoned, remote.PubKey)`
- `cluster.go:294` → `e.c.tbl.Upsert(ns.NodeID, ns.Addr, ns.Cordoned, ns.PubKey)`
- `cluster.go:304` → `e.c.tbl.Upsert(ns.NodeID, ns.Addr, ns.Cordoned, ns.PubKey)`

Then fix any other `Upsert(` callers the compiler flags (e.g. existing routing/forward tests) by appending `, nil`.

- [x] **Step 4: Run test to verify it passes**

Run: `go build ./... && go test ./internal/routing/ ./internal/membership/ -v`
Expected: PASS.

- [x] **Step 5: Commit**

```bash
git add internal/routing/table.go internal/membership/cluster.go internal/routing/table_test.go
git commit -m "feat(routing): table carries gossiped peer pubkey (preserve-on-empty)"
```

---

### Task 7: `peer.Pool` — per-target pinned TLS + node-key PerRPC creds

**Files:**
- Modify: `internal/peer/client.go`
- Modify: `internal/apiserver/forward.go:43` and `internal/apiserver/sse.go:129` (new `Conn` arity)
- Create: `internal/peer/nodekey_creds.go`
- Test: `internal/peer/client_test.go` (append), `internal/peer/pinning_test.go` (new)

- [x] **Step 1: Write the failing test** (`internal/peer/pinning_test.go`)

```go
package peer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/identity"
	"github.com/squall-chua/sbx-swarm-node/internal/nodekey"
	"github.com/squall-chua/sbx-swarm-node/internal/tlsutil"
	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

// echoNodeService records the node-key metadata it receives and returns node id.
type echoNodeService struct {
	sbxv1.UnimplementedNodeServiceServer
	gotNodeAuth chan string
}

func (s *echoNodeService) GetNodeInfo(ctx context.Context, _ *sbxv1.GetNodeInfoRequest) (*sbxv1.GetNodeInfoResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	v := md.Get(nodekey.MetadataKey)
	if len(v) > 0 {
		s.gotNodeAuth <- v[0]
	} else {
		s.gotNodeAuth <- ""
	}
	return &sbxv1.GetNodeInfoResponse{NodeId: "server"}, nil
}

func startTLSGRPC(t *testing.T, priv ed25519.PrivateKey) string {
	t.Helper()
	cert, err := tlsutil.GenerateForKey(priv)
	require.NoError(t, err)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}})))
	sbxv1.RegisterNodeServiceServer(srv, &echoNodeService{gotNodeAuth: make(chan string, 1)})
	go srv.Serve(ln)
	t.Cleanup(srv.Stop)
	return ln.Addr().String()
}

func TestPool_PinnedDial_AcceptsMatching(t *testing.T) {
	srvPub, srvPriv, _ := ed25519.GenerateKey(rand.Reader)
	srvID := identity.DeriveNodeID(srvPub)
	addr := startTLSGRPC(t, srvPriv)

	callerPub, callerPriv, _ := ed25519.GenerateKey(rand.Reader)
	callerID := identity.DeriveNodeID(callerPub)

	pins := map[string][]byte{srvID: srvPub}
	pool := NewPool(
		WithNodeKey(callerID, callerPriv),
		WithPinResolver(func(id string) ([]byte, bool) { p, ok := pins[id]; return p, ok }),
	)
	defer pool.Close()

	conn, err := pool.Conn(addr, srvID)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := sbxv1.NewNodeServiceClient(conn).GetNodeInfo(ctx, &sbxv1.GetNodeInfoRequest{})
	require.NoError(t, err)
	require.Equal(t, "server", out.NodeId)
}

func TestPool_PinnedDial_RejectsMismatch(t *testing.T) {
	srvPub, srvPriv, _ := ed25519.GenerateKey(rand.Reader)
	srvID := identity.DeriveNodeID(srvPub)
	addr := startTLSGRPC(t, srvPriv)

	wrongPub, _, _ := ed25519.GenerateKey(rand.Reader) // gossiped pin is wrong
	_, callerPriv, _ := ed25519.GenerateKey(rand.Reader)
	pool := NewPool(
		WithNodeKey("caller", callerPriv),
		WithPinResolver(func(string) ([]byte, bool) { return wrongPub, true }),
	)
	defer pool.Close()

	conn, err := pool.Conn(addr, srvID)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = sbxv1.NewNodeServiceClient(conn).GetNodeInfo(ctx, &sbxv1.GetNodeInfoRequest{})
	require.Error(t, err) // TLS handshake fails the pin
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/peer/ -run TestPool_Pinned -v`
Expected: FAIL (`WithNodeKey`, `WithPinResolver`, `Conn` arity undefined).

- [x] **Step 3: Write minimal implementation**

Create `internal/peer/nodekey_creds.go`:

```go
package peer

import (
	"context"
	"crypto/ed25519"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/nodekey"
)

// nodeKeyCreds implements grpc/credentials.PerRPCCredentials, attaching an
// audience-bound node-key token (ADR-0004) for a fixed target node.
type nodeKeyCreds struct {
	callerID string
	priv     ed25519.PrivateKey
	targetID string
}

func (c nodeKeyCreds) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{nodekey.MetadataKey: nodekey.Sign(c.priv, c.callerID, c.targetID, time.Now())}, nil
}

func (c nodeKeyCreds) RequireTransportSecurity() bool { return true }
```

Edit `internal/peer/client.go`:

```go
// Pool caches one gRPC client connection per peer address.
type Pool struct {
	mu          sync.Mutex
	conns       map[string]*grpc.ClientConn
	dialer      func(context.Context, string) (net.Conn, error)
	creds       credentials.TransportCredentials
	callerID    string
	priv        ed25519.PrivateKey
	pinResolver func(nodeID string) ([]byte, bool)
}
```

```go
// WithNodeKey sets the local identity used to sign per-peer node-key tokens.
func WithNodeKey(callerID string, priv ed25519.PrivateKey) Option {
	return func(p *Pool) { p.callerID = callerID; p.priv = priv }
}

// WithPinResolver supplies the gossiped pubkey for a target node id (TLS pin).
func WithPinResolver(f func(nodeID string) ([]byte, bool)) Option {
	return func(p *Pool) { p.pinResolver = f }
}
```

Replace `Conn` (note the new `targetNodeID` parameter and per-target creds):

```go
// Conn returns a cached connection to addr (owned by targetNodeID), dialing if
// needed. When a pin resolver + node key are configured it builds per-target
// pinned TLS creds and attaches a node-key PerRPCCredentials; otherwise it falls
// back to the static creds (tests / standalone).
func (p *Pool) Conn(addr, targetNodeID string) (*grpc.ClientConn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.conns[addr]; ok {
		return c, nil
	}

	var dialOpts []grpc.DialOption
	target := addr
	if p.dialer != nil {
		dialOpts = append(dialOpts, grpc.WithContextDialer(p.dialer))
		target = "passthrough:///" + addr
	}

	switch {
	case p.pinResolver != nil && p.priv != nil:
		pin, ok := p.pinResolver(targetNodeID)
		if !ok {
			return nil, fmt.Errorf("peer: no pin known for node %s (fail-closed)", targetNodeID)
		}
		tlsCfg := &tls.Config{
			InsecureSkipVerify:    true, //nolint:gosec // pin is enforced below
			VerifyPeerCertificate: tlsutil.PinnedVerify(ed25519.PublicKey(pin)),
		}
		dialOpts = append(dialOpts,
			grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
			grpc.WithPerRPCCredentials(nodeKeyCreds{callerID: p.callerID, priv: p.priv, targetID: targetNodeID}),
		)
	case p.creds != nil:
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(p.creds))
	}

	c, err := grpc.NewClient(target, dialOpts...)
	if err != nil {
		return nil, err
	}
	p.conns[addr] = c
	return c, nil
}
```

Add imports to `client.go`: `crypto/ed25519`, `crypto/tls`, `fmt`, and `"github.com/squall-chua/sbx-swarm-node/internal/tlsutil"`.

Update callers:
- `internal/apiserver/forward.go:43` → `conn, err := f.pool.Conn(addr, owner)`
- `internal/apiserver/sse.go:129` → `conn, err := pool.Conn(addr, nodeID)`
- Fix `internal/peer/client_test.go` existing `Conn(...)` calls to pass a second arg (e.g. `p.Conn(addr, "peer")`); that test uses `WithCreds(insecure...)` so it stays on the fallback branch.

- [x] **Step 4: Run test to verify it passes**

Run: `go build ./... && go test ./internal/peer/ -race -v`
Expected: PASS (pin accept + reject; existing reuse test still green).

- [x] **Step 5: Commit**

```bash
git add internal/peer/ internal/apiserver/forward.go internal/apiserver/sse.go
git commit -m "feat(peer): per-target pinned TLS + node-key PerRPC creds"
```

---

### Task 8: `OwnerProxy` — per-target pinned transport

**Files:**
- Modify: `internal/apiserver/forward_http.go`
- Test: `internal/apiserver/forward_http_test.go` (update the existing forward-to-owner test + add a mismatch test)

- [x] **Step 1: Write the failing test**

The `OwnerProxy` signature gains a `PinResolver` argument, so **every** caller in `forward_http_test.go` must be updated. The three fall-through tests (`TestOwnerProxy_LocalFallsThrough`, `TestOwnerProxy_UnknownOwnerFallsThrough`, `TestOwnerProxy_NonRoutableFallsThrough`) never proxy, so pass a nil-returning resolver:

```go
	noPin := func(string) (crypto.PublicKey, bool) { return nil, false }
	h := OwnerProxy(tbl, noPin, localSentinel(&called))
```

Replace the existing `TestOwnerProxy_RemoteProxies` (the one using `httptest.NewTLSServer` with `tbl.Upsert("nB", ownerAddr, false)`) with the two tests below. The owner backend's real leaf pubkey is fed to the pin resolver:

```go
func TestOwnerProxy_PinnedForwardToOwner(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()
	addr := strings.TrimPrefix(backend.URL, "https://")

	leafPub := backend.Certificate().PublicKey
	tbl := routing.NewTable("nA")
	tbl.Upsert("nB", addr, false, nil) // pubkey for pin comes from the resolver below

	resolver := func(nodeID string) (crypto.PublicKey, bool) {
		if nodeID == "nB" {
			return leafPub, true
		}
		return nil, false
	}
	h := OwnerProxy(tbl, resolver, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should have proxied")
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/nB.abc", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "ok", rr.Body.String())
}

func TestOwnerProxy_PinMismatchFailsClosed(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	addr := strings.TrimPrefix(backend.URL, "https://")

	wrongPub, _, _ := ed25519.GenerateKey(rand.Reader)
	tbl := routing.NewTable("nA")
	tbl.Upsert("nB", addr, false, nil)
	resolver := func(string) (crypto.PublicKey, bool) { return wrongPub, true }
	h := OwnerProxy(tbl, resolver, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/nB.abc", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	require.Equal(t, http.StatusBadGateway, rr.Code) // pin rejected the channel
}
```

Add test imports: `crypto`, `crypto/ed25519`, `crypto/rand`. **Note:** `httptest.NewTLSServer` uses an RSA cert, so `backend.Certificate().PublicKey` is `*rsa.PublicKey`, not Ed25519. Therefore the pin resolver must return a generic `crypto.PublicKey` and `PinnedVerify` must compare with a type-agnostic equality. Adjust `tlsutil.PinnedVerify` to accept `crypto.PublicKey` and compare via the `interface{ Equal(x crypto.PublicKey) bool }` that `ed25519`/`rsa`/`ecdsa` public keys all implement. **Make this adjustment as part of this task** (extend the Task 5 helper):

```go
// PinnedVerify (revised): accept any crypto.PublicKey and compare structurally.
func PinnedVerify(expected crypto.PublicKey) func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	type equaler interface{ Equal(crypto.PublicKey) bool }
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("tlsutil: peer presented no certificate")
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("tlsutil: parse peer leaf: %w", err)
		}
		eq, ok := leaf.PublicKey.(equaler)
		if !ok || !eq.Equal(expected) {
			return errors.New("tlsutil: peer certificate pin mismatch")
		}
		return nil
	}
}
```

Update Task 5's test call `tlsutil.PinnedVerify(pub)` accordingly (pass `ed25519.PublicKey`, still works since it implements `Equal(crypto.PublicKey)`). The pin stored in `routing.Table` is the Ed25519 pubkey bytes; the resolver passed to `OwnerProxy`/`peer.Pool` converts `[]byte` → `ed25519.PublicKey`. For the gRPC pool the pin is always Ed25519 (node-key cert); only the httptest-based OwnerProxy test uses RSA, which is why the resolver type is `crypto.PublicKey`.

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/apiserver/ -run TestOwnerProxy -v`
Expected: FAIL (`OwnerProxy` arity changed; undefined resolver param).

- [x] **Step 3: Write minimal implementation**

Edit `internal/apiserver/forward_http.go`:

```go
// PinResolver returns the expected peer public key for a node id (TLS pin).
type PinResolver func(nodeID string) (crypto.PublicKey, bool)

// OwnerProxy reverse-proxies REST/SSE for a remote sandbox to its owning node,
// pinning the owner's TLS leaf cert to its gossiped pubkey (ADR-0004). Fail-closed
// (502) when no pin is known.
func OwnerProxy(tbl *routing.Table, pins PinResolver, next http.Handler) http.Handler {
	if tbl == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := sandboxPathID(r.URL.Path)
		if !ok || !strings.Contains(id, ".") || tbl.IsLocal(id) {
			next.ServeHTTP(w, r)
			return
		}
		owner, found := tbl.Owner(id)
		if !found {
			next.ServeHTTP(w, r)
			return
		}
		addr, ok := tbl.Addr(owner)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		pin, ok := pins(owner)
		if !ok {
			http.Error(w, "no TLS pin known for owner node", http.StatusBadGateway)
			return
		}
		transport := &http.Transport{TLSClientConfig: &tls.Config{
			InsecureSkipVerify:    true, //nolint:gosec // pin is enforced below
			VerifyPeerCertificate: tlsutil.PinnedVerify(pin),
		}}
		proxy := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Scheme = "https"
				req.URL.Host = addr
				req.Host = addr
			},
			Transport:     transport,
			FlushInterval: -1,
		}
		proxy.ServeHTTP(w, r)
	})
}
```

Add imports: `crypto`, and `"github.com/squall-chua/sbx-swarm-node/internal/tlsutil"`. Remove the package-level shared `transport` var.

- [x] **Step 4: Run test to verify it passes**

Run: `go test ./internal/apiserver/ -run TestOwnerProxy -v && go build ./...`
Expected: PASS. (The `server.go` call to `OwnerProxy` won't compile yet — that's fixed in Task 9; build the `tlsutil`/`apiserver` test for this task with `go test ./internal/apiserver/ -run TestOwnerProxy` which compiles the package including server.go, so update the `server.go` call site to the new 3-arg form here too, passing a temporary `func(string)(crypto.PublicKey,bool){return nil,false}` if needed, and finalize in Task 9.)

> To keep the package compiling: in `server.go`, change `OwnerProxy(opts.Routing, v1)` to `OwnerProxy(opts.Routing, opts.Pins, v1)` and add `Pins PinResolver` to `Options` now (wired for real in Task 10). A nil `opts.Pins` with a non-nil `Routing` should be guarded: if `opts.Pins == nil`, default it to a fail-closed `func(string)(crypto.PublicKey,bool){return nil,false}` inside `Build`.

- [x] **Step 5: Commit**

```bash
git add internal/apiserver/forward_http.go internal/apiserver/forward_http_test.go internal/tlsutil/ internal/apiserver/server.go
git commit -m "feat(apiserver): OwnerProxy pins owner TLS to gossiped pubkey (fail-closed)"
```

---

### Task 9: `apiserver.Build` — loopback gateway + annotator + interceptor chain

**Files:**
- Modify: `internal/apiserver/server.go`
- Create: `internal/apiserver/loopback.go`
- Modify: `internal/apiserver/server_test.go` (fix the credential-less gRPC test) + add role-gate tests
- Test: `internal/apiserver/rolegate_test.go` (new)

- [x] **Step 1: Write the failing test** (`internal/apiserver/rolegate_test.go`)

```go
package apiserver

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/ops"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
)

func startRoleGateServer(t *testing.T) (string, func()) {
	t.Helper()
	cert := mustSelfSigned(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	gen := ids.NewGen("n1")
	mgr := sandbox.NewManager("n1", sandbox.NewFake(), st, gen)
	svc := NewSandboxService(mgr, ops.NewManager(st, gen))
	h, grpcSrv, err := Build(Options{
		NodeID: "n1", NodeName: "n", Version: "v0",
		Keys:      keyMap{"adm": "admin", "ro": "read-only"},
		Signer:    testSigner(),
		Cert:      cert,
		Sandboxes: svc,
	})
	require.NoError(t, err)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &http.Server{Handler: h, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", "http/1.1"}}}
	go srv.ServeTLS(ln, "", "")
	return ln.Addr().String(), func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		grpcSrv.Stop()
		_ = st.Close()
	}
}

func TestRoleGate_ReadOnlyCannotCreateOverREST(t *testing.T) {
	addr, cleanup := startRoleGateServer(t)
	defer cleanup()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}

	post := func(key string) int {
		req, _ := http.NewRequest(http.MethodPost, "https://"+addr+"/v1/sandboxes", strings.NewReader(`{"cpus":1}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		return resp.StatusCode
	}
	require.Equal(t, http.StatusForbidden, post("ro")) // read-only blocked
	require.Equal(t, http.StatusOK, post("adm"))       // admin allowed
}

func TestRoleGate_ReadOnlyCanListOverREST(t *testing.T) {
	addr, cleanup := startRoleGateServer(t)
	defer cleanup()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	req, _ := http.NewRequest(http.MethodGet, "https://"+addr+"/v1/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer ro")
	resp, err := client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/apiserver/ -run TestRoleGate -v`
Expected: FAIL — currently read-only POST returns 200 (no gate), expected 403.

- [x] **Step 3: Write minimal implementation**

Create `internal/apiserver/loopback.go`:

```go
package apiserver

import (
	"context"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

const loopbackBuf = 1 << 20

func loopback(grpcSrv *grpc.Server) (*grpc.ClientConn, *bufconn.Listener, error) {
	lis := bufconn.Listen(loopbackBuf)
	go func() { _ = grpcSrv.Serve(lis) }()
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		lis.Close()
		return nil, nil, err
	}
	return conn, lis, nil
}
```

Edit `internal/apiserver/server.go`:

1. Add to `Options`: `Pins PinResolver` and `Denylist func(nodeID string) bool` and `PubKeyFor func(nodeID string) ([]byte, bool)`.
2. Build the interceptor chain and loopback. Replace the grpc server construction + gateway registration:

```go
	// Authn + authz always-on (closes the native-gRPC port even standalone).
	authn := newAuthenticator(authnDeps{
		Signer: opts.Signer, Keys: opts.Keys, LocalNodeID: opts.NodeID,
		PubKeyFor: opts.PubKeyFor, Denied: opts.Denylist,
	})
	unary := []grpc.UnaryServerInterceptor{authn.unaryInterceptor(), authzUnaryInterceptor()}
	if opts.Forward != nil {
		unary = append(unary, opts.Forward.UnaryInterceptor())
	}
	grpcSrv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(unary...),
		grpc.ChainStreamInterceptor(authn.streamInterceptor(), authzStreamInterceptor()),
	)

	sbxv1.RegisterNodeServiceServer(grpcSrv, node)
	if opts.Sandboxes != nil {
		sbxv1.RegisterSandboxServiceServer(grpcSrv, opts.Sandboxes)
	}
	if opts.Policy != nil {
		sbxv1.RegisterPolicyServiceServer(grpcSrv, opts.Policy)
	}
	if opts.Events != nil {
		sbxv1.RegisterEventServiceServer(grpcSrv, NewEventService(opts.Events))
	}

	// Loopback: the gateway dials the local gRPC server so REST traverses the
	// interceptor chain. Bridge the HTTP-authenticated role across the wire as a
	// signed x-sbx-authz token.
	loopConn, _, err := loopback(grpcSrv)
	if err != nil {
		return nil, nil, err
	}
	gw := runtime.NewServeMux(
		runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{
			MarshalOptions:   protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true},
			UnmarshalOptions: protojson.UnmarshalOptions{DiscardUnknown: true},
		}),
		runtime.WithIncomingHeaderMatcher(incomingHeaderMatcher),
		runtime.WithMetadata(roleAnnotator(opts.Signer)),
	)
	if err := sbxv1.RegisterNodeServiceHandler(context.Background(), gw, loopConn); err != nil {
		return nil, nil, err
	}
	if opts.Sandboxes != nil {
		if err := sbxv1.RegisterSandboxServiceHandler(context.Background(), gw, loopConn); err != nil {
			return nil, nil, err
		}
	}
	if opts.Policy != nil {
		if err := sbxv1.RegisterPolicyServiceHandler(context.Background(), gw, loopConn); err != nil {
			return nil, nil, err
		}
	}
```

3. Update the OwnerProxy call to use `opts.Pins`, defaulting to fail-closed:

```go
	if opts.Routing != nil {
		pins := opts.Pins
		if pins == nil {
			pins = func(string) (crypto.PublicKey, bool) { return nil, false }
		}
		v1 = OwnerProxy(opts.Routing, pins, v1)
	}
```

4. Add `roleAnnotator` (new func in `server.go` or `authn.go`):

```go
// roleAnnotator injects the HTTP-authenticated role across the loopback as a
// signed x-sbx-authz token (cookie callers have no Authorization header).
func roleAnnotator(signer *auth.Signer) func(context.Context, *http.Request) metadata.MD {
	return func(_ context.Context, r *http.Request) metadata.MD {
		role, ok := auth.RoleFromContext(r.Context())
		if !ok {
			return nil
		}
		return metadata.Pairs("x-sbx-authz", signer.Mint(role, time.Now().Add(30*time.Second)))
	}
}
```

Add imports to `server.go`: `crypto`, `google.golang.org/grpc/metadata`. Remove the now-unused `RegisterXHandlerServer` import usage.

5. Fix `internal/apiserver/server_test.go::TestServer_GRPCGetNodeInfo` — the native gRPC call now requires credentials. Add a bearer to the call:

```go
func TestServer_GRPCGetNodeInfo(t *testing.T) {
	addr, cleanup := startTestServer(t)
	defer cleanup()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})))
	require.NoError(t, err)
	defer conn.Close()

	// no creds -> Unauthenticated
	_, err = sbxv1.NewNodeServiceClient(conn).GetNodeInfo(context.Background(), &sbxv1.GetNodeInfoRequest{})
	require.Equal(t, codes.Unauthenticated, status.Code(err))

	// bearer in metadata -> ok
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer adm"))
	out, err := sbxv1.NewNodeServiceClient(conn).GetNodeInfo(ctx, &sbxv1.GetNodeInfoRequest{})
	require.NoError(t, err)
	require.Equal(t, "n1", out.NodeId)
}
```

Add imports to `server_test.go`: `google.golang.org/grpc/codes`, `google.golang.org/grpc/metadata`, `google.golang.org/grpc/status`.

- [x] **Step 4: Run test to verify it passes**

Run: `go build ./... && go test ./internal/apiserver/ -race -v`
Expected: PASS — role-gate REST tests pass; gRPC no-creds → Unauthenticated, bearer → ok; existing REST create (admin) still 200; idempotency + SSE + forward tests still green.

- [x] **Step 5: Commit**

```bash
git add internal/apiserver/
git commit -m "feat(apiserver): loopback gateway + always-on authn/authz role-gate"
```

---

### Task 10: Wire node.go — pubkey gossip, swarm-wide session key, pinned dialer

**Files:**
- Modify: `internal/node/node.go`
- Test: `internal/node/node_test.go` (append a build/boot smoke + session-key assertion)

- [x] **Step 1: Write the failing test** (append)

```go
func TestNode_SessionKeyIsSwarmWideWhenClustered(t *testing.T) {
	// Two nodes with the same cluster secret derive the same session signer, so a
	// token minted by one verifies on the other (cross-node sessions, ADR-0010).
	seedA := bytes.Repeat([]byte{1}, ed25519.SeedSize)
	seedB := bytes.Repeat([]byte{2}, ed25519.SeedSize)
	kA := auth.DeriveSessionKey("shared-secret", ed25519.NewKeyFromSeed(seedA).Seed())
	kB := auth.DeriveSessionKey("shared-secret", ed25519.NewKeyFromSeed(seedB).Seed())
	require.Equal(t, kA, kB)
}
```

Add imports `bytes`, `crypto/ed25519`, `github.com/squall-chua/sbx-swarm-node/internal/auth` as needed. (This locks the contract; the wiring change below is verified by `go build` + the apiserver/integration tests.)

- [x] **Step 2: Run test to verify it fails / build breaks**

Run: `go test ./internal/node/ -run TestNode_SessionKey -v`
Expected: PASS already (Task 2 added the helper) — this test guards the invariant. The real verification for this task is the wiring compiling and the integration test (Task 11).

- [x] **Step 3: Write minimal implementation** — edit `internal/node/node.go`:

1. Derive the session key from the cluster secret (replace `node.go:118`):

```go
	signer := auth.NewSigner(auth.DeriveSessionKey(cfg.ClusterSecret, id.PrivateKey.Seed()))
```

2. Generate the TLS cert from the node key when none is operator-provided (replace the `tlsutil.LoadOrGenerate` call at `node.go:112`):

```go
	var cert tls.Certificate
	if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
		cert, err = tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	} else {
		cert, err = tlsutil.GenerateForKey(id.PrivateKey) // leaf pubkey == node pubkey (pinning)
	}
	if err != nil {
		cancel()
		_ = st.Close()
		return nil, fmt.Errorf("tls: %w", err)
	}
```

3. Populate the local `NodeState.PubKey` (add to the `localNS := membership.NodeState{...}` literal at `node.go:148`):

```go
		PubKey: id.PublicKey,
```

4. Build the peer pool with the pinned dialer + node key (replace the `tlsCreds`/`pool` block at `node.go:138-139`):

```go
	pool := peer.NewPool(
		peer.WithNodeKey(id.NodeID, id.PrivateKey),
		peer.WithPinResolver(func(nodeID string) ([]byte, bool) { return tbl.PubKey(nodeID) }),
	)
```

(Delete the now-unused `tlsCreds` line and the `credentials`/`crypto/tls` imports if they become unused — check with `go build`.)

5. Pass pinning + node-key verification into `apiserver.Build` Options (extend the `apiserver.Build(apiserver.Options{...})` call at `node.go:187`):

```go
		Pins:      func(nodeID string) (crypto.PublicKey, bool) {
			pk, ok := tbl.PubKey(nodeID)
			if !ok {
				return nil, false
			}
			return ed25519.PublicKey(pk), true
		},
		PubKeyFor: func(nodeID string) ([]byte, bool) { return tbl.PubKey(nodeID) },
		// Denylist: nil for v1 (local-only hook; gossiped revocation is vNext).
```

Add imports to `node.go`: `crypto`, `crypto/ed25519`. Keep `crypto/tls` (still used for the server `TLSConfig`).

- [x] **Step 4: Verify build + full suite**

Run: `go build ./... && go vet ./... && go test ./... `
Expected: PASS across all packages (default, non-integration).

- [x] **Step 5: Commit**

```bash
git add internal/node/node.go internal/node/node_test.go
git commit -m "feat(node): node-key TLS cert, swarm-wide session key, pinned peer dialer"
```

---

### Task 11: Integration — cross-node forwarded role-gate + browser session

**Files:**
- Modify: `internal/membership/cluster_integration_test.go` (add the read-only key to the shared `startNode` helper, line 53)
- Create: `internal/membership/security_integration_test.go`
- Test build tag: `//go:build integration`

**Context:** The existing integration suite (`go test -tags integration ./internal/membership/`) already provides shared helpers in `package membership_test`: `startNode(t, listenAddr, gossipAddr, seeds)`, `tlsClient()`, `authedGet(t, client, url, key)`, `waitForPeer(t, nodeA, peerID, timeout)`, and `createSandboxOnB(t, client, owner)` (POSTs a sandbox on the owner and returns its `<nodeID>.<ulid>` id). The new file shares those. The existing forward/SSE tests (`TestCluster_ForwardSandboxRequest`, `TestCluster_ForwardLogsSSE`) already exercise cross-node forwarding and the `WatchEvents` SSE merge — after this milestone they implicitly cover the node-key-authenticated peer path and must stay green (the final verification re-runs them).

- [x] **Step 1: Add the read-only key to the shared helper**

In `internal/membership/cluster_integration_test.go:53`, change:

```go
	cfg.APIKeys = []config.APIKey{{Key: "adm", Role: "admin"}}
```
to (identical keys on every node — the ADR-0010 swarm-wide invariant):
```go
	cfg.APIKeys = []config.APIKey{{Key: "adm", Role: "admin"}, {Key: "ro", Role: "read-only"}}
```

- [x] **Step 2: Write the failing tests**

Create `internal/membership/security_integration_test.go`:

```go
//go:build integration

package membership_test

import (
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// postStatus issues a bodyless POST with a bearer key and returns the status.
func postStatus(t *testing.T, client *http.Client, url, key string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	return resp.StatusCode
}

// TestSecurity_ForwardedMutationRoleGate: a mutating call (StopSandbox) for a
// B-owned sandbox, issued at non-owner A, is gated at the owner — read-only is
// rejected (403, relayed by A), admin is allowed (200).
func TestSecurity_ForwardedMutationRoleGate(t *testing.T) {
	nodeA := startNode(t, "127.0.0.1:19643", "127.0.0.1:17966", nil)
	nodeB := startNode(t, "127.0.0.1:19644", "127.0.0.1:17967", []string{"127.0.0.1:17966"})
	waitForPeer(t, nodeA, nodeB.NodeID(), 10*time.Second)

	client := tlsClient()
	sbxID := createSandboxOnB(t, client, nodeB)

	stopURL := fmt.Sprintf("https://%s/v1/sandboxes/%s/stop", nodeA.Addr(), sbxID)
	require.Equal(t, http.StatusForbidden, postStatus(t, client, stopURL, "ro"),
		"read-only must be rejected on a forwarded mutation")
	require.Equal(t, http.StatusOK, postStatus(t, client, stopURL, "adm"),
		"admin must be allowed on a forwarded mutation")
}

// TestSecurity_CrossNodeBrowserSession: a session cookie minted on A authorizes a
// forwarded read on B (swarm-wide session key, ADR-0010).
func TestSecurity_CrossNodeBrowserSession(t *testing.T) {
	nodeA := startNode(t, "127.0.0.1:19645", "127.0.0.1:17968", nil)
	nodeB := startNode(t, "127.0.0.1:19646", "127.0.0.1:17969", []string{"127.0.0.1:17968"})
	waitForPeer(t, nodeA, nodeB.NodeID(), 10*time.Second)

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	client := tlsClient()
	client.Jar = jar

	// Mint a session on A by exchanging the admin bearer.
	sessURL := fmt.Sprintf("https://%s/v1/auth/session", nodeA.Addr())
	req, _ := http.NewRequest(http.MethodPost, sessURL, nil)
	req.Header.Set("Authorization", "Bearer adm")
	resp, err := client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	// Create a B-owned sandbox (bearer), then fetch it via A using ONLY the cookie.
	sbxID := createSandboxOnB(t, tlsClient(), nodeB)
	getURL := fmt.Sprintf("https://%s/v1/sandboxes/%s", nodeA.Addr(), sbxID)
	getReq, _ := http.NewRequest(http.MethodGet, getURL, nil) // no Authorization; cookie jar carries the session
	getResp, err := client.Do(getReq)
	require.NoError(t, err)
	defer getResp.Body.Close()
	require.Equal(t, http.StatusOK, getResp.StatusCode,
		"a session minted on A must authorize a forwarded read on B (swarm-wide session key)")
}
```

> Note on the pin-mismatch (fail-closed) path: it is covered white-box by the unit
> tests in Tasks 7 (`TestPool_PinnedDial_RejectsMismatch`) and 8
> (`TestOwnerProxy_PinMismatchFailsClosed`); reproducing it black-box here would
> require corrupting a gossiped pubkey, which the harness can't reach, so it is
> intentionally not asserted at the integration layer.

- [x] **Step 3: Run tests to verify they fail (before the milestone code) / pass (after)**

Run: `go test -tags integration ./internal/membership/ -run TestSecurity -v -timeout 120s`
Expected: PASS once Tasks 1–10 are implemented (read-only forwarded mutation → 403; admin → 200; cross-node cookie read → 200). If run before Task 9/10, the role-gate test fails (read-only would get 200).

- [x] **Step 4: Run the full integration suite + race**

Run:
```bash
go test -tags integration ./internal/membership/ -v -timeout 120s
go test -race ./internal/apiserver/ ./internal/peer/ ./internal/routing/ ./internal/nodekey/ ./internal/membership/
go test ./...
```
Expected: PASS everywhere (including the pre-existing `TestCluster_ForwardSandboxRequest` / `TestCluster_ForwardLogsSSE`, which now exercise the node-key-authenticated peer path); no data races.

- [x] **Step 5: Commit**

```bash
git add internal/membership/security_integration_test.go internal/membership/cluster_integration_test.go
git commit -m "test(security): cross-node forwarded role-gate + browser session integration"
```

---

## Final verification (after all tasks)

- [ ] `go build ./... && go vet ./...` clean.
- [ ] `go test ./...` green (default suite).
- [ ] `go test -tags integration ./internal/membership/ -timeout 120s` green (run, don't just compile).
- [ ] `go test -race ./internal/apiserver/ ./internal/peer/ ./internal/membership/ ./internal/routing/ ./internal/events/ ./internal/ops/` green.
- [ ] Leak audit: `grep -rn "node.key\|PrivateKey\|x-sbx-node-auth\|x-sbx-authz" internal/ | grep -i "log\|slog\|Printf"` returns nothing (no secret/token logging).
- [ ] One independent Opus holistic review over the whole branch diff, then a fix round (resume the implementer subagent), then re-verify.
- [ ] Flip plan checkboxes; `finishing-a-development-branch` → ff-merge `security-node-trust` → `main`; update project memory; then proceed to M5.
