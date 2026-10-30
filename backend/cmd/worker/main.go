// Komut worker arka plan işlerini çalıştırır: süresi dolan rezervasyon ve
// siparişlerin süpürülmesi (ileride outbox yayımlayıcısı da burada).
//
//	worker              işçiyi başlatır; :WORKER_PORT'ta /health ve /metrics
//	worker healthcheck  konteyner sağlık kontrolü
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"carsihub/internal/inventory"
	"carsihub/internal/order"
	"carsihub/internal/platform/config"
	"carsihub/internal/platform/db"
	"carsihub/internal/platform/httpx"
	"carsihub/internal/platform/logx"
	"carsihub/internal/platform/metrics"
	"carsihub/internal/sweeper"
)

func main() {
	cfg, err := config.FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(cfg.WorkerPort))
	}
	log := logx.New(os.Stdout, cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL, 5)
	if err != nil {
		log.Error("veritabanına bağlanılamadı", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	policy, _ := inventory.ParsePolicy(cfg.PartialFulfillment)
	sw := sweeper.New(pool, order.NewService(pool, policy, cfg.PaymentTTL), cfg.SweepInterval, log)
	m := metrics.New()
	m.MustRegister(sw.Collectors()...)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		if !sw.Healthy() {
			httpx.JSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded"})
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.Handle("GET /metrics", m.Handler())
	srv := &http.Server{Addr: fmt.Sprintf(":%d", cfg.WorkerPort), Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("worker http sunucusu durdu", "error", err)
			stop()
		}
	}()

	log.Info("worker başladı", "sweep_interval", cfg.SweepInterval.String())
	sw.Run(ctx)

	log.Info("worker kapanıyor")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

func healthcheck(port int) int {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/health", port), nil)
	if err != nil {
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
