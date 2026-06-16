package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKV_PutGetDeleteForEach(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	require.NoError(t, s.Put("sandboxes", "a", []byte("1")))
	require.NoError(t, s.Put("sandboxes", "b", []byte("2")))

	v, ok, err := s.Get("sandboxes", "a")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []byte("1"), v)

	seen := map[string]string{}
	require.NoError(t, s.ForEach("sandboxes", func(k, v []byte) error {
		seen[string(k)] = string(v)
		return nil
	}))
	require.Equal(t, map[string]string{"a": "1", "b": "2"}, seen)

	require.NoError(t, s.Delete("sandboxes", "a"))
	_, ok, err = s.Get("sandboxes", "a")
	require.NoError(t, err)
	require.False(t, ok)
}
