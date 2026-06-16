// Package obs provides logging, Prometheus metrics, and the health/readiness
// HTTP endpoints.
package obs

import (
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewLogger returns a JSON slog logger at the given level ("debug"|"info"|
// "warn"|"error", defaulting to info).
func NewLogger(level string, w io.Writer) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lv}))
}

// RegisterBuildInfo registers a constant gauge carrying the build version.
func RegisterBuildInfo(reg prometheus.Registerer, version string) {
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "sbx_build_info",
		Help:        "Build information; constant 1.",
		ConstLabels: prometheus.Labels{"version": version},
	})
	g.Set(1)
	reg.MustRegister(g)
}

// Health serves /healthz, /readyz, and /metrics.
type Health struct {
	ready atomic.Bool
	reg   *prometheus.Registry
}

// NewHealth builds a Health backed by the given registry.
func NewHealth(reg *prometheus.Registry) *Health {
	return &Health{reg: reg}
}

// SetReady marks the node ready (or not) for /readyz.
func (h *Health) SetReady(v bool) { h.ready.Store(v) }

// Handler returns the HTTP handler exposing the endpoints.
func (h *Health) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if h.ready.Load() {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ready")
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "not ready")
	})
	mux.Handle("/metrics", promhttp.HandlerFor(h.reg, promhttp.HandlerOpts{}))
	return mux
}
