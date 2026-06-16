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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/squall-chua/sbx-swarm-node/internal/apiserver"
	"github.com/squall-chua/sbx-swarm-node/internal/auth"
	"github.com/squall-chua/sbx-swarm-node/internal/config"
	"github.com/squall-chua/sbx-swarm-node/internal/events"
	"github.com/squall-chua/sbx-swarm-node/internal/identity"
	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/obs"
	"github.com/squall-chua/sbx-swarm-node/internal/ops"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/squall-chua/sbx-swarm-node/internal/tlsutil"
	"google.golang.org/grpc"
)

// Node is a single standalone node.
type Node struct {
	cfg     *config.Config
	log     *slog.Logger
	id      *identity.Identity
	ids     *ids.Gen
	store   *store.Store
	health  *obs.Health
	srv     *http.Server
	grpcSrv *grpc.Server
	cert    tls.Certificate
	ln      net.Listener
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

	gen := ids.NewGen(id.NodeID)
	bus := events.NewBus(id.NodeID, 1024)
	backend := sandbox.NewFake() // M1c default backend so the node boots without a daemon
	mgr := sandbox.NewManager(id.NodeID, backend, st, gen)
	mgr.SetPublisher(bus)
	opsM := ops.NewManager(st, gen)
	opsM.SetPublisher(bus)
	sandboxes := apiserver.NewSandboxService(mgr, opsM)

	cert, err := tlsutil.LoadOrGenerate(cfg.TLSCertFile, cfg.TLSKeyFile, cfg.DataDir)
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("tls: %w", err)
	}
	signer := auth.NewSigner(id.PrivateKey.Seed()) // stable per-node session signing key

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
	})
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("apiserver: %w", err)
	}

	// Best-effort reconcile of persisted records against backend truth at boot.
	if err := mgr.Reconcile(context.Background()); err != nil {
		log.Warn("initial reconcile failed", "err", err)
	}

	return &Node{
		cfg:    cfg,
		log:    log,
		id:     id,
		ids:    gen,
		store:  st,
		health: health,
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

// Start binds the listener and serves the one-port TLS server in the background,
// then marks ready.
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
	return nil
}

// Stop gracefully shuts the HTTP server, stops the gRPC server, and closes the
// store.
func (n *Node) Stop(ctx context.Context) error {
	n.health.SetReady(false)
	err := n.srv.Shutdown(ctx)
	n.grpcSrv.Stop()
	if cerr := n.store.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}
