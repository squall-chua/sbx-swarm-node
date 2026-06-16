// Package nodekey signs and verifies audience-bound Ed25519 tokens that
// authenticate one node to another (ADR-0004). A token binds the caller node id,
// the intended target node id (audience), and a timestamp, so it cannot be
// replayed to a third node or outside a short freshness window.
package nodekey

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/identity"
)

// MetadataKey is the gRPC metadata key carrying the token.
const MetadataKey = "x-sbx-node-auth"

func payload(callerID, targetID string, unix int64) string {
	return callerID + "|" + targetID + "|" + strconv.FormatInt(unix, 10)
}

// Sign returns a token of the form "<caller>.<target>.<unix>.<base64(sig)>".
func Sign(priv ed25519.PrivateKey, callerID, targetID string, now time.Time) string {
	unix := now.Unix()
	sig := ed25519.Sign(priv, []byte(payload(callerID, targetID, unix)))
	return callerID + "." + targetID + "." + strconv.FormatInt(unix, 10) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
}

// Verify parses and validates a token. pubkeyFor resolves a caller node id to its
// gossiped pubkey (the TOFU pin); denied (nil ok) reports revoked node ids.
func Verify(
	token, expectedTarget string,
	pubkeyFor func(nodeID string) ([]byte, bool),
	now time.Time,
	skew time.Duration,
	denied func(nodeID string) bool,
) (callerID string, err error) {
	parts := strings.Split(token, ".")
	if len(parts) != 4 {
		return "", errors.New("nodekey: malformed token")
	}
	callerID, targetID, unixStr, sigB64 := parts[0], parts[1], parts[2], parts[3]
	if targetID != expectedTarget {
		return "", fmt.Errorf("nodekey: wrong audience %q", targetID)
	}
	if denied != nil && denied(callerID) {
		return "", fmt.Errorf("nodekey: node %s is denied", callerID)
	}
	pub, ok := pubkeyFor(callerID)
	if !ok {
		return "", fmt.Errorf("nodekey: unknown peer %s", callerID)
	}
	if len(pub) != ed25519.PublicKeySize || identity.DeriveNodeID(pub) != callerID {
		return "", errors.New("nodekey: pubkey does not match node id")
	}
	unix, perr := strconv.ParseInt(unixStr, 10, 64)
	if perr != nil {
		return "", fmt.Errorf("nodekey: bad timestamp: %w", perr)
	}
	if d := now.Sub(time.Unix(unix, 0)); d > skew || d < -skew {
		return "", errors.New("nodekey: stale token")
	}
	sig, derr := base64.RawURLEncoding.DecodeString(sigB64)
	if derr != nil {
		return "", fmt.Errorf("nodekey: bad signature encoding: %w", derr)
	}
	if !ed25519.Verify(pub, []byte(payload(callerID, targetID, unix)), sig) {
		return "", errors.New("nodekey: signature verification failed")
	}
	return callerID, nil
}
