// Package node wires the node's components — identity, store, observability,
// and the one-port TLS API server (gRPC + REST + static) — into a startable,
// stoppable node.
package node

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/squall-chua/sbx-swarm-node/internal/apiserver"
	"github.com/squall-chua/sbx-swarm-node/internal/audit"
	"github.com/squall-chua/sbx-swarm-node/internal/auth"
	"github.com/squall-chua/sbx-swarm-node/internal/config"
	"github.com/squall-chua/sbx-swarm-node/internal/events"
	"github.com/squall-chua/sbx-swarm-node/internal/identity"
	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/membership"
	"github.com/squall-chua/sbx-swarm-node/internal/obs"
	"github.com/squall-chua/sbx-swarm-node/internal/obsd"
	"github.com/squall-chua/sbx-swarm-node/internal/ops"
	"github.com/squall-chua/sbx-swarm-node/internal/peer"
	"github.com/squall-chua/sbx-swarm-node/internal/routing"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/squall-chua/sbx-swarm-node/internal/tlsutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Node is a single standalone node.
type Node struct {
	cfg     *config.Config
	log     *slog.Logger
	id      *identity.Identity
	ids     *ids.Gen
	store   *store.Store
	mgr     *sandbox.Manager
	health  *obs.Health
	srv     *http.Server
	grpcSrv *grpc.Server
	cert    tls.Certificate
	ln      net.Listener
	cancel  context.CancelFunc  // cancels background collector goroutines
	cluster *membership.Cluster // nil when not in cluster mode
	pool    *peer.Pool          // nil when not in cluster mode
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
	backend := sandbox.NewFake() // M1c default backend so the node boots without a daemon
	mgr := sandbox.NewManager(id.NodeID, backend, st, gen)
	mgr.SetPublisher(bus)
	opsM := ops.NewManager(st, gen)
	opsM.SetPublisher(bus)
	opsM.SetMetrics(metrics)
	sandboxes := apiserver.NewSandboxService(mgr, opsM)
	auditLog := audit.New(st, func() int64 { return time.Now().Unix() })
	policySvc := apiserver.NewPolicyService(mgr, auditLog)

	// Background observability collectors.
	nctx, cancel := context.WithCancel(context.Background())
	statsC := obsd.NewStatsCollector(backend, namesList(mgr), obsd.DefaultProvisionLimit(), 4)
	netC := obsd.NewNetLogCollector(backend, mgr.ResolveVMToID)
	sandboxes.WithObserve(apiserver.ObserveDeps{Stats: statsC, NetLog: netC, Backend: backend, Mgr: mgr})
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
			}
			metrics.ResetSandboxes()
			for status, n := range counts {
				metrics.SetSandboxes(status, n)
			}
		}
	})
	go runTicker(nctx, 15*time.Second, func() { _ = netC.PollOnce(nctx) })

	cert, err := tlsutil.LoadOrGenerate(cfg.TLSCertFile, cfg.TLSKeyFile, cfg.DataDir)
	if err != nil {
		cancel()
		_ = st.Close()
		return nil, fmt.Errorf("tls: %w", err)
	}
	signer := auth.NewSigner(id.PrivateKey.Seed()) // stable per-node session signing key

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

	// Peer TLS: use InsecureSkipVerify for v1. Nodes use self-signed certs;
	// trust is established via the shared cluster secret (encrypted gossip,
	// ADR-0004) rather than a CA. Node-key challenge auth (ADR-0004 §future)
	// will replace this in a later milestone.
	//
	// SECURITY NOTE: InsecureSkipVerify is intentional for v1. Flag for review.
	tlsCreds := credentials.NewTLS(&tls.Config{InsecureSkipVerify: true}) //nolint:gosec
	pool := peer.NewPool(peer.WithCreds(tlsCreds))
	fwd := apiserver.NewForwarder(tbl, pool)

	// Build the initial local NodeState from config + current sandbox list.
	swarmName := cfg.SwarmName
	if swarmName == "" {
		swarmName = si.SwarmName
	}
	ownedIDs := ownedSandboxIDs(context.Background(), mgr)
	localNS := membership.NodeState{
		NodeID:          id.NodeID,
		Addr:            dialableAddr(cfg.ListenAddr),
		ProtocolVersion: membership.ProtocolVersion,
		Capabilities:    []string{"clone", "stats", "exec"},
		OwnedSandboxIDs: ownedIDs,
		SwarmID:         si.SwarmID,
		SwarmName:       swarmName,
		Labels:          cfg.Labels,
		LimitCPU:        cfg.ProvisionLimits.CPUCores,
		LimitMemKB:      float64(cfg.ProvisionLimits.MemoryBytes / 1024),
	}

	// Build NodeService before cluster so we can wire the Cordoner below.
	nodeSvc := apiserver.NewNodeService(id.NodeID, cfg.NodeName, version)

	var clusterInstance *membership.Cluster
	if cfg.GossipAddr != "" && cfg.ClusterSecret != "" {
		// Only build the cluster when a cluster_secret is configured. A pure
		// standalone node (no secret, no seeds) skips gossip entirely.
		cl, clErr := membership.NewCluster(cfg, localNS, tbl, si, siPath,
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
		// Re-gossip OwnedSandboxIDs on create/delete so peer node-state stays
		// fresh (M5 scheduling reads gossiped owned-id sets).
		mgr.SetOwnedIDsNotifier(cl)
	}

	handler, grpcSrv, err := apiserver.Build(apiserver.Options{
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
		NodeSvc:   nodeSvc, // pre-wired with Cordoner (nil-safe if no cluster)
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

	return &Node{
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
