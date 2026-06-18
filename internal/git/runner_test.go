package git

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRunner_RunsAllowedStepsStopsOnError(t *testing.T) {
	r := NewRunner([]string{"echo", "false"})

	res, err := r.Run(context.Background(), t.TempDir(), nil, [][]string{{"echo", "hello"}})
	require.NoError(t, err)
	require.Len(t, res, 1)
	require.Equal(t, 0, res[0].ExitCode)

	res, err = r.Run(context.Background(), t.TempDir(), nil, [][]string{{"false"}, {"echo", "never"}})
	require.Error(t, err)  // stops at the failing step
	require.Len(t, res, 1) // second step never ran
}

func TestRunner_RejectsDisallowedBinary(t *testing.T) {
	r := NewRunner([]string{"git"})
	_, err := r.Run(context.Background(), t.TempDir(), nil, [][]string{{"rm", "-rf", "/"}})
	require.ErrorContains(t, err, "not allowed")
}
