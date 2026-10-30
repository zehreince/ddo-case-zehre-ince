// Package server HTTP yönlendiricisini kurar. Modüller kendi rotalarını
// buraya bağlar; iş kuralı bu pakette yer almaz.
package server

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"carsihub/internal/health"
	"carsihub/internal/platform/config"
	"carsihub/internal/platform/httpx"
	"carsihub/internal/platform/metrics"
)

// Module kendi rotalarını /api/v1 altına bağlayan bir alan paketidir.
type Module interface {
	Mount(r chi.Router)
}

type Deps struct {
	Config  config.Config
	Log     *slog.Logger
	DB      health.Pinger
	Metrics *metrics.Metrics
	Modules []Module
}

func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	r.NotFound(httpx.NotFound)
	r.MethodNotAllowed(httpx.MethodNotAllowed)

	r.Use(
		httpx.RequestID,
		httpx.Recover(d.Log),
		httpx.AccessLog(d.Log),
		d.Metrics.Middleware,
		httpx.CORS(d.Config.CORSOrigins),
		httpx.Timeout(d.Config.RequestTimeout),
	)

	r.Get("/health", health.Handler(d.DB))
	r.Method(http.MethodGet, "/metrics", d.Metrics.Handler())

	r.Route("/api/v1", func(api chi.Router) {
		for _, m := range d.Modules {
			m.Mount(api)
		}
	})

	return r
}
