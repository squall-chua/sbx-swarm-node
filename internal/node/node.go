// Package node wires the node's components — identity, store, observability,
// and the one-port TLS API server (gRPC + REST + static) — into a startable,
// stoppable node.
package node

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/squall-chua/sbx-swarm-node/internal/apiserver"
	"github.com/squall-chua/sbx-swarm-node/internal/audit"
	"github.com/squall-chua/sbx-swarm-node/internal/auth"
	"github.com/squall-chua/sbx-swarm-node/internal/config"
	"github.com/squall-chua/sbx-swarm-node/internal/coordinator"
	"github.com/squall-chua/sbx-swarm-node/internal/events"
	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/git"
	"github.com/squall-chua/sbx-swarm-node/internal/identity"
	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/membership"
	"github.com/squall-chua/sbx-swarm-node/internal/obs"
	"github.com/squall-chua/sbx-swarm-node/internal/obsd"
	"github.com/squall-chua/sbx-swarm-node/internal/ops"
	"github.com/squall-chua/sbx-swarm-node/internal/peer"
	"github.com/squall-chua/sbx-swarm-node/internal/routing"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/squall-chua/sbx-swarm-node/internal/scheduler"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/squall-chua/sbx-swarm-node/internal/tlsutil"
	"google.golang.org/grpc"
)

// Node is a single standalone node.
type Node struct {
	cfg        *config.Config
	log        *slog.Logger
	id         *identity.Identity
	ids        *ids.Gen
	store      *store.Store
	mgr        *sandbox.Manager
	health     *obs.Health
	srv        *http.Server
	grpcSrv    *grpc.Server
	cert       tls.Certificate
	ln         net.Listener
	consoleSrv *http.Server // nil unless cfg.ConsoleAddr is set (browser-facing listener)
	consoleLn  net.Listener
	cancel     context.CancelFunc  // cancels background collector goroutines
	cluster    *membership.Cluster // nil when not in cluster mode
	pool       *peer.Pool          // nil when not in cluster mode
}

// New constructs a node: it establishes identity, opens the store, loads the TLS
// certificate, and builds the one-port API server, but does not listen yet.
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
	metrics := obs.NewMetrics(reg)

	gen := ids.NewGen(id.NodeID)
	bus := events.NewBus(id.NodeID, 1024)
	backend, err := buildBackend(cfg)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	mgr := sandbox.NewManager(id.NodeID, backend, st, gen)
	mgr.SetPublisher(bus)
	dc, dm, dd := sandbox.DetectHostLimits(cfg.DataDir)
	capt := sandbox.NewCapacity(
		resolveCfgLimit(cfg.ProvisionLimits.CPUCores, dc),
		resolveCfgLimit(float64(cfg.ProvisionLimits.MemoryBytes)/1024, dm),
		resolveCfgLimit(cfg.ProvisionLimits.DiskGB, dd),
	)
	mgr.SetCapacity(capt)
	opsM := ops.NewManager(st, gen)
	opsM.SetPublisher(bus)
	opsM.SetMetrics(metrics)
	sandboxes := apiserver.NewSandboxService(mgr, opsM)
	auditLog := audit.New(st, func() int64 { return time.Now().Unix() })
	gitWS := buildGitWorkspaces(cfg.Workspaces, cfg.DataDir)
	sandboxes.SetGit(gitWS)
	sandboxes.SetAudit(auditLog)
	sandboxes.SetEvents(bus)
	sandboxes.SetIdleTimeout(cfg.IdleTimeoutDuration())
	sandboxes.SetMaxUploadBytes(cfg.MaxUploadBytes)
	policySvc := apiserver.NewPolicyService(mgr, auditLog)

	// Background observability collectors.
	nctx, cancel := context.WithCancel(context.Background())
	var clusterInstance *membership.Cluster
	statsC := obsd.NewStatsCollector(backend, namesList(mgr), obsd.DefaultProvisionLimit(), 4)
	netC := obsd.NewNetLogCollector(backend, mgr.ResolveVMToID)
	sandboxes.WithObserve(apiserver.ObserveDeps{Stats: statsC, NetLog: netC, Backend: backend, Mgr: mgr})
	idleEnabled := cfg.IdleTimeoutDuration() > 0
	go runTicker(nctx, 10*time.Second, func() {
		_ = statsC.PollOnce(nctx)
		// Surface the spec §9 actual_util reconstruction on /metrics.
		au := statsC.ActualUtil()
		metrics.SetActualUtil(au.CPU, au.Mem)
		// Update the sandbox status gauge from manager records. Reset first so
		// statuses absent from this snapshot don't retain stale values.
		if recs, err := mgr.List(nctx); err == nil {
			counts := map[string]int{}
			for _, r := range recs {
				counts[r.Status]++
				if idleEnabled && r.Status == "running" {
					if u, ok := statsC.Latest(r.BackendName); ok && u.CPUPercent >= cpuActiveThreshold {
						_ = mgr.BumpActivity(nctx, r.ID) // observed work counts as Activity
					}
				}
			}
			metrics.ResetSandboxes()
			for status, n := range counts {
				metrics.SetSandboxes(status, n)
			}
		}
		_ = mgr.Reconcile(nctx)
		if clusterInstance != nil {
			rc, rm, rd := mgr.Capacity().Snapshot()
			clusterInstance.UpdateLocalLoad(rc, rm, rd, au.CPU, au.Mem)
		}
	})
	go runTicker(nctx, 15*time.Second, func() { _ = netC.PollOnce(nctx) })
	if idle := cfg.IdleTimeoutDuration(); idle > 0 {
		go runTicker(nctx, reapInterval(idle), func() { sandboxes.ReapIdle(nctx, time.Now()) })
	}

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
	signer := auth.NewSigner(auth.DeriveSessionKey(cfg.ClusterSecret, id.PrivateKey.Seed()))

	// --- M4 cluster wiring ---
	// Load (or initialise) the swarm identity. Standalone when no seeds are set.
	siPath := filepath.Join(cfg.DataDir, "swarm.json")
	si, err := membership.LoadOrInit(siPath, cfg.Join)
	if err != nil {
		cancel()
		_ = st.Close()
		return nil, fmt.Errorf("swarm identity: %w", err)
	}

	tbl := routing.NewTable(id.NodeID)

	pool := peer.NewPool(
		peer.WithNodeKey(id.NodeID, id.PrivateKey),
		peer.WithPinResolver(func(nodeID string) ([]byte, bool) { return tbl.PubKey(nodeID) }),
	)
	fwd := apiserver.NewForwarder(tbl, pool, id.NodeID)

	// Build the initial local NodeState from config + current sandbox list.
	swarmName := cfg.SwarmName
	if swarmName == "" {
		swarmName = si.SwarmName
	}
	ownedIDs := ownedSandboxIDs(context.Background(), mgr)
	lc, lm, ld := capt.Limits()
	ac, am, ad := capt.Snapshot()
	tmpls, _ := backend.ListTemplates(context.Background())
	localNS := membership.NodeState{
		NodeID:          id.NodeID,
		Addr:            dialableAddr(cfg.ListenAddr),
		ProtocolVersion: membership.ProtocolVersion,
		Capabilities:    []string{"clone", "stats", "exec", "write-files"},
		OwnedSandboxIDs: ownedIDs,
		SwarmID:         si.SwarmID,
		SwarmName:       swarmName,
		Labels:          cfg.Labels,
		LimitCPU:        lc,
		LimitMemKB:      lm,
		LimitDiskGB:     ld,
		AllocCPU:        ac,
		AllocMemKB:      am,
		AllocDiskGB:     ad,
		Workspaces:      workspaceNames(cfg.Workspaces),
		GitWorkspaces:   gitWorkspaceNames(cfg.Workspaces),
		Templates:       tmpls,
		PubKey:          id.PublicKey,
	}

	// Build NodeService before cluster so we can wire the Cordoner below.
	nodeSvc := apiserver.NewNodeService(id.NodeID, cfg.NodeName, version)

	if cfg.GossipAddr != "" && cfg.ClusterSecret != "" {
		// Only build the cluster when a cluster_secret is configured. A pure
		// standalone node (no secret, no seeds) skips gossip entirely.
		cl, clErr := membership.NewCluster(cfg, localNS, tbl, si, siPath, st,
			func(deadNodeID string) {
				mgr.MarkUnreachable(deadNodeID)
			},
			log,
		)
		if clErr != nil {
			cancel()
			_ = st.Close()
			pool.Close()
			return nil, fmt.Errorf("membership cluster: %w", clErr)
		}
		clusterInstance = cl
		nodeSvc.SetCordoner(cl)
		nodeSvc.SetRevoker(cl)
		// Re-gossip OwnedSandboxIDs on create/delete so peer node-state stays
		// fresh (M5 scheduling reads gossiped owned-id sets).
		mgr.SetOwnedIDsNotifier(cl)
	}

	nodeSvc.SetTemplateLister(mgr.Backend().ListTemplateInfo)
	nodeSvc.SetNodeLister(func() []apiserver.NodeRow {
		// Self row: live capacity + current templates + drain/cordon state.
		lc, lm, ld := capt.Limits()
		ac, am, ad := capt.Snapshot()
		tmpls, _ := mgr.Backend().ListTemplates(context.Background())
		self := apiserver.NodeRow{
			NodeID: id.NodeID, NodeName: cfg.NodeName, Draining: nodeSvc.Draining(),
			Cordoned:      clusterInstance != nil && clusterInstance.LocalNodeState().Cordoned,
			Labels:        cfg.Labels,
			Capabilities:  []string{"clone", "stats", "exec", "write-files"},
			Workspaces:    workspaceNames(cfg.Workspaces),
			GitWorkspaces: gitWorkspaceNames(cfg.Workspaces),
			Templates:     tmpls,
			LimitCPU:      lc, LimitMemKB: lm, LimitDiskGB: ld,
			AllocCPU: ac, AllocMemKB: am, AllocDiskGB: ad,
		}
		if clusterInstance != nil { // self actual util (gossiped), same source as buildCandidates
			ls := clusterInstance.LocalNodeState()
			self.ActualCPU, self.ActualMem = ls.ActualCPU, ls.ActualMem
		} else { // standalone (no gossip): read util straight from the collector
			au := statsC.ActualUtil()
			self.ActualCPU, self.ActualMem = au.CPU, au.Mem
		}
		rows := []apiserver.NodeRow{self}
		if clusterInstance != nil {
			for _, ns := range clusterInstance.PeerStates() {
				rows = append(rows, rowFromState(ns))
			}
		}
		return rows
	})

	coord := coordinator.New(func() []scheduler.Candidate {
		return buildCandidates(id.NodeID, cfg, capt, mgr, clusterInstance, tbl)
	})
	sandboxes.WithPlacement(
		func(ctx context.Context, req scheduler.Request, spec *sbxv1.CreateSandboxRequest) (string, error) {
			req.Local = id.NodeID // prefer this (entry) node on a score tie
			return coord.Provision(ctx, req, attemptFor(id.NodeID, spec, req.RequestID, mgr, gitWS, tbl, pool, log))
		},
		cfg.DefaultStrategy,
		sandbox.Resources{
			CPUCores:    cfg.DefaultSandboxResources.CPUCores,
			MemoryBytes: cfg.DefaultSandboxResources.MemoryBytes,
			DiskGB:      cfg.DefaultSandboxResources.DiskGB,
		},
	)

	handler, consoleHandler, grpcSrv, err := apiserver.Build(apiserver.Options{
		NodeID:    id.NodeID,
		NodeName:  cfg.NodeName,
		Version:   version,
		Keys:      cfg,
		Signer:    signer,
		Cert:      cert,
		Health:    health,
		Sandboxes: sandboxes,
		Events:    bus,
		Policy:    policySvc,
		Forward:   fwd,
		Routing:   tbl,
		Peers:     pool,
		Internal:  apiserver.NewInternalService(mgr, gitWS, func() bool { return tbl.IsCordoned(id.NodeID) }),
		NodeSvc:   nodeSvc, // pre-wired with Cordoner (nil-safe if no cluster)
		Pins: func(nodeID string) (crypto.PublicKey, bool) {
			pk, ok := tbl.PubKey(nodeID)
			if !ok {
				return nil, false
			}
			return ed25519.PublicKey(pk), true
		},
		PubKeyFor: func(nodeID string) ([]byte, bool) { return tbl.PubKey(nodeID) },
		Denylist:  func(nodeID string) bool { return clusterInstance != nil && clusterInstance.IsRevoked(nodeID) },
	})
	if err != nil {
		cancel()
		_ = st.Close()
		pool.Close()
		if clusterInstance != nil {
			_ = clusterInstance.Shutdown()
		}
		return nil, fmt.Errorf("apiserver: %w", err)
	}

	// Best-effort reconcile of persisted records against backend truth at boot.
	if err := mgr.Reconcile(context.Background()); err != nil {
		log.Warn("initial reconcile failed", "err", err)
	}

	// Unstick operations left non-terminal by a previous crash (ops crash-recovery).
	if n, rerr := opsM.RecoverInterrupted(); rerr != nil {
		log.Warn("op recovery failed", "err", rerr)
	} else if n > 0 {
		log.Info("recovered interrupted operations", "count", n)
	}

	nodeInst := &Node{
		cfg:     cfg,
		log:     log,
		id:      id,
		ids:     gen,
		store:   st,
		mgr:     mgr,
		health:  health,
		cancel:  cancel,
		cluster: clusterInstance,
		pool:    pool,
		srv: &http.Server{
			Handler:   handler,
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", "http/1.1"}},
		},
		grpcSrv: grpcSrv,
		cert:    cert,
	}

	// Optional browser-facing console listener: serves the SPA + REST (no gRPC
	// surface) over a browser-compatible cert (operator-provided, else a
	// self-signed ECDSA P-256 cert persisted under DataDir/console). The main
	// ListenAddr keeps its pinned Ed25519 identity cert (browsers reject it).
	if cfg.ConsoleAddr != "" {
		nodeInst.consoleSrv = &http.Server{Handler: consoleHandler}
		if cfg.ConsoleTLS {
			consoleCert, cerr := tlsutil.LoadOrGenerate(cfg.ConsoleTLSCertFile, cfg.ConsoleTLSKeyFile, filepath.Join(cfg.DataDir, "console"))
			if cerr != nil {
				cancel()
				_ = st.Close()
				pool.Close()
				if clusterInstance != nil {
					_ = clusterInstance.Shutdown()
				}
				return nil, fmt.Errorf("console tls: %w", cerr)
			}
			nodeInst.consoleSrv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{consoleCert}, NextProtos: []string{"h2", "http/1.1"}}
		} else {
			log.Warn("console serving over plain HTTP (cleartext) — front with a TLS proxy or keep on a trusted network", "addr", cfg.ConsoleAddr)
		}
	}

	return nodeInst, nil
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

// ConsoleAddr returns the browser console listen address (valid after Start);
// empty when no console listener is configured.
func (n *Node) ConsoleAddr() string {
	if n.consoleLn == nil {
		return n.cfg.ConsoleAddr
	}
	return n.consoleLn.Addr().String()
}

// Cluster returns the membership.Cluster (nil in standalone mode).
func (n *Node) Cluster() *membership.Cluster { return n.cluster }

// Start binds the listener and serves the one-port TLS server in the background,
// then marks ready. If seeds are configured it also initiates a non-blocking
// join in the background (retried once; startup modes handle the rest).
func (n *Node) Start() error {
	ln, err := net.Listen("tcp", n.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", n.cfg.ListenAddr, err)
	}
	n.ln = ln
	go func() {
		if err := n.srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			n.log.Error("http server stopped", "err", err)
		}
	}()
	if n.consoleSrv != nil {
		cln, err := net.Listen("tcp", n.cfg.ConsoleAddr)
		if err != nil {
			return fmt.Errorf("listen console %s: %w", n.cfg.ConsoleAddr, err)
		}
		n.consoleLn = cln
		go func() {
			var err error
			if n.consoleSrv.TLSConfig != nil {
				err = n.consoleSrv.ServeTLS(cln, "", "")
			} else {
				err = n.consoleSrv.Serve(cln)
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				n.log.Error("console server stopped", "err", err)
			}
		}()
		n.log.Info("console serving", "addr", n.ConsoleAddr())
	}

	n.health.SetReady(true)
	n.log.Info("node serving", "addr", n.Addr())

	// Background join: non-blocking. If seeds are set and the cluster is up,
	// attempt to join. The pending-join → member transition happens inside
	// MergeRemoteState (Adopt) when the first bulk push/pull round-trip completes.
	if n.cluster != nil && len(n.cfg.Join) > 0 {
		go func() {
			if _, err := n.cluster.Join(n.cfg.Join); err != nil {
				n.log.Warn("cluster join failed (will rely on gossip re-contact)", "err", err)
			}
		}()
	}

	return nil
}

// Stop gracefully shuts the HTTP server, stops the gRPC server, closes the
// cluster membership, and closes the store.
func (n *Node) Stop(ctx context.Context) error {
	n.health.SetReady(false)
	if n.cancel != nil {
		n.cancel() // stop background collector goroutines
	}

	// Leave the cluster before closing the API so peers learn we're gone.
	if n.cluster != nil {
		_ = n.cluster.Leave(3 * time.Second)
		_ = n.cluster.Shutdown()
	}
	if n.pool != nil {
		n.pool.Close()
	}

	if n.consoleSrv != nil {
		_ = n.consoleSrv.Shutdown(ctx)
	}
	err := n.srv.Shutdown(ctx)
	n.grpcSrv.Stop()
	if cerr := n.store.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}

// namesList returns a function that lists backend names of running sandboxes
// only (exec.Stats requires a running sandbox).
func namesList(mgr *sandbox.Manager) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		recs, err := mgr.List(ctx)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(recs))
		for _, r := range recs {
			if r.Status != "running" {
				continue
			}
			names = append(names, r.BackendName)
		}
		return names, nil
	}
}

// ownedSandboxIDs returns the IDs of all sandboxes currently known to the manager.
func ownedSandboxIDs(ctx context.Context, mgr *sandbox.Manager) []string {
	recs, err := mgr.List(ctx)
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, r.ID)
	}
	return ids
}

// dialableAddr converts a listen address like ":8443" to "127.0.0.1:8443" so
// peers can dial it. In production, set ListenAddr to a concrete host. For
// integration tests (loopback), this default is correct.
func dialableAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return "127.0.0.1:" + port
	}
	return addr
}

// reapInterval is the reaper sweep cadence: min(timeout, 1m) — minute-scale for
// real timeouts, responsive for small ones.
// ponytail: a fixed 1m feels broken for a 10s test timeout; a separate
// sweep-interval knob is YAGNI.
func reapInterval(timeout time.Duration) time.Duration {
	if timeout < time.Minute {
		return timeout
	}
	return time.Minute
}

// cpuActiveThreshold: per-sandbox CPU% at/above which observed work counts as
// Activity (ADR-0016). ponytail: 5% counts as "doing work"; tune if barely-busy
// sandboxes get reaped. Dynamic only on the real backend (fake reports a fixed 10%).
const cpuActiveThreshold = 5.0

// runTicker calls fn on every interval tick until ctx is done.
func runTicker(ctx context.Context, interval time.Duration, fn func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			fn()
		case <-ctx.Done():
			return
		}
	}
}

func resolveCfgLimit(configured, detected float64) float64 {
	if configured > 0 {
		return configured
	}
	return detected
}

// effectiveGitBase resolves a workspace's host-side base repo path: the operator-
// set host_path, or (for a provider workspace with a remote_url and no host_path)
// a node-managed mirror base under the data dir (ADR-0020).
func effectiveGitBase(w config.WorkspaceConfig, dataDir string) string {
	if w.HostPath != "" {
		return w.HostPath
	}
	if w.Git != nil && w.Git.RemoteURL != "" {
		root := os.Getenv("SBX_GIT_WORKSPACE_DIR")
		if root == "" {
			root = filepath.Join(dataDir, "git-workspaces")
		}
		base := filepath.Join(root, w.Name+".git")
		// Absolute so the base resolves the same regardless of the child git's cwd
		// (EnsureBase clones from one dir; PreLock/Publish run with cwd=Base). A
		// relative base doubled the path between them. Abs only fails on a broken
		// cwd; fall back to the relative path in that impossible case.
		if abs, err := filepath.Abs(base); err == nil {
			return abs
		}
		return base
	}
	return w.HostPath // "" — legacy non-provider workspace with no host_path
}

func buildGitWorkspaces(ws []config.WorkspaceConfig, dataDir string) map[string]*git.Workspace {
	out := map[string]*git.Workspace{}
	for _, w := range ws {
		if w.Git == nil {
			continue
		}
		g := w.Git.WithDefaults()
		var token string
		if g.TokenEnv != "" {
			token = os.Getenv(g.TokenEnv)
		}
		out[w.Name] = git.New(git.Spec{
			Name: w.Name, Base: effectiveGitBase(w, dataDir), Remote: g.Remote,
			RemoteURL: g.RemoteURL, Provider: g.Provider,
			Cred: git.Credential{
				Token: token, SSHKeyPath: g.SSHKeyPath,
				SSHKnownHostsPath: g.SSHKnownHostsPath, CAPath: g.CAPath,
			},
			DefaultBranch: g.DefaultBranch,
			AllowPush:     g.AllowPush, APIBaseURL: g.APIBaseURL, PreSteps: g.PreSteps, PublishSteps: g.PublishSteps, Allowlist: g.ExecAllowlist,
		})
	}
	return out
}

// buildBackend selects the sandbox backend from config. "sdk" connects to the
// local sbx daemon (auto-starting it, version-checked); a connect failure fails
// boot rather than silently falling back to the fake. Default/"fake" boots
// without a daemon (tests, daemonless nodes).
func buildBackend(cfg *config.Config) (sandbox.Backend, error) {
	if cfg.Backend == "sdk" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return sandbox.NewSDKBackend(ctx, workspaceResolver(cfg.Workspaces, cfg.DataDir))
	}
	return sandbox.NewFake(), nil
}

// workspaceResolver maps a workspace name to its host path + read-only flag for
// the SDK backend. Git-backed workspaces are always read-only — the bare base
// must never be agent-writable (ADR-0015).
func workspaceResolver(ws []config.WorkspaceConfig, dataDir string) sandbox.WorkspaceResolver {
	type entry struct {
		path     string
		readOnly bool
	}
	m := make(map[string]entry, len(ws))
	for _, w := range ws {
		m[w.Name] = entry{path: effectiveGitBase(w, dataDir), readOnly: w.ReadOnly || w.Git != nil}
	}
	return func(name string) (string, bool, bool) {
		e, ok := m[name]
		return e.path, e.readOnly, ok
	}
}

func workspaceNames(ws []config.WorkspaceConfig) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Name)
	}
	return out
}

// gitWorkspaceNames returns the names of the git-backed workspaces (those whose
// config has a git block, ADR-0015). Subset of workspaceNames.
func gitWorkspaceNames(ws []config.WorkspaceConfig) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		if w.Git != nil {
			out = append(out, w.Name)
		}
	}
	return out
}

func nameSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

// rowFromState maps a gossiped NodeState to a NodeRow (peer view: no name/draining).
func rowFromState(ns membership.NodeState) apiserver.NodeRow {
	return apiserver.NodeRow{
		NodeID: ns.NodeID, Cordoned: ns.Cordoned, Labels: ns.Labels,
		Capabilities: ns.Capabilities, Workspaces: ns.Workspaces, GitWorkspaces: ns.GitWorkspaces, Templates: ns.Templates,
		LimitCPU: ns.LimitCPU, LimitMemKB: ns.LimitMemKB, LimitDiskGB: ns.LimitDiskGB,
		AllocCPU: ns.AllocCPU, AllocMemKB: ns.AllocMemKB, AllocDiskGB: ns.AllocDiskGB,
		ActualCPU: ns.ActualCPU, ActualMem: ns.ActualMem,
	}
}

// buildCandidates assembles the self candidate (live local capacity) + gossiped peers.
func buildCandidates(self string, cfg *config.Config, capt *sandbox.Capacity, mgr *sandbox.Manager, cl *membership.Cluster, tbl *routing.Table) []scheduler.Candidate {
	lc, lm, ld := capt.Limits()
	ac, am, ad := capt.Snapshot()
	recs, _ := mgr.List(context.Background())
	selfTmpls, _ := mgr.Backend().ListTemplates(context.Background())
	var selfUtilCPU, selfUtilMem float64
	if cl != nil {
		ls := cl.LocalNodeState()
		selfUtilCPU, selfUtilMem = ls.ActualCPU, ls.ActualMem
	}
	out := []scheduler.Candidate{{
		NodeID:       self,
		Workspaces:   nameSet(workspaceNames(cfg.Workspaces)),
		Templates:    nameSet(selfTmpls),
		Capabilities: map[string]bool{"clone": true, "stats": true, "exec": true},
		Labels:       cfg.Labels,
		LimitCPU:     lc, LimitMem: lm, LimitDisk: ld,
		AllocCPU: ac, AllocMem: am, AllocDisk: ad,
		Sandboxes: len(recs),
		ActualCPU: selfUtilCPU, ActualMem: selfUtilMem,
		Cordoned: tbl.IsCordoned(self),
	}}
	if cl == nil {
		return out
	}
	for _, ns := range cl.PeerStates() {
		out = append(out, scheduler.Candidate{
			NodeID:       ns.NodeID,
			Workspaces:   nameSet(ns.Workspaces),
			Templates:    nameSet(ns.Templates),
			Capabilities: nameSet(ns.Capabilities),
			Labels:       ns.Labels,
			LimitCPU:     ns.LimitCPU, LimitMem: ns.LimitMemKB, LimitDisk: ns.LimitDiskGB,
			AllocCPU: ns.AllocCPU, AllocMem: ns.AllocMemKB, AllocDisk: ns.AllocDiskGB,
			Sandboxes: len(ns.OwnedSandboxIDs),
			ActualCPU: ns.ActualCPU, ActualMem: ns.ActualMem,
			Cordoned: ns.Cordoned,
		})
	}
	return out
}

// callProvisionWithRetry sends a Provision and, on a post-dial RPC error, retries
// the SAME target once — safe because the target dedups by request_id, so a lost
// response will not create a duplicate. It NEVER falls through to another
// candidate after an ambiguous error (a different node does not share the dedup
// map). With no request_id it does not retry (can't dedup -> duplicate risk).
func callProvisionWithRetry(ctx context.Context, client sbxv1.InternalServiceClient, msg *sbxv1.ProvisionRequest) (*sbxv1.ProvisionReply, error) {
	reply, err := client.Provision(ctx, msg)
	if err != nil && msg.RequestId != "" {
		reply, err = client.Provision(ctx, msg)
	}
	return reply, err
}

// attemptFor builds the per-request attempt closure: local admit+create, or a
// remote Provision RPC over the pinned peer pool.
func attemptFor(self string, spec *sbxv1.CreateSandboxRequest, requestID string, mgr *sandbox.Manager, gitWS map[string]*git.Workspace, tbl *routing.Table, pool *peer.Pool, log *slog.Logger) coordinator.AttemptFunc {
	return func(ctx context.Context, nodeID string) (string, error) {
		if nodeID == self {
			rec, err := apiserver.ProvisionLocal(ctx, mgr, gitWS, apiserver.ToSpecForProvision(spec))
			if err == sandbox.ErrNoCapacity {
				return "", coordinator.ErrNack
			}
			if err != nil {
				return "", err
			}
			return rec.ID, nil
		}
		addr, ok := tbl.Addr(nodeID)
		if !ok {
			return "", coordinator.ErrNack
		}
		conn, err := pool.Conn(addr, nodeID)
		if err != nil {
			// Can't reach this peer (e.g. pin not yet gossiped): NACK so the
			// coordinator tries the next candidate instead of aborting placement.
			log.Warn("provision: peer unreachable, skipping", "node_id", nodeID, "err", err)
			return "", coordinator.ErrNack
		}
		reply, err := callProvisionWithRetry(ctx, sbxv1.NewInternalServiceClient(conn),
			&sbxv1.ProvisionRequest{Spec: spec, RequestId: requestID})
		if err != nil {
			return "", err
		}
		if !reply.Accepted {
			return "", coordinator.ErrNack
		}
		return reply.SandboxId, nil
	}
}
