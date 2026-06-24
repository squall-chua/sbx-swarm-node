package sandbox

import (
	"testing"

	"github.com/stretchr/testify/require"
)

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
