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

// TestFake_SecretSetReplacesSameEnv pins the contract the real daemon enforces
// and this branch just fixed there: a custom secret is keyed on (scope, env),
// so a second SecretSet for the same env replaces the entry (host included)
// rather than appending a duplicate row, and the existing placeholder survives
// the replace. A different env still appends as a separate entry.
func TestFake_SecretSetReplacesSameEnv(t *testing.T) {
	f := NewFake()
	ctx := context.Background()

	require.NoError(t, f.SecretSet(ctx, "s1", CustomSecret{Host: "api.x", Env: "TOKEN", Placeholder: "ph-1", Value: "first"}))
	require.NoError(t, f.SecretSet(ctx, "s1", CustomSecret{Host: "api.y", Env: "TOKEN", Value: "second"}))

	// Reach into the unexported store directly: Fake.SecretList does not surface
	// Placeholder, and that field is exactly what this test needs to check.
	require.Len(t, f.secrets["s1"], 1, "second write for the same env must replace, not duplicate")
	require.Equal(t, "api.y", f.secrets["s1"][0].Host, "replace must take the new host")
	require.Equal(t, "ph-1", f.secrets["s1"][0].Placeholder, "replace must preserve the existing placeholder")

	require.NoError(t, f.SecretSet(ctx, "s1", CustomSecret{Host: "other.z", Env: "OTHER", Value: "third"}))
	require.Len(t, f.secrets["s1"], 2, "a different env must still append")
}
