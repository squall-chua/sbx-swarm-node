// Package peer maintains cached gRPC connections to other nodes.
package peer

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"fmt"
	"net"
	"sync"

	"github.com/squall-chua/sbx-swarm-node/internal/tlsutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

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

// Option configures the Pool.
type Option func(*Pool)

// WithContextDialer overrides the dialer (tests use bufconn).
func WithContextDialer(d func(context.Context, string) (net.Conn, error)) Option {
	return func(p *Pool) { p.dialer = d }
}

// WithCreds sets transport credentials (TLS in production; node-key auth is
// added via a PerRPCCredentials / interceptor in a later step).
func WithCreds(c credentials.TransportCredentials) Option { return func(p *Pool) { p.creds = c } }

// WithNodeKey sets the local identity used to sign per-peer node-key tokens.
func WithNodeKey(callerID string, priv ed25519.PrivateKey) Option {
	return func(p *Pool) { p.callerID = callerID; p.priv = priv }
}

// WithPinResolver supplies the gossiped pubkey for a target node id (TLS pin).
func WithPinResolver(f func(nodeID string) ([]byte, bool)) Option {
	return func(p *Pool) { p.pinResolver = f }
}

// NewPool builds a connection pool.
func NewPool(opts ...Option) *Pool {
	p := &Pool{conns: map[string]*grpc.ClientConn{}}
	for _, o := range opts {
		o(p)
	}
	return p
}

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

// Close closes all connections.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = map[string]*grpc.ClientConn{}
}
