// Package membership manages swarm membership: identity, gossip, and failure
// detection.
package membership

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
)

// Mode is how the node starts up relative to a swarm (spec §7).
type Mode int

const (
	ModeStandalone  Mode = iota // minted own id, no seeds
	ModePendingJoin             // seeds configured but no id yet; adopt on contact
	ModeRejoin                  // persisted id + seeds
)

// SwarmIdentity is the node's view of which swarm it belongs to.
type SwarmIdentity struct {
	SwarmID   string `json:"swarm_id"`
	SwarmName string `json:"swarm_name,omitempty"`
	Mode      Mode   `json:"-"`
}

// LoadOrInit loads a persisted swarm identity or initializes one per the
// startup rules: persisted id => rejoin; no id + seeds => pending-join (never
// mint); no id + no seeds => mint standalone.
func LoadOrInit(path string, seeds []string) (*SwarmIdentity, error) {
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		var si SwarmIdentity
		if err := json.Unmarshal(raw, &si); err != nil {
			return nil, err
		}
		si.Mode = ModeRejoin
		return &si, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	if len(seeds) > 0 {
		return &SwarmIdentity{Mode: ModePendingJoin}, nil // adopt on contact; do not persist yet
	}
	si := &SwarmIdentity{SwarmID: newSwarmID(), Mode: ModeStandalone}
	return si, persist(path, si)
}

// Adopt records the swarm id learned from seeds (pending-join → member).
func (si *SwarmIdentity) Adopt(path, swarmID, swarmName string) error {
	si.SwarmID, si.SwarmName = swarmID, swarmName
	return persist(path, si)
}

// GuardJoin refuses to merge with a peer presenting a different swarm id under
// the same secret (ADR-0001). An empty local id means pending-join (adopt).
func GuardJoin(localID, peerID string) error {
	if localID == "" || localID == peerID {
		return nil
	}
	return errors.New("refusing to join: peer swarm id differs from ours under the same cluster secret")
}

func newSwarmID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func persist(path string, si *SwarmIdentity) error {
	raw, err := json.Marshal(si)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}
