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
