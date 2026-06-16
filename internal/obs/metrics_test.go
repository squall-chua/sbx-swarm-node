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

func TestMetrics_ResetSandboxesClearsStaleSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.SetSandboxes("running", 1)
	require.Equal(t, float64(1), testutil.ToFloat64(m.sandboxes.WithLabelValues("running")))

	// A subsequent poll with zero running sandboxes must clear the stale series.
	m.ResetSandboxes()
	require.Equal(t, 0, testutil.CollectAndCount(m.sandboxes))
	require.Equal(t, float64(0), testutil.ToFloat64(m.sandboxes.WithLabelValues("running")))
}

func TestMetrics_ActualUtilGauge(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.SetActualUtil(0.25, 0.5)

	require.Equal(t, 0.25, testutil.ToFloat64(m.actualUtil.WithLabelValues("cpu")))
	require.Equal(t, 0.5, testutil.ToFloat64(m.actualUtil.WithLabelValues("mem")))
}

func TestMetrics_IncOpCounter(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.IncOp("provision", "done")
	m.IncOp("provision", "done")

	require.Equal(t, float64(2), testutil.ToFloat64(m.opsTotal.WithLabelValues("provision", "done")))
}
