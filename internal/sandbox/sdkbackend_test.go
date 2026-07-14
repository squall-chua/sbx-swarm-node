package sandbox

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScrubSecretValue(t *testing.T) {
	// The sbx CLI echoes the raw value in its error argv; it must be scrubbed.
	err := errors.New(`sbx [secret set-custom sb --env K --value sk-SECRET] failed`)
	got := scrubSecretValue(err, "sk-SECRET")
	require.NotContains(t, got.Error(), "sk-SECRET")
	require.Contains(t, got.Error(), "<redacted>")

	// Nothing to scrub → original error is preserved (unwrap chain intact).
	orig := errors.New("some other failure")
	require.Equal(t, orig, scrubSecretValue(orig, "sk-SECRET"))
	require.Equal(t, orig, scrubSecretValue(orig, ""))
	require.Nil(t, scrubSecretValue(nil, "sk-SECRET"))
}

// workspaceArg applies sbx's "primary workspace must be read/write" rule. These
// cases pin the behaviour discovered against the live daemon: the primary (first)
// workspace rejects ":ro"; secondary workspaces accept it.
func TestWorkspaceArg(t *testing.T) {
	tests := []struct {
		name                     string
		host                     string
		readOnly, primary, clone bool
		wantPath                 string
		wantErr                  bool
	}{
		{name: "primary RW", host: "/ws", primary: true, wantPath: "/ws"},
		{name: "primary RO non-clone rejected", host: "/ws", readOnly: true, primary: true, wantErr: true},
		{name: "primary RO clone drops :ro", host: "/ws", readOnly: true, primary: true, clone: true, wantPath: "/ws"},
		{name: "secondary RW", host: "/ws", wantPath: "/ws"},
		{name: "secondary RO appends :ro", host: "/ws", readOnly: true, wantPath: "/ws:ro"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := workspaceArg("ws", tc.host, tc.readOnly, tc.primary, tc.clone)
			if tc.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), "read-only")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantPath, got)
		})
	}
}

func TestDedupePorts(t *testing.T) {
	// The daemon lists IPv4 + IPv6 rows for one published port; once host_ip is
	// dropped they're identical and must collapse to a single row.
	in := []PublishedPort{
		{ContainerPort: 8080, HostPort: 54176}, // 127.0.0.1
		{ContainerPort: 8080, HostPort: 54176}, // ::1
		{ContainerPort: 9090, HostPort: 54177},
	}
	out := dedupePorts(in)
	require.Equal(t, []PublishedPort{
		{ContainerPort: 8080, HostPort: 54176},
		{ContainerPort: 9090, HostPort: 54177},
	}, out, "duplicate (container, host) pairs collapse; distinct mappings stay")
}
