package api

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics bundles the Prometheus collectors exposed at /metrics.
type Metrics struct {
	Requests      *prometheus.CounterVec
	Duration      *prometheus.HistogramVec
	DamageApplied prometheus.Counter
}

// NewMetrics registers and returns the service metrics. Duration buckets are
// tuned around the 100ms p99 target.
func NewMetrics() *Metrics {
	return &Metrics{
		Requests: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "behemoth_http_requests_total",
			Help: "Total HTTP requests by route, method and status.",
		}, []string{"route", "method", "status"}),
		Duration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "behemoth_http_request_duration_seconds",
			Help:    "HTTP request latency by route.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 1},
		}, []string{"route"}),
		DamageApplied: promauto.NewCounter(prometheus.CounterOpts{
			Name: "behemoth_damage_applied_total",
			Help: "Cumulative damage applied across all bosses.",
		}),
	}
}
