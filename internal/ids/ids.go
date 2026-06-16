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
