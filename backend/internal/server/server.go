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

type Deps struct {
	Config  config.Config
	Log     *slog.Logger
	DB      health.Pinger
	Metrics *metrics.Metrics
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
		// Modül rotaları sonraki adımlarda burada bağlanacak.
		_ = api
	})

	return r
}
