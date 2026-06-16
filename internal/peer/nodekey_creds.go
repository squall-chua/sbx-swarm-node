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
