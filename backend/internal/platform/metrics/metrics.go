// Package metrics Prometheus formatında HTTP metrikleri üretir.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"carsihub/internal/platform/httpx"
)

type Metrics struct {
	registry *prometheus.Registry
	duration *prometheus.HistogramVec
}

func New() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP isteklerinin süresi.",
		Buckets: []float64{.005, .01, .025, .05, .1, .15, .25, .5, 1, 2.5},
	}, []string{"method", "route", "status"})
	reg.MustRegister(duration)

	return &Metrics{registry: reg, duration: duration}
}

func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Middleware ham yol yerine chi rota desenini etiketler (/products/{id});
// aksi halde her ürün kimliği ayrı bir zaman serisi açardı.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &httpx.StatusRecorder{ResponseWriter: w, Status: http.StatusOK}
		next.ServeHTTP(rec, r)

		route := "unmatched"
		if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
			route = rc.RoutePattern()
		}
		m.duration.WithLabelValues(r.Method, route, strconv.Itoa(rec.Status)).
			Observe(time.Since(start).Seconds())
	})
}
