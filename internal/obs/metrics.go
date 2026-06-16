package obs

import "github.com/prometheus/client_golang/prometheus"

// Metrics holds the node's domain metrics.
type Metrics struct {
	sandboxes  *prometheus.GaugeVec
	actualUtil *prometheus.GaugeVec
	opsTotal   *prometheus.CounterVec
}

// NewMetrics registers and returns the domain metrics on the given registerer.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		sandboxes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sbx_sandboxes",
			Help: "Sandboxes by status.",
		}, []string{"status"}),
		actualUtil: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sbx_actual_util",
			Help: "Aggregate normalized actual utilization (0..1+) vs provision limit, by resource.",
		}, []string{"resource"}),
		opsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sbx_operations_total",
			Help: "Operations by type and final state.",
		}, []string{"type", "state"}),
	}
	reg.MustRegister(m.sandboxes, m.actualUtil, m.opsTotal)
	return m
}

// SetSandboxes sets the gauge for a given status label.
func (m *Metrics) SetSandboxes(status string, n int) {
	m.sandboxes.WithLabelValues(status).Set(float64(n))
}

// ResetSandboxes clears all sandbox-status series so a fresh snapshot does not
// leave stale series behind (the status ticker is the sole writer).
func (m *Metrics) ResetSandboxes() {
	m.sandboxes.Reset()
}

// SetActualUtil sets the aggregate normalized utilization gauges.
func (m *Metrics) SetActualUtil(cpu, mem float64) {
	m.actualUtil.WithLabelValues("cpu").Set(cpu)
	m.actualUtil.WithLabelValues("mem").Set(mem)
}

// IncOp increments the operation counter for type and final state.
func (m *Metrics) IncOp(opType, state string) {
	m.opsTotal.WithLabelValues(opType, state).Inc()
}
