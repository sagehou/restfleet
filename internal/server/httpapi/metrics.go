package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	registry *prometheus.Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func NewMetrics() *Metrics {
	registry := prometheus.NewRegistry()
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "restfleet_http_requests_total",
		Help: "HTTP requests handled by route, method, and status class.",
	}, []string{"route", "method", "status_class"})
	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "restfleet_http_request_duration_seconds",
		Help:    "HTTP request duration by route and method.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route", "method"})
	registry.MustRegister(requests, duration)
	return &Metrics{registry: registry, requests: requests, duration: duration}
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *Metrics) observe(route, method string, status int, duration time.Duration) {
	statusClass := strconv.Itoa(status/100) + "xx"
	m.requests.WithLabelValues(route, method, statusClass).Inc()
	m.duration.WithLabelValues(route, method).Observe(duration.Seconds())
}
