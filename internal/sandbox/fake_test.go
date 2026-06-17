package sandbox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFake_ListTemplates(t *testing.T) {
	f := NewFake()
	got, err := f.ListTemplates(context.Background())
	require.NoError(t, err)
	require.Empty(t, got)

	f.SetTemplates([]string{"base:1", "gpu:2"})
	got, err = f.ListTemplates(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"base:1", "gpu:2"}, got)
}

func TestFake_LifecycleAndExec(t *testing.T) {
	f := NewFake()
	ctx := context.Background()

	sb, err := f.Create(ctx, CreateSpec{Name: "s1", CPUs: 2, MemoryBytes: 1 << 30})
	require.NoError(t, err)
	require.Equal(t, "s1", sb.Name)
	require.Equal(t, "running", sb.Status)

	list, err := f.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)

	res, err := f.Exec(ctx, "s1", []string{"echo", "hi"}, ExecOpts{})
	require.NoError(t, err)
	require.Equal(t, 0, res.ExitCode)

	id, err := f.ExecDetached(ctx, "s1", []string{"sleep", "1"}, ExecOpts{})
	require.NoError(t, err)
	st, err := f.PollDetached(ctx, "s1", id)
	require.NoError(t, err)
	require.True(t, st.Done)

	require.NoError(t, f.Stop(ctx, "s1"))
	require.NoError(t, f.Remove(ctx, "s1"))
	_, err = f.Get(ctx, "s1")
	require.ErrorIs(t, err, ErrNotFound)
}
