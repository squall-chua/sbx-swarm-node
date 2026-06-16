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
