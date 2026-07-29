package sandbox

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFake_ListTemplateInfo(t *testing.T) {
	got, err := NewFake().ListTemplateInfo(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, got)
	require.Equal(t, "fake/base", got[0].Repository)
}

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

func TestFake_ListTemplateInfoSplitsPortedHostFromTag(t *testing.T) {
	f := NewFake()
	require.NoError(t, f.SaveTemplate(context.Background(), "sb-1", "localhost:5000/img:1"))

	got, err := f.ListTemplateInfo(context.Background())
	require.NoError(t, err)
	require.Contains(t, got, TemplateInfo{Repository: "localhost:5000/img", Tag: "1"})
}

func TestFake_SaveAndRemoveTemplate(t *testing.T) {
	f := NewFake()
	require.NoError(t, f.SaveTemplate(context.Background(), "sb-1", "myimage:v1"))
	require.Equal(t, []string{"sb-1=>myimage:v1"}, f.SavedTemplates())

	// A saved template is listed, then removable.
	got, err := f.ListTemplates(context.Background())
	require.NoError(t, err)
	require.Contains(t, got, "myimage:v1")

	require.NoError(t, f.RemoveTemplate(context.Background(), "myimage:v1"))
	got, err = f.ListTemplates(context.Background())
	require.NoError(t, err)
	require.NotContains(t, got, "myimage:v1")
}

func TestFake_ExecInteractiveEchoes(t *testing.T) {
	sess, err := NewFake().ExecInteractive(context.Background(), "sb", []string{"/bin/sh"}, true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sess.Close() })

	go func() { _, _ = sess.Stdin().Write([]byte("ping")) }()
	buf := make([]byte, 4)
	_, err = io.ReadFull(sess.Stdout(), buf)
	require.NoError(t, err)
	require.Equal(t, "ping", string(buf))
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
