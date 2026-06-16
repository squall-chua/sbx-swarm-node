package obs

import "github.com/prometheus/client_golang/prometheus"

// Metrics holds the node's domain metrics.
type Metrics struct {
	sandboxes *prometheus.GaugeVec
	opsTotal  *prometheus.CounterVec
}

// NewMetrics registers and returns the domain metrics on the given registerer.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		sandboxes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sbx_sandboxes",
			Help: "Sandboxes by status.",
		}, []string{"status"}),
		opsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sbx_operations_total",
			Help: "Operations by type and final state.",
		}, []string{"type", "state"}),
	}
	reg.MustRegister(m.sandboxes, m.opsTotal)
	return m
}

// SetSandboxes sets the gauge for a given status label.
func (m *Metrics) SetSandboxes(status string, n int) {
	m.sandboxes.WithLabelValues(status).Set(float64(n))
}

// IncOp increments the operation counter for type and final state.
func (m *Metrics) IncOp(opType, state string) {
	m.opsTotal.WithLabelValues(opType, state).Inc()
}
