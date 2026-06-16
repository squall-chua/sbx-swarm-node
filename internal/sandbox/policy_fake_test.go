package sandbox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFake_PolicyAndSecrets(t *testing.T) {
	f := NewFake()
	ctx := context.Background()

	require.NoError(t, f.PolicyDeny(ctx, "", "evil.example"))
	rules, err := f.PolicyList(ctx, "")
	require.NoError(t, err)
	require.Len(t, rules, 1)
	require.Equal(t, "deny", rules[0].Decision)

	require.NoError(t, f.SecretSet(ctx, "s1", CustomSecret{Host: "api.x", Env: "TOKEN", Value: "shh"}))
	secs, err := f.SecretList(ctx, "s1")
	require.NoError(t, err)
	require.Len(t, secs.Custom, 1)
	require.Equal(t, "api.x", secs.Custom[0].Host)
	require.Empty(t, secs.Custom[0].Value) // backend never returns values
}
