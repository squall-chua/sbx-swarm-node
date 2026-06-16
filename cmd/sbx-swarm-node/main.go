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
