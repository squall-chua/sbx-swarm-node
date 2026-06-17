package sandbox

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveLimit(t *testing.T) {
	require.Equal(t, 5.0, resolveLimit(5, 99)) // explicit config wins
	require.Equal(t, 12.0, resolveLimit(0, 12)) // 0 -> detected
	require.Equal(t, 0.0, resolveLimit(0, 0))   // detection failed -> unlimited
}

func TestDetectHostLimits_PositiveOnThisHost(t *testing.T) {
	cpu, memKB, diskGB := detectHostLimits(".")
	require.Greater(t, cpu, 0.0)
	// memKB/diskGB are best-effort; the contract only guarantees ≥ 0
	// (0 == unknown → unlimited).
	require.GreaterOrEqual(t, memKB, 0.0)
	require.GreaterOrEqual(t, diskGB, 0.0)
}
