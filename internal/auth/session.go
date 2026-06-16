// Package auth provides bearer + cookie-session authentication and role gating.
package auth

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Signer mints and verifies HMAC-signed session tokens of the form
// "<role>|<expiryUnix>|<sigBase64>".
type Signer struct{ key []byte }

// NewSigner returns a Signer over the given secret key.
func NewSigner(key []byte) *Signer { return &Signer{key: key} }

func (s *Signer) sign(payload string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Mint returns a signed token carrying role and expiry.
func (s *Signer) Mint(role string, expiry time.Time) string {
	payload := role + "|" + strconv.FormatInt(expiry.Unix(), 10)
	return payload + "|" + s.sign(payload)
}

// Verify checks the signature and expiry, returning the role.
func (s *Signer) Verify(token string) (string, error) {
	parts := strings.Split(token, "|")
	if len(parts) != 3 {
		return "", errors.New("malformed session token")
	}
	payload := parts[0] + "|" + parts[1]
	if !hmac.Equal([]byte(parts[2]), []byte(s.sign(payload))) {
		return "", errors.New("bad session signature")
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", fmt.Errorf("bad expiry: %w", err)
	}
	if time.Now().After(time.Unix(exp, 0)) {
		return "", errors.New("session expired")
	}
	return parts[0], nil
}

// DeriveSessionKey returns the HMAC key used to sign session/x-sbx-authz tokens.
// In a swarm it is derived from the cluster secret so a token minted by any node
// verifies on every node (ADR-0010); a standalone node (empty clusterSecret)
// uses its per-node seed.
func DeriveSessionKey(clusterSecret string, nodeSeed []byte) []byte {
	if clusterSecret == "" {
		return nodeSeed
	}
	key, err := hkdf.Key(sha256.New, []byte(clusterSecret), nil, "sbx-session-v1", 32)
	if err != nil {
		// HKDF only errors on absurd key lengths; 32 is always valid.
		panic("auth: hkdf derive session key: " + err.Error())
	}
	return key
}
