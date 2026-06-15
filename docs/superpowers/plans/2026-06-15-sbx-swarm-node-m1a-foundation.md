# sbx-swarm-node M1a — Bootable Node Skeleton Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A runnable single-node `sbx-swarm-node` binary that loads layered config, establishes a persistent cryptographic node identity, opens a versioned `bbolt` store, mints self-routing IDs, and serves health/readiness/metrics — with everything unit-tested.

**Architecture:** Small, single-responsibility packages under `internal/`, wired together by a `node.Node` that a thin `cmd/sbx-swarm-node` main starts and stops on signals. No swarm, no SDK, no gRPC yet — this is the foundation later milestones build on. Decisions come from `docs/adr/0002` (self-routing IDs), `0004` (node key → `node_id`), `0005` (`schema_version`), and the persistence model in the spec §16.

**Tech Stack:** Go 1.23, `crypto/ed25519` + `crypto/sha256` (identity), `go.etcd.io/bbolt` (store), `github.com/oklog/ulid/v2` (IDs), `log/slog` (logging), `github.com/prometheus/client_golang` (metrics), `gopkg.in/yaml.v3` (config file), `github.com/stretchr/testify` (tests).

**Scope:** This is sub-plan **M1a** of Milestone 1. It deliberately excludes the SDK, gRPC/REST transport, auth, the sandbox domain, and events — those are M1b–M1d. M1a ends with a binary that boots, proves identity persistence, and answers `/healthz`, `/readyz`, `/metrics`.

---

## File Structure

| File | Responsibility |
|---|---|
| `go.mod` / `go.sum` | Module definition + deps |
| `cmd/sbx-swarm-node/main.go` | Parse flags → load config → build & start `node.Node` → wait for signal → stop |
| `internal/config/config.go` | `Config` struct, `Default()`, `Load()` (file→env→flags precedence), `Validate()` |
| `internal/config/config_test.go` | Precedence, defaults, validation tests |
| `internal/identity/identity.go` | `Identity`, `DeriveNodeID()`, `LoadOrCreate()` (Ed25519 key persisted at `<data_dir>/node.key`) |
| `internal/identity/identity_test.go` | Deterministic derivation, persistence/reuse, perms |
| `internal/ids/ids.go` | `Gen` (mints `<node_id>.<ulid>`), `Owner()` (parses prefix) |
| `internal/ids/ids_test.go` | Format, uniqueness, owner parsing |
| `internal/store/store.go` | `Store`, `Open()`, buckets, `schema_version`, forward-migration framework + downgrade guard |
| `internal/store/store_test.go` | Bucket creation, reopen, downgrade guard |
| `internal/obs/obs.go` | `NewLogger()`, `NewHealth()` (`/healthz` `/readyz` `/metrics`), `RegisterBuildInfo()` |
| `internal/obs/obs_test.go` | Handler status codes, metrics exposition |
| `internal/node/node.go` | `Node` — wires identity + store + obs server; `New`/`Start`/`Addr`/`Stop` |
| `internal/node/node_test.go` | Boot on `:0`, serve healthz, clean stop |

Module path: `github.com/squall-chua/sbx-swarm-node`.

---

## Task 1: Project scaffolding

**Files:**
- Create: `go.mod`
- Create: `cmd/sbx-swarm-node/main.go`

- [ ] **Step 1: Create `go.mod`**

```
module github.com/squall-chua/sbx-swarm-node

go 1.23

require (
	github.com/oklog/ulid/v2 v2.1.0
	github.com/prometheus/client_golang v1.20.5
	github.com/stretchr/testify v1.9.0
	go.etcd.io/bbolt v1.3.11
	gopkg.in/yaml.v3 v3.0.1
)
```

- [ ] **Step 2: Create a minimal compilable `cmd/sbx-swarm-node/main.go`**

```go
// Command sbx-swarm-node runs a single Docker-sandbox swarm node.
package main

import "fmt"

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	fmt.Println("sbx-swarm-node", version)
}
```

- [ ] **Step 3: Fetch deps and verify it builds**

Run: `go mod tidy && go build ./...`
Expected: no errors; `go.sum` is created.

- [ ] **Step 4: Verify it runs**

Run: `go run ./cmd/sbx-swarm-node`
Expected: prints `sbx-swarm-node dev`

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum cmd/sbx-swarm-node/main.go
git commit -m "chore: scaffold sbx-swarm-node module and entrypoint"
```

---

## Task 2: Config loading with precedence

**Files:**
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

```go
package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func noEnv(string) (string, bool) { return "", false }

func TestLoad_PrecedenceFlagsOverEnvOverFileOverDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("node_name: fromfile\nlog_level: warn\n"), 0o600))

	env := func(k string) (string, bool) {
		if k == "SBX_NODE_NAME" {
			return "fromenv", true
		}
		return "", false
	}

	// file sets node_name+log_level; env overrides node_name; flag overrides node_name again.
	cfg, err := Load([]string{"--config", cfgPath, "--node-name", "fromflag"}, env)
	require.NoError(t, err)
	require.Equal(t, "fromflag", cfg.NodeName) // flag wins
	require.Equal(t, "warn", cfg.LogLevel)     // from file (not overridden)
}

func TestLoad_Defaults(t *testing.T) {
	cfg, err := Load(nil, noEnv)
	require.NoError(t, err)
	require.NotEmpty(t, cfg.NodeName) // defaults to hostname
	require.Equal(t, "./data", cfg.DataDir)
	require.Equal(t, ":8443", cfg.ListenAddr)
	require.Equal(t, "info", cfg.LogLevel)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestLoad -v`
Expected: FAIL — `undefined: Load`

- [ ] **Step 3: Write the implementation**

```go
// Package config loads node configuration from defaults, an optional YAML
// file, environment variables, and command-line flags, in that increasing
// order of precedence (flags > env > file > defaults).
package config

import (
	"flag"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the M1a subset of node configuration. Later milestones extend it.
type Config struct {
	NodeName   string `yaml:"node_name"`
	DataDir    string `yaml:"data_dir"`
	ListenAddr string `yaml:"listen_addr"`
	LogLevel   string `yaml:"log_level"`
}

// Default returns the baseline configuration before any overrides.
func Default() *Config {
	host, _ := os.Hostname()
	if host == "" {
		host = "sbx-node"
	}
	return &Config{
		NodeName:   host,
		DataDir:    "./data",
		ListenAddr: ":8443",
		LogLevel:   "info",
	}
}

// Load builds a Config from defaults, an optional --config YAML file, env vars
// (SBX_ prefix), and flags. lookupEnv is injected for testability (use
// os.LookupEnv in production).
func Load(args []string, lookupEnv func(string) (string, bool)) (*Config, error) {
	cfg := Default()

	fs := flag.NewFlagSet("sbx-swarm-node", flag.ContinueOnError)
	var (
		configPath = fs.String("config", "", "path to YAML config file")
		nodeName   = fs.String("node-name", "", "human-readable node name")
		dataDir    = fs.String("data-dir", "", "directory for node key and database")
		listenAddr = fs.String("listen-addr", "", "address for the HTTP server")
		logLevel   = fs.String("log-level", "", "debug|info|warn|error")
	)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	// File layer.
	if *configPath != "" {
		raw, err := os.ReadFile(*configPath)
		if err != nil {
			return nil, fmt.Errorf("read config file: %w", err)
		}
		if err := yaml.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("parse config file: %w", err)
		}
	}

	// Env layer.
	if v, ok := lookupEnv("SBX_NODE_NAME"); ok {
		cfg.NodeName = v
	}
	if v, ok := lookupEnv("SBX_DATA_DIR"); ok {
		cfg.DataDir = v
	}
	if v, ok := lookupEnv("SBX_LISTEN_ADDR"); ok {
		cfg.ListenAddr = v
	}
	if v, ok := lookupEnv("SBX_LOG_LEVEL"); ok {
		cfg.LogLevel = v
	}

	// Flag layer (only flags the user actually set).
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "node-name":
			cfg.NodeName = *nodeName
		case "data-dir":
			cfg.DataDir = *dataDir
		case "listen-addr":
			cfg.ListenAddr = *listenAddr
		case "log-level":
			cfg.LogLevel = *logLevel
		}
	})

	return cfg, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -run TestLoad -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/config/
git commit -m "feat(config): layered config loader (file/env/flags precedence)"
```

---

## Task 3: Config validation

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test (append to `config_test.go`)**

```go
func TestValidate(t *testing.T) {
	ok := Default()
	require.NoError(t, ok.Validate())

	bad := Default()
	bad.LogLevel = "loud"
	require.Error(t, bad.Validate())

	empty := Default()
	empty.DataDir = ""
	require.Error(t, empty.Validate())
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestValidate -v`
Expected: FAIL — `cfg.Validate undefined`

- [ ] **Step 3: Add `Validate` to `config.go`**

```go
// Validate checks the configuration for obvious mistakes.
func (c *Config) Validate() error {
	if c.NodeName == "" {
		return fmt.Errorf("node_name must not be empty")
	}
	if c.DataDir == "" {
		return fmt.Errorf("data_dir must not be empty")
	}
	if c.ListenAddr == "" {
		return fmt.Errorf("listen_addr must not be empty")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level must be one of debug|info|warn|error, got %q", c.LogLevel)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -v`
Expected: PASS (all config tests)

- [ ] **Step 5: Commit**

```bash
git add internal/config/
git commit -m "feat(config): add Validate"
```

---

## Task 4: Node identity (Ed25519 key → node_id)

Implements ADR-0004 (`node_id = short-hash(pubkey)`) and the persisted-key requirement (spec §16).

**Files:**
- Create: `internal/identity/identity.go`
- Test: `internal/identity/identity_test.go`

- [ ] **Step 1: Write the failing test**

```go
package identity

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeriveNodeID_DeterministicAndFormatted(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize) // all-zero seed → fixed key
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)

	id1 := DeriveNodeID(pub)
	id2 := DeriveNodeID(pub)
	require.Equal(t, id1, id2)        // deterministic
	require.Len(t, id1, 16)           // 10 bytes base32-no-pad = 16 chars
	require.Equal(t, id1, strings.ToLower(id1)) // lowercase
}

func TestLoadOrCreate_PersistsAndReuses(t *testing.T) {
	dir := t.TempDir()

	a, err := LoadOrCreate(dir)
	require.NoError(t, err)
	require.NotEmpty(t, a.NodeID)

	info, err := os.Stat(filepath.Join(dir, "node.key"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	b, err := LoadOrCreate(dir) // second load reuses the key
	require.NoError(t, err)
	require.Equal(t, a.NodeID, b.NodeID)
}
```

Add `"strings"` to the test imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/identity/ -v`
Expected: FAIL — `undefined: DeriveNodeID` / `LoadOrCreate`

- [ ] **Step 3: Write the implementation**

```go
// Package identity manages the node's persistent Ed25519 keypair and derives
// the node_id from its public key (ADR-0004). The key is critical, irreplaceable
// state: losing it gives the node a new identity and orphans its sandboxes.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const keyFileName = "node.key"

// Identity is the node's keypair plus its derived node_id.
type Identity struct {
	PublicKey  ed25519.PublicKey
	PrivateKey ed25519.PrivateKey
	NodeID     string
}

var idEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// DeriveNodeID computes the node_id as the lowercase base32 of the first 10
// bytes of SHA-256(pubkey) — a short, self-certifying identifier.
func DeriveNodeID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return strings.ToLower(idEncoding.EncodeToString(sum[:10]))
}

// LoadOrCreate loads the node key from <dir>/node.key, generating and
// persisting a new one (0600) if absent.
func LoadOrCreate(dir string) (*Identity, error) {
	path := filepath.Join(dir, keyFileName)
	seed, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		seed = make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, fmt.Errorf("generate node key: %w", err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
		if err := os.WriteFile(path, seed, 0o600); err != nil {
			return nil, fmt.Errorf("write node key: %w", err)
		}
	case err != nil:
		return nil, fmt.Errorf("read node key: %w", err)
	case len(seed) != ed25519.SeedSize:
		return nil, fmt.Errorf("node key file %s has wrong size %d", path, len(seed))
	}

	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	return &Identity{PublicKey: pub, PrivateKey: priv, NodeID: DeriveNodeID(pub)}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/identity/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/identity/
git commit -m "feat(identity): persistent ed25519 key and node_id derivation (ADR-0004)"
```

---

## Task 5: Self-routing ID generator

Implements ADR-0002 (`<node_id>.<ulid>`).

**Files:**
- Create: `internal/ids/ids.go`
- Test: `internal/ids/ids_test.go`

- [ ] **Step 1: Write the failing test**

```go
package ids

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGen_FormatAndUniqueness(t *testing.T) {
	g := NewGen("node123")

	a := g.Sandbox()
	b := g.Sandbox()
	require.NotEqual(t, a, b) // monotonic ULIDs differ

	owner, ok := Owner(a)
	require.True(t, ok)
	require.Equal(t, "node123", owner)

	op := g.Op()
	o2, ok := Owner(op)
	require.True(t, ok)
	require.Equal(t, "node123", o2)
}

func TestOwner_Invalid(t *testing.T) {
	_, ok := Owner("noprefix")
	require.False(t, ok)
	_, ok = Owner("")
	require.False(t, ok)
	_, ok = Owner("trailingdot.")
	require.False(t, ok)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ids/ -v`
Expected: FAIL — `undefined: NewGen`

- [ ] **Step 3: Write the implementation**

```go
// Package ids mints self-routing identifiers of the form <node_id>.<ulid>,
// where the prefix names the owning node so any peer can route without a
// lookup (ADR-0002).
package ids

import (
	"crypto/rand"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// Gen mints IDs prefixed with a fixed node_id. Safe for concurrent use.
type Gen struct {
	node    string
	mu      sync.Mutex
	entropy *ulid.MonotonicEntropy
}

// NewGen returns a generator that prefixes IDs with nodeID.
func NewGen(nodeID string) *Gen {
	return &Gen{node: nodeID, entropy: ulid.Monotonic(rand.Reader, 0)}
}

func (g *Gen) newULID() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return ulid.MustNew(ulid.Timestamp(time.Now()), g.entropy).String()
}

// Sandbox returns a new self-routing sandbox ID.
func (g *Gen) Sandbox() string { return g.node + "." + g.newULID() }

// Op returns a new self-routing operation ID.
func (g *Gen) Op() string { return g.node + "." + g.newULID() }

// Owner extracts the node_id prefix from a self-routing ID.
func Owner(id string) (string, bool) {
	i := strings.IndexByte(id, '.')
	if i <= 0 || i == len(id)-1 {
		return "", false
	}
	return id[:i], true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/ids/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/ids/
git commit -m "feat(ids): self-routing id generator (ADR-0002)"
```

---

## Task 6: bbolt store with schema version + downgrade guard

Implements the persistence model (spec §16) and the `schema_version` forward-migration + downgrade guard (ADR-0005/0009).

**Files:**
- Create: `internal/store/store.go`
- Test: `internal/store/store_test.go`

- [ ] **Step 1: Write the failing test**

```go
package store

import (
	"encoding/binary"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
)

func TestOpen_CreatesBucketsAndSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.db")

	s, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	require.NoError(t, s.DB().View(func(tx *bolt.Tx) error {
		for _, b := range []string{"meta", "sandboxes", "operations", "idempotency", "blocked_egress", "audit"} {
			require.NotNil(t, tx.Bucket([]byte(b)), "bucket %s", b)
		}
		return nil
	}))
}

func TestOpen_ReopenSucceeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.db")
	s1, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, s1.Close())

	s2, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, s2.Close())
}

func TestOpen_DowngradeGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.db")
	s, err := Open(path)
	require.NoError(t, err)

	// Forge a future schema version on disk.
	require.NoError(t, s.DB().Update(func(tx *bolt.Tx) error {
		future := make([]byte, 8)
		binary.BigEndian.PutUint64(future, 999)
		return tx.Bucket([]byte("meta")).Put([]byte("schema_version"), future)
	}))
	require.NoError(t, s.Close())

	_, err = Open(path)
	require.ErrorContains(t, err, "newer")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -v`
Expected: FAIL — `undefined: Open`

- [ ] **Step 3: Write the implementation**

```go
// Package store is the node's durable state in a single bbolt file. It owns
// schema versioning: fresh databases start at SchemaVersion, older ones are
// migrated forward, and a database newer than the binary is refused
// (downgrade guard).
package store

import (
	"encoding/binary"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// SchemaVersion is the schema this binary understands.
const SchemaVersion uint64 = 1

var (
	bucketNames    = []string{"meta", "sandboxes", "operations", "idempotency", "blocked_egress", "audit"}
	metaBucket     = []byte("meta")
	schemaKey      = []byte("schema_version")
	bucketMeta     = []byte("meta")
)

// migration migrates the database from version i to i+1. migrations[i-1] takes
// the store from version i to i+1. Empty until a v2 schema exists.
var migrations = []func(tx *bolt.Tx) error{}

// Store wraps the bbolt database.
type Store struct {
	db *bolt.DB
}

// DB exposes the underlying handle for later packages and tests.
func (s *Store) DB() *bolt.DB { return s.db }

// Open opens (or creates) the database at path, ensures all buckets exist, and
// reconciles the schema version.
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("open bbolt: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		for _, name := range bucketNames {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return fmt.Errorf("create bucket %s: %w", name, err)
			}
		}
		meta := tx.Bucket(bucketMeta)

		raw := meta.Get(schemaKey)
		if raw == nil { // fresh database
			return putUint64(meta, schemaKey, SchemaVersion)
		}

		v := binary.BigEndian.Uint64(raw)
		if v > SchemaVersion {
			return fmt.Errorf("on-disk schema v%d is newer than supported v%d (downgrade guard)", v, SchemaVersion)
		}
		for v < SchemaVersion {
			if int(v-1) >= len(migrations) {
				return fmt.Errorf("missing migration from v%d", v)
			}
			if err := migrations[v-1](tx); err != nil {
				return fmt.Errorf("migrate v%d: %w", v, err)
			}
			v++
		}
		return putUint64(meta, schemaKey, SchemaVersion)
	})
}

func putUint64(b *bolt.Bucket, key []byte, v uint64) error {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, v)
	return b.Put(key, buf)
}
```

Note: the unused `metaBucket` alias is intentional naming clarity — if `go vet`/lint flags it, delete the `metaBucket` line and keep `bucketMeta`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store/
git commit -m "feat(store): bbolt store with buckets, schema version, downgrade guard"
```

---

## Task 7: Observability — logger, metrics, health endpoints

**Files:**
- Create: `internal/obs/obs.go`
- Test: `internal/obs/obs_test.go`

- [ ] **Step 1: Write the failing test**

```go
package obs

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestHealth_Endpoints(t *testing.T) {
	reg := prometheus.NewRegistry()
	RegisterBuildInfo(reg, "test")
	h := NewHealth(reg)
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)

	// healthz always ok
	resp, err := http.Get(srv.URL + "/healthz")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// readyz 503 until ready
	resp, err = http.Get(srv.URL + "/readyz")
	require.NoError(t, err)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	resp.Body.Close()

	h.SetReady(true)
	resp, err = http.Get(srv.URL + "/readyz")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// metrics exposes our build_info
	resp, err = http.Get(srv.URL + "/metrics")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Contains(t, string(body), "sbx_build_info")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/obs/ -v`
Expected: FAIL — `undefined: NewHealth`

- [ ] **Step 3: Write the implementation**

```go
// Package obs provides logging, Prometheus metrics, and the health/readiness
// HTTP endpoints.
package obs

import (
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewLogger returns a JSON slog logger at the given level ("debug"|"info"|
// "warn"|"error", defaulting to info).
func NewLogger(level string, w io.Writer) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lv}))
}

// RegisterBuildInfo registers a constant gauge carrying the build version.
func RegisterBuildInfo(reg prometheus.Registerer, version string) {
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "sbx_build_info",
		Help:        "Build information; constant 1.",
		ConstLabels: prometheus.Labels{"version": version},
	})
	g.Set(1)
	reg.MustRegister(g)
}

// Health serves /healthz, /readyz, and /metrics.
type Health struct {
	ready atomic.Bool
	reg   *prometheus.Registry
}

// NewHealth builds a Health backed by the given registry.
func NewHealth(reg *prometheus.Registry) *Health {
	return &Health{reg: reg}
}

// SetReady marks the node ready (or not) for /readyz.
func (h *Health) SetReady(v bool) { h.ready.Store(v) }

// Handler returns the HTTP handler exposing the endpoints.
func (h *Health) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if h.ready.Load() {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ready")
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "not ready")
	})
	mux.Handle("/metrics", promhttp.HandlerFor(h.reg, promhttp.HandlerOpts{}))
	return mux
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/obs/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/obs/
git commit -m "feat(obs): slog logger, prometheus metrics, health endpoints"
```

---

## Task 8: Node wiring + main + graceful shutdown

**Files:**
- Create: `internal/node/node.go`
- Test: `internal/node/node_test.go`
- Modify: `cmd/sbx-swarm-node/main.go`

- [ ] **Step 1: Write the failing test**

```go
package node

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/config"
	"github.com/squall-chua/sbx-swarm-node/internal/obs"
	"github.com/stretchr/testify/require"
)

func TestNode_BootServeStop(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ListenAddr = "127.0.0.1:0" // random free port

	n, err := New(cfg, obs.NewLogger("error", io.Discard), "test")
	require.NoError(t, err)

	require.NoError(t, n.Start())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = n.Stop(ctx)
	})

	require.NotEmpty(t, n.NodeID())

	resp, err := http.Get("http://" + n.Addr() + "/healthz")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	resp, err = http.Get("http://" + n.Addr() + "/readyz")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode) // Start sets ready
	resp.Body.Close()
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/node/ -v`
Expected: FAIL — `undefined: New`

- [ ] **Step 3: Write `internal/node/node.go`**

```go
// Package node wires the M1a components — identity, store, observability —
// into a startable, stoppable node serving the health/metrics endpoints.
package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/squall-chua/sbx-swarm-node/internal/config"
	"github.com/squall-chua/sbx-swarm-node/internal/identity"
	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/obs"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
)

// Node is a single standalone node (M1a scope).
type Node struct {
	cfg    *config.Config
	log    *slog.Logger
	id     *identity.Identity
	ids    *ids.Gen
	store  *store.Store
	health *obs.Health
	srv    *http.Server
	ln     net.Listener
}

// New constructs a node: it establishes identity and opens the store, but does
// not listen yet.
func New(cfg *config.Config, log *slog.Logger, version string) (*Node, error) {
	id, err := identity.LoadOrCreate(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	log = log.With("node_id", id.NodeID, "node_name", cfg.NodeName)

	st, err := store.Open(filepath.Join(cfg.DataDir, "node.db"))
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}

	reg := prometheus.NewRegistry()
	obs.RegisterBuildInfo(reg, version)
	health := obs.NewHealth(reg)

	return &Node{
		cfg:    cfg,
		log:    log,
		id:     id,
		ids:    ids.NewGen(id.NodeID),
		store:  st,
		health: health,
		srv:    &http.Server{Handler: health.Handler()},
	}, nil
}

// NodeID returns this node's identifier.
func (n *Node) NodeID() string { return n.id.NodeID }

// Addr returns the actual listen address (valid after Start).
func (n *Node) Addr() string {
	if n.ln == nil {
		return n.cfg.ListenAddr
	}
	return n.ln.Addr().String()
}

// Start binds the listener and serves in the background, then marks ready.
func (n *Node) Start() error {
	ln, err := net.Listen("tcp", n.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", n.cfg.ListenAddr, err)
	}
	n.ln = ln
	go func() {
		if err := n.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			n.log.Error("http server stopped", "err", err)
		}
	}()
	n.health.SetReady(true)
	n.log.Info("node serving", "addr", n.Addr())
	return nil
}

// Stop gracefully shuts the server and closes the store.
func (n *Node) Stop(ctx context.Context) error {
	n.health.SetReady(false)
	err := n.srv.Shutdown(ctx)
	if cerr := n.store.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/node/ -v`
Expected: PASS

- [ ] **Step 5: Rewrite `cmd/sbx-swarm-node/main.go` to wire it all**

```go
// Command sbx-swarm-node runs a single Docker-sandbox swarm node.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/config"
	"github.com/squall-chua/sbx-swarm-node/internal/node"
	"github.com/squall-chua/sbx-swarm-node/internal/obs"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Args[1:], os.LookupEnv)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	log := obs.NewLogger(cfg.LogLevel, os.Stderr)
	n, err := node.New(cfg, log, version)
	if err != nil {
		return err
	}
	if err := n.Start(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	log.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return n.Stop(shutCtx)
}
```

- [ ] **Step 6: Verify build, full tests, and manual smoke**

Run: `go build ./... && go test ./...`
Expected: PASS across all packages.

Run: `go run ./cmd/sbx-swarm-node --data-dir ./tmp-data --listen-addr 127.0.0.1:8443 &` then `curl -s localhost:8443/healthz` then `curl -s localhost:8443/readyz`
Expected: `ok` and `ready`. Stop with `kill %1`. (Clean up: `rm -rf ./tmp-data`.)

- [ ] **Step 7: Commit**

```bash
git add internal/node/ cmd/sbx-swarm-node/main.go
git commit -m "feat(node): wire identity, store, observability into bootable node"
```

---

## Self-Review

**Spec coverage (M1a slice):**
- Project layout → Task 1 + the File Structure map ✓
- Config file+env+flags precedence → Tasks 2–3 ✓ (hot-reload/SIGHUP is M1b+, full workspace/limits config grows in later milestones — noted in scope)
- `bbolt` store, buckets, `schema_version` + forward migrations + downgrade guard → Task 6 ✓ (the six buckets from spec §16 are all created; record read/write APIs land with the domain in M1c)
- Node keypair + `node_id = hash(pubkey)` (ADR-0004) → Task 4 ✓
- Self-routing IDs (ADR-0002) → Task 5 ✓
- Health/readyz/metrics → Tasks 7–8 ✓
- **Deferred to later sub-plans (explicitly out of M1a):** `SandboxBackend` + SDK adapter/fake, gRPC+gateway+TLS multiplex, embedded SPA serving, sandbox CRUD/exec/agent-run/ports/files, operations + `Idempotency-Key`, event bus + SSE, auth (bearer + cookie). These are M1b–M1d.

**Placeholder scan:** No TBD/TODO; every code step has complete, compilable code and an exact run command with expected output.

**Type consistency:** `config.Config`/`Default`/`Load`/`Validate`; `identity.LoadOrCreate`→`*Identity{NodeID}`; `ids.NewGen`→`*Gen` with `Sandbox`/`Op`, `ids.Owner`; `store.Open`→`*Store` with `DB`/`Close`/`SchemaVersion`; `obs.NewLogger`/`NewHealth`→`*Health{Handler,SetReady}`/`RegisterBuildInfo`; `node.New(cfg, log, version)`→`*Node{NodeID,Addr,Start,Stop}`. The `node_test.go` and `main.go` call sites match these signatures.

**Note for the implementer:** if `go vet` flags the unused `metaBucket` var in `store.go` (Task 6), delete that one line — `bucketMeta` is the one used.
