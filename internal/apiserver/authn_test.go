package apiserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/auth"
	"github.com/squall-chua/sbx-swarm-node/internal/identity"
	"github.com/squall-chua/sbx-swarm-node/internal/nodekey"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestAuthenticate_SignedAuthzToken(t *testing.T) {
	signer := auth.NewSigner([]byte("k"))
	a := newAuthenticator(authnDeps{
		Signer: signer, Keys: keyMap{"adm": "admin"}, LocalNodeID: "self",
	})
	tok := signer.Mint("read-only", time.Now().Add(time.Minute))
	md := metadata.Pairs("x-sbx-authz", tok)
	ctx := metadata.NewIncomingContext(context.Background(), md)
	p, err := a.authenticate(ctx)
	require.NoError(t, err)
	require.Equal(t, "read-only", p.userRole)
}

func TestAuthenticate_BearerAPIKey(t *testing.T) {
	a := newAuthenticator(authnDeps{Signer: auth.NewSigner([]byte("k")), Keys: keyMap{"adm": "admin"}, LocalNodeID: "self"})
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer adm"))
	p, err := a.authenticate(ctx)
	require.NoError(t, err)
	require.Equal(t, "admin", p.userRole)
}

func TestAuthenticate_NodeKey(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	callerID := identity.DeriveNodeID(pub)
	a := newAuthenticator(authnDeps{
		Signer: auth.NewSigner([]byte("k")), Keys: keyMap{}, LocalNodeID: "self",
		PubKeyFor: func(id string) ([]byte, bool) {
			if id == callerID {
				return pub, true
			}
			return nil, false
		},
	})
	tok := nodekey.Sign(priv, callerID, "self", time.Now())
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(nodekey.MetadataKey, tok))
	p, err := a.authenticate(ctx)
	require.NoError(t, err)
	require.True(t, p.node)
	require.Empty(t, p.userRole)
}

func TestAuthenticate_NoCredsRejected(t *testing.T) {
	a := newAuthenticator(authnDeps{Signer: auth.NewSigner([]byte("k")), Keys: keyMap{}, LocalNodeID: "self"})
	_, err := a.authenticate(context.Background())
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}

func TestAuthenticate_CSRFTokenRejectedAsAuthz(t *testing.T) {
	signer := auth.NewSigner([]byte("k"))
	a := newAuthenticator(authnDeps{
		Signer: signer, Keys: keyMap{}, LocalNodeID: "self",
	})
	// Mint a CSRF-purpose token (role="csrf") and replay it as x-sbx-authz.
	csrfTok := signer.Mint("csrf", time.Now().Add(time.Minute))
	md := metadata.Pairs("x-sbx-authz", csrfTok)
	ctx := metadata.NewIncomingContext(context.Background(), md)
	_, err := a.authenticate(ctx)
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}
