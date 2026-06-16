package obs

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func TestMetrics_SandboxGauge(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.SetSandboxes("running", 3)

	// Assert the gauge value for the "running" label is 3.
	require.Equal(t, float64(3), testutil.ToFloat64(m.sandboxes.WithLabelValues("running")))
	// Assert only 1 series is present.
	require.Equal(t, 1, testutil.CollectAndCount(m.sandboxes))
}
