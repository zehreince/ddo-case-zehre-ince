package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"carsihub/internal/platform/config"
	"carsihub/internal/platform/httpx"
	"carsihub/internal/platform/metrics"
)

type okPinger struct{}

func (okPinger) Ping(context.Context) error { return nil }

func newTestRouter() http.Handler {
	return NewRouter(Deps{
		Config:  config.Config{RequestTimeout: time.Second},
		Log:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
		DB:      okPinger{},
		Metrics: metrics.New(),
	})
}

func TestRouter_UnknownRouteUsesErrorShape(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/yok", nil))

	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"code":"`+httpx.CodeNotFound+`"`) {
		t.Fatalf("404 biçimi yanlış: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRouter_MetricsUseRoutePattern(t *testing.T) {
	h := newTestRouter()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `route="/health"`) {
		t.Fatalf("/metrics health rotasını göstermiyor: %d", rec.Code)
	}
}
