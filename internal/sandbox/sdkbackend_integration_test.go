//go:build integration

package sandbox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// Requires a running sandboxd + a compatible sbx daemon.
// Run: go test -tags integration ./internal/sandbox/
func TestSDKBackend_CreateExecRemove(t *testing.T) {
	ctx := context.Background()
	b, err := NewSDKBackend(ctx, func(string) (string, bool, bool) { return "", false, false })
	require.NoError(t, err)

	sb, err := b.Create(ctx, CreateSpec{Name: "it-" + t.Name(), CPUs: 1, MemoryBytes: 1 << 30})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Remove(ctx, sb.Name) })

	res, err := b.Exec(ctx, sb.Name, []string{"true"}, ExecOpts{})
	require.NoError(t, err)
	require.Equal(t, 0, res.ExitCode)
}
