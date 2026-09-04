package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	registry   *prometheus.Registry
	requests   *prometheus.CounterVec
	duration   *prometheus.HistogramVec
	heartbeats *prometheus.CounterVec
	agents     *prometheus.GaugeVec
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
	heartbeats := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "restfleet_agent_heartbeats_total",
		Help: "Agent heartbeat messages by bounded processing result.",
	}, []string{"result"})
	agents := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "restfleet_agents",
		Help: "Current active Agent identities by derived health.",
	}, []string{"health"})
	registry.MustRegister(requests, duration, heartbeats, agents)
	return &Metrics{
		registry: registry, requests: requests, duration: duration,
		heartbeats: heartbeats, agents: agents,
	}
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *Metrics) observe(route, method string, status int, duration time.Duration) {
	statusClass := strconv.Itoa(status/100) + "xx"
	m.requests.WithLabelValues(route, method, statusClass).Inc()
	m.duration.WithLabelValues(route, method).Observe(duration.Seconds())
}

func (m *Metrics) observeAgentHeartbeat(result string) {
	m.heartbeats.WithLabelValues(result).Inc()
}

func (m *Metrics) setAgentHealth(online, degraded, offline int) {
	m.agents.WithLabelValues("online").Set(float64(online))
	m.agents.WithLabelValues("degraded").Set(float64(degraded))
	m.agents.WithLabelValues("offline").Set(float64(offline))
}
