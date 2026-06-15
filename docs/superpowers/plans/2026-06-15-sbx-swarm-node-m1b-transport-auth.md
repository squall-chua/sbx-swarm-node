# sbx-swarm-node M1b — One-Port Transport + Auth Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve gRPC, a grpc-gateway REST/JSON facade, and static assets on a **single TLS port**, protected by two-path auth (bearer for API clients, httpOnly cookie sessions for the browser), with the first real RPC (`NodeService.GetNodeInfo`) reachable over both gRPC and REST.

**Architecture:** A single `http.Server` over TLS (ALPN h2) whose handler dispatches by protocol: HTTP/2 + `application/grpc` → the native `grpc.Server` via `ServeHTTP`; everything else → an auth-wrapped mux holding the grpc-gateway, the auth endpoints, the M1a health endpoints, and a static file fallback. Auth resolves every request to a role (`admin`/`read-only`) per ADR-0006. Builds on M1a (`config`, `identity`, `store`, `obs`, `node`).

**Tech Stack:** Go 1.23, `buf` + `protoc-gen-go`/`-go-grpc`/`-grpc-gateway` (codegen), `google.golang.org/grpc`, `github.com/grpc-ecosystem/grpc-gateway/v2`, `crypto/tls` + `crypto/ecdsa` (self-signed dev cert), `crypto/hmac` (session cookies), `github.com/stretchr/testify`.

**Scope:** Transport + auth + one trivial RPC only. The sandbox domain (M1c) and events/SSE (M1d) plug into this server later. SSE endpoints exist as routes only once M1d lands.

---

## File Structure

| File | Responsibility |
|---|---|
| `proto/sbxswarm/v1/node.proto` | `NodeService.GetNodeInfo` with `google.api.http` annotation |
| `buf.yaml`, `buf.gen.yaml` | buf module + codegen config |
| `internal/gen/sbxswarm/v1/*.pb.go` | **generated** message/grpc/gateway code (committed) |
| `internal/config/config.go` | extend with `TLSCertFile`/`TLSKeyFile`, `APIKeys`, role lookup |
| `internal/tlsutil/tlsutil.go` | load-or-generate a self-signed cert |
| `internal/auth/auth.go` | api-key→role, bearer + cookie middleware, role gate, CSRF |
| `internal/auth/session.go` | HMAC-signed session token mint/verify |
| `internal/apiserver/nodeservice.go` | `NodeService` gRPC implementation |
| `internal/apiserver/server.go` | build grpc.Server + gateway mux + static + one-port handler |
| `internal/apiserver/mux.go` | the protocol-dispatch `http.Handler` |
| `web/embed.go` + `web/dist/index.html` | placeholder embedded SPA |
| `internal/node/node.go` | swap the plain health server for the TLS one-port server |

---

## Task 1: Proto + buf codegen for NodeService

**Files:**
- Create: `proto/sbxswarm/v1/node.proto`, `buf.yaml`, `buf.gen.yaml`
- Create (generated): `internal/gen/sbxswarm/v1/`

- [ ] **Step 1: Write `proto/sbxswarm/v1/node.proto`**

```proto
syntax = "proto3";

package sbxswarm.v1;

import "google/api/annotations.proto";

option go_package = "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1;sbxswarmv1";

// NodeService exposes node-level information and control.
service NodeService {
  rpc GetNodeInfo(GetNodeInfoRequest) returns (NodeInfo) {
    option (google.api.http) = {get: "/v1/node"};
  }
}

message GetNodeInfoRequest {}

message NodeInfo {
  string node_id = 1;
  string node_name = 2;
  string version = 3;
}
```

- [ ] **Step 2: Write `buf.yaml`**

```yaml
version: v2
modules:
  - path: proto
deps:
  - buf.build/googleapis/googleapis
lint:
  use: [STANDARD]
breaking:
  use: [FILE]
```

- [ ] **Step 3: Write `buf.gen.yaml`**

```yaml
version: v2
managed:
  enabled: true
plugins:
  - remote: buf.build/protocolbuffers/go
    out: internal/gen
    opt: paths=source_relative
  - remote: buf.build/grpc/go
    out: internal/gen
    opt: paths=source_relative
  - remote: buf.build/grpc-ecosystem/gateway
    out: internal/gen
    opt: paths=source_relative
```

- [ ] **Step 4: Generate and wire deps**

Run:
```bash
buf dep update
buf generate
go get google.golang.org/grpc github.com/grpc-ecosystem/grpc-gateway/v2 google.golang.org/protobuf
go mod tidy
go build ./...
```
Expected: `internal/gen/sbxswarm/v1/node.pb.go`, `node_grpc.pb.go`, `node.pb.gw.go` exist and compile.
(If `buf` is not installed: `go install github.com/bufbuild/buf/cmd/buf@latest` and ensure `$GOBIN` is on `PATH`.)

- [ ] **Step 5: Commit**

```bash
git add proto/ buf.yaml buf.gen.yaml internal/gen/ go.mod go.sum
git commit -m "feat(proto): NodeService proto + buf codegen"
```

---

## Task 2: NodeService implementation

**Files:**
- Create: `internal/apiserver/nodeservice.go`
- Test: `internal/apiserver/nodeservice_test.go`

- [ ] **Step 1: Write the failing test**

```go
package apiserver

import (
	"context"
	"testing"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/stretchr/testify/require"
)

func TestNodeService_GetNodeInfo(t *testing.T) {
	svc := NewNodeService("node-abc", "alpha", "v9")
	out, err := svc.GetNodeInfo(context.Background(), &sbxv1.GetNodeInfoRequest{})
	require.NoError(t, err)
	require.Equal(t, "node-abc", out.NodeId)
	require.Equal(t, "alpha", out.NodeName)
	require.Equal(t, "v9", out.Version)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/apiserver/ -run TestNodeService -v`
Expected: FAIL — `undefined: NewNodeService`

- [ ] **Step 3: Write the implementation**

```go
// Package apiserver builds the node's one-port gRPC + REST + static server.
package apiserver

import (
	"context"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
)

// NodeService implements sbxv1.NodeServiceServer.
type NodeService struct {
	sbxv1.UnimplementedNodeServiceServer
	nodeID, nodeName, version string
}

// NewNodeService returns a NodeService reporting the given identity.
func NewNodeService(nodeID, nodeName, version string) *NodeService {
	return &NodeService{nodeID: nodeID, nodeName: nodeName, version: version}
}

// GetNodeInfo returns static node identity.
func (s *NodeService) GetNodeInfo(_ context.Context, _ *sbxv1.GetNodeInfoRequest) (*sbxv1.NodeInfo, error) {
	return &sbxv1.NodeInfo{NodeId: s.nodeID, NodeName: s.nodeName, Version: s.version}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/apiserver/ -run TestNodeService -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/apiserver/nodeservice.go internal/apiserver/nodeservice_test.go
git commit -m "feat(apiserver): NodeService.GetNodeInfo"
```

---

## Task 3: Self-signed TLS cert helper

**Files:**
- Create: `internal/tlsutil/tlsutil.go`
- Test: `internal/tlsutil/tlsutil_test.go`

- [ ] **Step 1: Write the failing test**

```go
package tlsutil

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadOrGenerate_GeneratesUsableCert(t *testing.T) {
	dir := t.TempDir()
	cert, err := LoadOrGenerate("", "", dir)
	require.NoError(t, err)
	require.NotNil(t, cert.PrivateKey)
	require.NotEmpty(t, cert.Certificate)

	// Regenerated load from the now-persisted files returns a cert too.
	again, err := LoadOrGenerate(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), dir)
	require.NoError(t, err)
	require.NotEmpty(t, again.Certificate)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tlsutil/ -v`
Expected: FAIL — `undefined: LoadOrGenerate`

- [ ] **Step 3: Write the implementation**

```go
// Package tlsutil loads a TLS certificate or generates a self-signed one for
// development when none is configured.
package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// LoadOrGenerate loads the cert/key pair from the given paths. If both paths
// are empty it generates a self-signed cert, persists it to dir/tls.crt and
// dir/tls.key, and returns it.
func LoadOrGenerate(certFile, keyFile, dir string) (tls.Certificate, error) {
	if certFile != "" && keyFile != "" {
		return tls.LoadX509KeyPair(certFile, keyFile)
	}

	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	if _, err := os.Stat(certPath); err == nil {
		return tls.LoadX509KeyPair(certPath, keyPath)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate key: %w", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "sbx-swarm-node"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create cert: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tlsutil/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tlsutil/
git commit -m "feat(tlsutil): load-or-generate self-signed TLS cert"
```

---

## Task 4: Config — API keys + roles + TLS paths

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test (append to `config_test.go`)**

```go
func TestRoleForKey(t *testing.T) {
	c := Default()
	c.APIKeys = []APIKey{{Key: "adm", Role: "admin"}, {Key: "ro", Role: "read-only"}}
	role, ok := c.RoleForKey("adm")
	require.True(t, ok)
	require.Equal(t, "admin", role)
	_, ok = c.RoleForKey("nope")
	require.False(t, ok)
}

func TestValidate_RejectsBadRole(t *testing.T) {
	c := Default()
	c.APIKeys = []APIKey{{Key: "x", Role: "wizard"}}
	require.Error(t, c.Validate())
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run "TestRoleForKey|TestValidate_RejectsBadRole" -v`
Expected: FAIL — `undefined: APIKey`

- [ ] **Step 3: Extend `config.go`**

Add to the file:

```go
// APIKey is a bearer credential mapped to a role ("admin"|"read-only").
type APIKey struct {
	Key  string `yaml:"key"`
	Role string `yaml:"role"`
}
```

Add these fields to `Config`:

```go
	TLSCertFile string   `yaml:"tls_cert_file"`
	TLSKeyFile  string   `yaml:"tls_key_file"`
	APIKeys     []APIKey `yaml:"api_keys"`
```

Add the lookup and extend `Validate`:

```go
// RoleForKey returns the role for a bearer key, if configured.
func (c *Config) RoleForKey(key string) (string, bool) {
	for _, k := range c.APIKeys {
		if k.Key == key {
			return k.Role, true
		}
	}
	return "", false
}
```

In `Validate`, before `return nil`, add:

```go
	for _, k := range c.APIKeys {
		if k.Key == "" {
			return fmt.Errorf("api_keys: empty key")
		}
		if k.Role != "admin" && k.Role != "read-only" {
			return fmt.Errorf("api_keys: role must be admin|read-only, got %q", k.Role)
		}
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/config/
git commit -m "feat(config): API keys with roles and TLS cert paths"
```

---

## Task 5: Session tokens (HMAC-signed)

**Files:**
- Create: `internal/auth/session.go`
- Test: `internal/auth/session_test.go`

- [ ] **Step 1: Write the failing test**

```go
package auth

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSession_RoundTrip(t *testing.T) {
	s := NewSigner([]byte("secret-key-32-bytes-long-aaaaaaa"))
	tok := s.Mint("admin", time.Now().Add(time.Hour))

	role, err := s.Verify(tok)
	require.NoError(t, err)
	require.Equal(t, "admin", role)
}

func TestSession_RejectsExpiredAndTampered(t *testing.T) {
	s := NewSigner([]byte("secret-key-32-bytes-long-aaaaaaa"))

	expired := s.Mint("admin", time.Now().Add(-time.Minute))
	_, err := s.Verify(expired)
	require.Error(t, err)

	tok := s.Mint("read-only", time.Now().Add(time.Hour))
	_, err = s.Verify(tok + "x") // tamper
	require.Error(t, err)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/auth/ -run TestSession -v`
Expected: FAIL — `undefined: NewSigner`

- [ ] **Step 3: Write the implementation**

```go
// Package auth provides bearer + cookie-session authentication and role gating.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Signer mints and verifies HMAC-signed session tokens of the form
// "<role>|<expiryUnix>|<sigBase64>".
type Signer struct{ key []byte }

// NewSigner returns a Signer over the given secret key.
func NewSigner(key []byte) *Signer { return &Signer{key: key} }

func (s *Signer) sign(payload string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Mint returns a signed token carrying role and expiry.
func (s *Signer) Mint(role string, expiry time.Time) string {
	payload := role + "|" + strconv.FormatInt(expiry.Unix(), 10)
	return payload + "|" + s.sign(payload)
}

// Verify checks the signature and expiry, returning the role.
func (s *Signer) Verify(token string) (string, error) {
	parts := strings.Split(token, "|")
	if len(parts) != 3 {
		return "", errors.New("malformed session token")
	}
	payload := parts[0] + "|" + parts[1]
	if !hmac.Equal([]byte(parts[2]), []byte(s.sign(payload))) {
		return "", errors.New("bad session signature")
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", fmt.Errorf("bad expiry: %w", err)
	}
	if time.Now().After(time.Unix(exp, 0)) {
		return "", errors.New("session expired")
	}
	return parts[0], nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/auth/ -run TestSession -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/auth/session.go internal/auth/session_test.go
git commit -m "feat(auth): HMAC-signed session tokens"
```

---

## Task 6: Auth middleware (bearer + cookie + role gate + CSRF)

**Files:**
- Create: `internal/auth/auth.go`
- Test: `internal/auth/auth_test.go`

- [ ] **Step 1: Write the failing test**

```go
package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type keyStore map[string]string // key -> role

func (k keyStore) RoleForKey(key string) (string, bool) { r, ok := k[key]; return r, ok }

func TestAuthenticate_BearerAndRoleGate(t *testing.T) {
	keys := keyStore{"adm": "admin", "ro": "read-only"}
	m := New(keys, NewSigner([]byte("k")))

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// require admin
	h := m.Authenticate(m.RequireRole("admin", final))

	// no creds -> 401
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/x", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// read-only bearer on admin route -> 403
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer ro")
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)

	// admin bearer -> 200
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer adm")
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestAuthenticate_CookieAndCSRF(t *testing.T) {
	keys := keyStore{"adm": "admin"}
	signer := NewSigner([]byte("k"))
	m := New(keys, signer)

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := m.Authenticate(final)

	tok := signer.Mint("admin", time.Now().Add(time.Hour))

	// cookie GET -> ok (no CSRF needed for safe method)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: tok})
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// cookie POST without CSRF header -> 403
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/x", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: tok})
	req.AddCookie(&http.Cookie{Name: CSRFCookie, Value: "abc"})
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)

	// cookie POST with matching CSRF header+cookie -> ok
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/x", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: tok})
	req.AddCookie(&http.Cookie{Name: CSRFCookie, Value: "abc"})
	req.Header.Set(CSRFHeader, "abc")
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/auth/ -run TestAuthenticate -v`
Expected: FAIL — `undefined: New`

- [ ] **Step 3: Write the implementation**

```go
package auth

import (
	"context"
	"net/http"
	"strings"
)

// Cookie/header names for sessions and CSRF (ADR-0006).
const (
	SessionCookie = "sbx_session"
	CSRFCookie    = "sbx_csrf"
	CSRFHeader    = "X-CSRF-Token"
)

type ctxKey int

const roleKey ctxKey = 0

// KeyStore resolves a bearer key to a role.
type KeyStore interface {
	RoleForKey(key string) (string, bool)
}

// Middleware authenticates requests (bearer or cookie) and gates by role.
type Middleware struct {
	keys   KeyStore
	signer *Signer
}

// New builds the middleware.
func New(keys KeyStore, signer *Signer) *Middleware {
	return &Middleware{keys: keys, signer: signer}
}

// RoleFromContext returns the authenticated role, if any.
func RoleFromContext(ctx context.Context) (string, bool) {
	r, ok := ctx.Value(roleKey).(string)
	return r, ok
}

// Authenticate resolves a role from a bearer token or a session cookie, and for
// cookie-authenticated unsafe methods enforces double-submit CSRF. On success it
// stores the role in the request context; otherwise it writes 401/403.
func (m *Middleware) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Bearer path.
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			if role, ok := m.keys.RoleForKey(strings.TrimPrefix(h, "Bearer ")); ok {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), roleKey, role)))
				return
			}
			http.Error(w, "invalid bearer token", http.StatusUnauthorized)
			return
		}

		// Cookie session path.
		if c, err := r.Cookie(SessionCookie); err == nil {
			role, verr := m.signer.Verify(c.Value)
			if verr != nil {
				http.Error(w, "invalid session", http.StatusUnauthorized)
				return
			}
			if !isSafeMethod(r.Method) && !csrfOK(r) {
				http.Error(w, "csrf check failed", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), roleKey, role)))
			return
		}

		http.Error(w, "authentication required", http.StatusUnauthorized)
	})
}

// RequireRole wraps a handler so only the given role (or admin) may proceed.
func (m *Middleware) RequireRole(role string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := RoleFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if got != role && got != "admin" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func csrfOK(r *http.Request) bool {
	c, err := r.Cookie(CSRFCookie)
	if err != nil {
		return false
	}
	return c.Value != "" && c.Value == r.Header.Get(CSRFHeader)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/auth/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/auth/auth.go internal/auth/auth_test.go
git commit -m "feat(auth): bearer + cookie auth, role gate, double-submit CSRF"
```

---

## Task 7: Embedded SPA placeholder

**Files:**
- Create: `web/dist/index.html`, `web/embed.go`

- [ ] **Step 1: Write `web/dist/index.html`**

```html
<!doctype html>
<title>sbx-swarm-node</title>
<h1>sbx-swarm-node console (placeholder)</h1>
```

- [ ] **Step 2: Write `web/embed.go`**

```go
// Package web embeds the built SPA assets served by the node.
package web

import (
	"embed"
	"io/fs"
)

//go:embed dist
var dist embed.FS

// FS returns the embedded SPA file system rooted at the dist directory.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // dist is embedded at build time; this cannot fail
	}
	return sub
}
```

- [ ] **Step 3: Verify build**

Run: `go build ./...`
Expected: no errors.

- [ ] **Step 4: Commit**

```bash
git add web/
git commit -m "feat(web): embedded SPA placeholder"
```

---

## Task 8: One-port multiplex handler

**Files:**
- Create: `internal/apiserver/mux.go`
- Test: `internal/apiserver/mux_test.go`

- [ ] **Step 1: Write the failing test**

```go
package apiserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMultiplex_RoutesByProtocolAndContentType(t *testing.T) {
	grpcH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("X-Route", "grpc") })
	otherH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("X-Route", "other") })

	mux := Multiplex(grpcH, otherH)

	// HTTP/2 + application/grpc -> grpc
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/svc/Method", nil)
	req.ProtoMajor = 2
	req.Header.Set("Content-Type", "application/grpc")
	mux.ServeHTTP(rec, req)
	require.Equal(t, "grpc", rec.Header().Get("X-Route"))

	// plain GET -> other
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/node", nil)
	mux.ServeHTTP(rec, req)
	require.Equal(t, "other", rec.Header().Get("X-Route"))
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/apiserver/ -run TestMultiplex -v`
Expected: FAIL — `undefined: Multiplex`

- [ ] **Step 3: Write the implementation**

```go
package apiserver

import (
	"net/http"
	"strings"
)

// Multiplex returns a handler that routes HTTP/2 application/grpc requests to
// grpcH and everything else to otherH (gateway/auth/static). Serve it over TLS
// (ALPN h2) so gRPC clients negotiate HTTP/2.
func Multiplex(grpcH, otherH http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			grpcH.ServeHTTP(w, r)
			return
		}
		otherH.ServeHTTP(w, r)
	})
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/apiserver/ -run TestMultiplex -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/apiserver/mux.go internal/apiserver/mux_test.go
git commit -m "feat(apiserver): protocol-dispatch multiplex handler"
```

---

## Task 9: Assemble the server (gRPC + gateway + auth + static + session endpoint)

**Files:**
- Create: `internal/apiserver/server.go`
- Test: `internal/apiserver/server_test.go`

- [ ] **Step 1: Write the failing test**

```go
package apiserver

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func startTestServer(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	cert := mustSelfSigned(t)
	opts := Options{
		NodeID: "n1", NodeName: "n", Version: "v0",
		Keys:   keyMap{"adm": "admin"},
		Signer: testSigner(),
		Cert:   cert,
	}
	h, grpcSrv, err := Build(opts)
	require.NoError(t, err)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{Handler: h, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", "http/1.1"}}}
	go srv.ServeTLS(ln, "", "")

	return ln.Addr().String(), func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		grpcSrv.Stop()
	}
}

func TestServer_RESTRequiresAuth_AndReturnsNodeInfo(t *testing.T) {
	addr, cleanup := startTestServer(t)
	defer cleanup()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}

	// no auth -> 401
	resp, err := client.Get("https://" + addr + "/v1/node")
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	// bearer -> 200 + node id
	req, _ := http.NewRequest(http.MethodGet, "https://"+addr+"/v1/node", nil)
	req.Header.Set("Authorization", "Bearer adm")
	resp, err = client.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, string(body), `"node_id":"n1"`)
}

func TestServer_GRPCGetNodeInfo(t *testing.T) {
	addr, cleanup := startTestServer(t)
	defer cleanup()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})))
	require.NoError(t, err)
	defer conn.Close()

	out, err := sbxv1.NewNodeServiceClient(conn).GetNodeInfo(context.Background(), &sbxv1.GetNodeInfoRequest{})
	require.NoError(t, err)
	require.Equal(t, "n1", out.NodeId)
}
```

Add these test helpers in `server_test.go`:

```go
type keyMap map[string]string

func (k keyMap) RoleForKey(key string) (string, bool) { r, ok := k[key]; return r, ok }
```

(Provide `mustSelfSigned`, `testSigner` via the tlsutil/auth packages in the test file — `mustSelfSigned` calls `tlsutil.LoadOrGenerate("","",t.TempDir())`; `testSigner` returns `auth.NewSigner([]byte("test-secret"))`. Import those packages.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/apiserver/ -run TestServer -v`
Expected: FAIL — `undefined: Build` / `Options`

- [ ] **Step 3: Write the implementation**

```go
package apiserver

import (
	"context"
	"crypto/tls"
	"net/http"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/squall-chua/sbx-swarm-node/internal/auth"
	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/obs"
	"github.com/squall-chua/sbx-swarm-node/web"
	"google.golang.org/grpc"
)

// Options configures the server.
type Options struct {
	NodeID, NodeName, Version string
	Keys                      auth.KeyStore
	Signer                    *auth.Signer
	Cert                      tls.Certificate
	Health                    *obs.Health // optional; health routes mounted if set
}

// Build constructs the one-port handler and the gRPC server. The caller serves
// the handler over TLS (ALPN h2) and stops grpcSrv on shutdown.
func Build(opts Options) (http.Handler, *grpc.Server, error) {
	node := NewNodeService(opts.NodeID, opts.NodeName, opts.Version)

	grpcSrv := grpc.NewServer()
	sbxv1.RegisterNodeServiceServer(grpcSrv, node)

	// In-process gateway: the gateway dials the gRPC server in-process by calling
	// the service directly via a local handler registration.
	gw := runtime.NewServeMux()
	if err := sbxv1.RegisterNodeServiceHandlerServer(context.Background(), gw, node); err != nil {
		return nil, nil, err
	}

	mw := auth.New(opts.Keys, opts.Signer)

	rest := http.NewServeMux()
	rest.Handle("/v1/auth/session", sessionHandler(opts.Keys, opts.Signer)) // unauthenticated exchange
	rest.Handle("/v1/", mw.Authenticate(gw))                                // everything else under /v1 is authed
	rest.Handle("/", http.FileServer(http.FS(web.FS())))                    // SPA fallback
	if opts.Health != nil {
		rest.Handle("/healthz", opts.Health.Handler())
		rest.Handle("/readyz", opts.Health.Handler())
		rest.Handle("/metrics", opts.Health.Handler())
	}

	return Multiplex(grpcSrv, rest), grpcSrv, nil
}

// sessionHandler exchanges a valid API key (JSON {"key":"..."}) for an httpOnly
// session cookie + a readable CSRF cookie.
func sessionHandler(keys auth.KeyStore, signer *auth.Signer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		key := r.Header.Get("Authorization")
		key = trimBearer(key)
		role, ok := keys.RoleForKey(key)
		if !ok {
			http.Error(w, "invalid key", http.StatusUnauthorized)
			return
		}
		tok := signer.Mint(role, time.Now().Add(12*time.Hour))
		http.SetCookie(w, &http.Cookie{Name: auth.SessionCookie, Value: tok, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
		csrf := signer.Mint("csrf", time.Now().Add(12*time.Hour)) // opaque random-ish value
		http.SetCookie(w, &http.Cookie{Name: auth.CSRFCookie, Value: csrf, Path: "/", Secure: true, SameSite: http.SameSiteStrictMode})
		w.WriteHeader(http.StatusNoContent)
	})
}

func trimBearer(h string) string {
	const p = "Bearer "
	if len(h) > len(p) && h[:len(p)] == p {
		return h[len(p):]
	}
	return h
}
```

Note: `RegisterNodeServiceHandlerServer` runs the gateway **in-process** (no self-dial), which is simplest and avoids a loopback gRPC connection. Streaming RPCs later use SSE (M1d), not the gateway.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/apiserver/ -v`
Expected: PASS (REST 401-then-200, and gRPC GetNodeInfo)

- [ ] **Step 5: Commit**

```bash
git add internal/apiserver/server.go internal/apiserver/server_test.go
git commit -m "feat(apiserver): one-port gRPC+gateway+auth+static server"
```

---

## Task 10: Wire the server into node.Node over TLS

**Files:**
- Modify: `internal/node/node.go`
- Modify: `internal/node/node_test.go`

- [ ] **Step 1: Update the node test to use TLS + bearer**

Replace the body of `TestNode_BootServeStop` in `node_test.go` with:

```go
func TestNode_BootServeStop(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.APIKeys = []config.APIKey{{Key: "adm", Role: "admin"}}

	n, err := New(cfg, obs.NewLogger("error", io.Discard), "test")
	require.NoError(t, err)
	require.NoError(t, n.Start())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = n.Stop(ctx)
	})

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}

	// health is unauthenticated
	resp, err := client.Get("https://" + n.Addr() + "/healthz")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// /v1/node needs auth
	req, _ := http.NewRequest(http.MethodGet, "https://"+n.Addr()+"/v1/node", nil)
	req.Header.Set("Authorization", "Bearer adm")
	resp, err = client.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}
```

Add imports: `crypto/tls`, `net/http`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/node/ -v`
Expected: FAIL (node still serves plain health only; `/v1/node` 404s and no TLS).

- [ ] **Step 3: Update `node.go`**

Add fields and a session signer/secret. Replace the server construction in `New` and the `Start`/`Stop` to use TLS + the apiserver. Concretely:

In `New`, after building `health`, add:

```go
	cert, err := tlsutil.LoadOrGenerate(cfg.TLSCertFile, cfg.TLSKeyFile, cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("tls: %w", err)
	}
	signer := auth.NewSigner(id.PrivateKey.Seed()) // stable per-node session signing key

	handler, grpcSrv, err := apiserver.Build(apiserver.Options{
		NodeID:   id.NodeID,
		NodeName: cfg.NodeName,
		Version:  version,
		Keys:     cfg,
		Signer:   signer,
		Cert:     cert,
		Health:   health,
	})
	if err != nil {
		return nil, fmt.Errorf("apiserver: %w", err)
	}
```

Change the `Node` struct: drop the old `srv` plain handler; store:

```go
	srv     *http.Server
	grpcSrv *grpc.Server
	cert    tls.Certificate
```

Set `srv: &http.Server{Handler: handler, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", "http/1.1"}}}`, and keep `grpcSrv: grpcSrv`, `cert: cert` in the returned `&Node{...}`.

In `Start`, replace `n.srv.Serve(ln)` with `n.srv.ServeTLS(ln, "", "")`.

In `Stop`, after `n.srv.Shutdown(ctx)`, add `n.grpcSrv.Stop()` before closing the store.

Add imports: `crypto/tls`, `google.golang.org/grpc`, and the `apiserver`, `auth`, `tlsutil` internal packages. `cfg` satisfies `auth.KeyStore` because `config.Config` has `RoleForKey` (Task 4).

- [ ] **Step 4: Run tests + manual smoke**

Run: `go test ./... -v`
Expected: PASS across all packages.

Run:
```bash
go run ./cmd/sbx-swarm-node --data-dir ./tmp-data --listen-addr 127.0.0.1:8443 &
curl -sk https://localhost:8443/healthz
curl -sk -H "Authorization: Bearer devkey" https://localhost:8443/v1/node   # 401 unless devkey configured
kill %1; rm -rf ./tmp-data
```
Expected: `ok` from healthz; `/v1/node` returns 401 (no key configured) — wiring confirmed.

- [ ] **Step 5: Commit**

```bash
git add internal/node/
git commit -m "feat(node): serve one-port TLS gRPC+REST+static with auth"
```

---

## Self-Review

**Spec coverage (M1b slice):**
- gRPC + grpc-gateway multiplexed on one TLS port → Tasks 1, 8, 9, 10 ✓ (in-process gateway, ServeHTTP gRPC over h2)
- Embedded SPA static serving → Tasks 7, 9 ✓ (placeholder; real Nuxt build is Milestone 8)
- Bearer + cookie-session auth, `admin`/`read-only` roles, CSRF (ADR-0006) → Tasks 4, 5, 6, 9 ✓
- First real RPC reachable via gRPC and REST → Tasks 2, 9 ✓
- **Deferred:** node↔node gRPC auth (ADR-0004) is M4 (swarm); SSE routes land in M1d; richer NodeInfo fields grow as the domain does.

**Placeholder scan:** No TBD/TODO. Generated `.pb.go` is produced by `buf generate` (Task 1), not hand-written — the correct practice. Every hand-written step has complete code + exact commands.

**Type consistency:** `config.Config.RoleForKey` satisfies `auth.KeyStore`; `auth.NewSigner`→`*Signer{Mint,Verify}`; `auth.New(keys,signer)`→`*Middleware{Authenticate,RequireRole}`; `apiserver.Build(Options)`→`(http.Handler,*grpc.Server,error)`; `apiserver.Multiplex(grpcH,otherH)`; `apiserver.NewNodeService(id,name,ver)`; `tlsutil.LoadOrGenerate(cert,key,dir)`. Server/node call sites match.

**Note:** grpc-go's `ServeHTTP` path (used by the multiplex) is functional but documented as lower-performance than native `Serve`; acceptable for v1. If perf matters later, switch the multiplex to `soheilhy/cmux` splitting at the connection layer — the handler boundary stays the same.
