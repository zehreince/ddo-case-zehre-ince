// Package health GET /health ucunu sağlar.
package health

import (
	"context"
	"net/http"
	"time"

	"carsihub/internal/platform/httpx"
)

// Pinger *pgxpool.Pool tarafından karşılanır; testte sahtesi verilir.
type Pinger interface {
	Ping(ctx context.Context) error
}

type Response struct {
	Status   string `json:"status"`
	Database string `json:"database"`
}

func Handler(db Pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := db.Ping(ctx); err != nil {
			httpx.JSON(w, http.StatusServiceUnavailable, Response{Status: "degraded", Database: "unreachable"})
			return
		}
		httpx.JSON(w, http.StatusOK, Response{Status: "ok", Database: "ok"})
	}
}
