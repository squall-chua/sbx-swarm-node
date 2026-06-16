// Package peer maintains cached gRPC connections to other nodes.
package peer

import (
	"context"
	"net"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Pool caches one gRPC client connection per peer address.
type Pool struct {
	mu     sync.Mutex
	conns  map[string]*grpc.ClientConn
	dialer func(context.Context, string) (net.Conn, error)
	creds  credentials.TransportCredentials
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

// NewPool builds a connection pool.
func NewPool(opts ...Option) *Pool {
	p := &Pool{conns: map[string]*grpc.ClientConn{}}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Conn returns a cached connection to addr, dialing if needed.
func (p *Pool) Conn(addr string) (*grpc.ClientConn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.conns[addr]; ok {
		return c, nil
	}
	var dialOpts []grpc.DialOption
	target := addr
	if p.dialer != nil {
		dialOpts = append(dialOpts, grpc.WithContextDialer(p.dialer))
		// passthrough scheme skips DNS resolution when a custom dialer is set.
		target = "passthrough:///" + addr
	}
	if p.creds != nil {
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
